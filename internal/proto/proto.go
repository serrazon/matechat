// Package proto implements the matechat wire protocol.
//
// Frame format:
//
//	[4-byte BE uint32: payload length][1-byte frame type][payload]
//
// Frame types:
//
//	0x01 — Text (JSON control + chat messages)
//	0x02 — Binary (file chunk: transferID[16] + chunkIdx[4] + chunkCount[4] + data)
//	0x03 — Relay (opaque forwarded bytes: sessionID[16] + encrypted payload)
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

const (
	FrameText   byte = 0x01
	FrameBinary byte = 0x02
	FrameRelay  byte = 0x03

	MaxFrameSize = 4 * 1024 * 1024 // 4 MB
)

// ReadFrame reads a single frame from r.
// Returns the frame type and the payload (without type byte).
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	payloadLen := binary.BigEndian.Uint32(lenBuf[:])
	if payloadLen < 1 {
		return 0, nil, fmt.Errorf("frame too small: %d", payloadLen)
	}
	if payloadLen > MaxFrameSize {
		return 0, nil, fmt.Errorf("frame too large: %d bytes (max %d)", payloadLen, MaxFrameSize)
	}

	buf := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}

	frameType := buf[0]
	payload := buf[1:]
	return frameType, payload, nil
}

// writeFrame writes a complete frame: [length][type][payload].
// If mu is non-nil it is held for the entire write.
func writeFrame(w io.Writer, mu *sync.Mutex, frameType byte, payload []byte) error {
	totalLen := uint32(1 + len(payload)) // type byte + payload
	if totalLen > MaxFrameSize {
		return fmt.Errorf("frame too large: %d bytes", totalLen)
	}

	var header [5]byte
	binary.BigEndian.PutUint32(header[:4], totalLen)
	header[4] = frameType

	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}

	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

// WriteTextFrame marshals v to JSON and writes a FrameText frame.
func WriteTextFrame(w io.Writer, mu *sync.Mutex, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return writeFrame(w, mu, FrameText, data)
}

// WriteBinaryFrame writes a FrameBinary frame for a file chunk.
func WriteBinaryFrame(w io.Writer, mu *sync.Mutex, transferID [16]byte, chunkIdx, chunkCount uint32, data []byte) error {
	// header: transferID(16) + chunkIdx(4) + chunkCount(4) = 24 bytes
	buf := make([]byte, 24+len(data))
	copy(buf[:16], transferID[:])
	binary.BigEndian.PutUint32(buf[16:20], chunkIdx)
	binary.BigEndian.PutUint32(buf[20:24], chunkCount)
	copy(buf[24:], data)
	return writeFrame(w, mu, FrameBinary, buf)
}

// WriteRelayFrame writes a FrameRelay frame for broker relay.
func WriteRelayFrame(w io.Writer, mu *sync.Mutex, sessionID [16]byte, payload []byte) error {
	buf := make([]byte, 16+len(payload))
	copy(buf[:16], sessionID[:])
	copy(buf[16:], payload)
	return writeFrame(w, mu, FrameRelay, buf)
}

// ParseBinaryFrame extracts fields from a FrameBinary payload.
func ParseBinaryFrame(payload []byte) (transferID [16]byte, chunkIdx, chunkCount uint32, data []byte, err error) {
	if len(payload) < 24 {
		return transferID, 0, 0, nil, fmt.Errorf("binary frame too short: %d bytes", len(payload))
	}
	copy(transferID[:], payload[:16])
	chunkIdx = binary.BigEndian.Uint32(payload[16:20])
	chunkCount = binary.BigEndian.Uint32(payload[20:24])
	data = payload[24:]
	return
}

// ParseRelayFrame extracts sessionID and opaque payload from a FrameRelay payload.
func ParseRelayFrame(payload []byte) (sessionID [16]byte, data []byte, err error) {
	if len(payload) < 16 {
		return sessionID, nil, fmt.Errorf("relay frame too short: %d bytes", len(payload))
	}
	copy(sessionID[:], payload[:16])
	data = payload[16:]
	return
}

// ParseEnvelope extracts just the "type" field from a JSON payload.
func ParseEnvelope(payload []byte) (string, error) {
	var env Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return "", err
	}
	return env.Type, nil
}
