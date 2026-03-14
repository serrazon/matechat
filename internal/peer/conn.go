// Package peer manages P2P connections between matechat clients.
package peer

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"matechat/internal/proto"
	"matechat/internal/store"
)

// Conn represents a single P2P connection to one peer.
type Conn struct {
	PeerName string
	ConnType string // "direct", "holepunch", "relay"
	tlsConn  *tls.Conn
	wmu      sync.Mutex
	store    *store.Store

	onMessage      func(proto.ChatMsg)
	onLeave        func(name string)
	onFileReceived func(from, filename, localPath string)

	closed chan struct{}
	once   sync.Once

	// File transfer reassembly
	transfers map[string]*incomingTransfer
	tmu       sync.Mutex
}

type incomingTransfer struct {
	Meta   proto.UploadStartMsg
	Chunks map[uint32][]byte
}

func newConn(peerName, connType string, tlsConn *tls.Conn, st *store.Store,
	onMessage func(proto.ChatMsg), onLeave func(string),
	onFileReceived func(from, filename, localPath string)) *Conn {
	return &Conn{
		PeerName:       peerName,
		ConnType:       connType,
		tlsConn:        tlsConn,
		store:          st,
		onMessage:      onMessage,
		onLeave:        onLeave,
		onFileReceived: onFileReceived,
		closed:         make(chan struct{}),
		transfers:      make(map[string]*incomingTransfer),
	}
}

// readLoop reads frames from the peer and dispatches them.
// It runs until the connection closes or errors.
func (c *Conn) readLoop() {
	defer c.Close()

	for {
		frameType, payload, err := proto.ReadFrame(c.tlsConn)
		if err != nil {
			break
		}

		switch frameType {
		case proto.FrameText:
			c.handleTextFrame(payload)
		case proto.FrameBinary:
			c.handleBinaryFrame(payload)
		default:
			log.Printf("peer %s: unexpected frame type %x", c.PeerName, frameType)
		}
	}
}

func (c *Conn) handleTextFrame(payload []byte) {
	msgType, err := proto.ParseEnvelope(payload)
	if err != nil {
		log.Printf("peer %s: parse envelope: %v", c.PeerName, err)
		return
	}

	switch msgType {
	case "hello":
		// Peer announced itself; initiate history sync
		c.requestSync()

	case "msg":
		var msg proto.ChatMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			log.Printf("peer %s: unmarshal msg: %v", c.PeerName, err)
			return
		}
		// Store locally
		c.store.InsertMessage(msg.From, msg.Body, msg.TS)
		// Notify TUI
		if c.onMessage != nil {
			c.onMessage(msg)
		}

	case "sync_req":
		var msg proto.SyncReqMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			return
		}
		c.handleSyncReq(msg)

	case "sync":
		var msg proto.SyncMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			return
		}
		c.handleSync(msg)

	case "upload_start":
		var msg proto.UploadStartMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			return
		}
		c.tmu.Lock()
		c.transfers[msg.TransferID] = &incomingTransfer{
			Meta:   msg,
			Chunks: make(map[uint32][]byte),
		}
		c.tmu.Unlock()

	case "leave":
		if c.onLeave != nil {
			c.onLeave(c.PeerName)
		}
		c.Close()

	default:
		log.Printf("peer %s: unknown message type %q", c.PeerName, msgType)
	}
}

func (c *Conn) handleBinaryFrame(payload []byte) {
	transferID, chunkIdx, chunkCount, data, err := proto.ParseBinaryFrame(payload)
	if err != nil {
		log.Printf("peer %s: parse binary frame: %v", c.PeerName, err)
		return
	}

	tid := hex.EncodeToString(transferID[:])

	c.tmu.Lock()
	t, ok := c.transfers[tid]
	if !ok {
		c.tmu.Unlock()
		log.Printf("peer %s: binary chunk for unknown transfer", c.PeerName)
		return
	}

	chunk := make([]byte, len(data))
	copy(chunk, data)
	t.Chunks[chunkIdx] = chunk

	if uint32(len(t.Chunks)) != chunkCount {
		c.tmu.Unlock()
		return
	}

	// All chunks received — extract and release lock before doing I/O.
	meta := t.Meta
	chunks := t.Chunks
	delete(c.transfers, tid)
	c.tmu.Unlock()

	go c.finishTransfer(meta, chunks, chunkCount)
}

func (c *Conn) finishTransfer(meta proto.UploadStartMsg, chunks map[uint32][]byte, chunkCount uint32) {
	// Reassemble in order.
	var buf bytes.Buffer
	for i := uint32(0); i < chunkCount; i++ {
		buf.Write(chunks[i])
	}

	// Sanitize filename to prevent path traversal.
	safeName := filepath.Base(meta.Filename)
	if safeName == "." || safeName == string(filepath.Separator) {
		safeName = "file"
	}

	// Resolve ~/Downloads, creating it if absent.
	home, _ := os.UserHomeDir()
	downloadsDir := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloadsDir, 0755); err != nil {
		log.Printf("peer %s: create Downloads dir: %v", c.PeerName, err)
		return
	}

	// Avoid overwriting an existing file by appending " (N)" before the extension.
	outPath := filepath.Join(downloadsDir, safeName)
	if _, err := os.Stat(outPath); err == nil {
		ext := filepath.Ext(safeName)
		base := strings.TrimSuffix(safeName, ext)
		for n := 1; ; n++ {
			outPath = filepath.Join(downloadsDir, fmt.Sprintf("%s (%d)%s", base, n, ext))
			if _, err := os.Stat(outPath); os.IsNotExist(err) {
				break
			}
		}
	}

	if err := os.WriteFile(outPath, buf.Bytes(), 0644); err != nil {
		log.Printf("peer %s: write received file: %v", c.PeerName, err)
		return
	}

	c.store.InsertTransfer(meta.TransferID, safeName, meta.Size, outPath, time.Now().UnixMilli())
	log.Printf("peer %s: received %s → %s", c.PeerName, safeName, outPath)

	if c.onFileReceived != nil {
		c.onFileReceived(c.PeerName, safeName, outPath)
	}
}

func (c *Conn) requestSync() {
	ts, _ := c.store.LatestTS()
	proto.WriteTextFrame(c.tlsConn, &c.wmu, proto.SyncReqMsg{
		Type:    "sync_req",
		SinceTS: ts,
	})
}

func (c *Conn) handleSyncReq(msg proto.SyncReqMsg) {
	msgs, err := c.store.MessagesSince(msg.SinceTS, 500)
	if err != nil {
		log.Printf("peer %s: query messages for sync: %v", c.PeerName, err)
		return
	}

	chatMsgs := make([]proto.ChatMsg, len(msgs))
	for i, m := range msgs {
		chatMsgs[i] = proto.ChatMsg{
			Type: "msg",
			From: m.From,
			Body: m.Body,
			TS:   m.TS,
		}
	}

	proto.WriteTextFrame(c.tlsConn, &c.wmu, proto.SyncMsg{
		Type:     "sync",
		Messages: chatMsgs,
	})
}

func (c *Conn) handleSync(msg proto.SyncMsg) {
	for _, m := range msg.Messages {
		c.store.InsertMessage(m.From, m.Body, m.TS)
		if c.onMessage != nil {
			c.onMessage(m)
		}
	}
}

// SendMsg sends a chat message to this peer.
func (c *Conn) SendMsg(from, body string) error {
	msg := proto.ChatMsg{
		Type: "msg",
		From: from,
		Body: body,
		TS:   time.Now().UnixMilli(),
	}
	return proto.WriteTextFrame(c.tlsConn, &c.wmu, msg)
}

// SendHello announces this client to the peer.
func (c *Conn) SendHello(from string) error {
	return proto.WriteTextFrame(c.tlsConn, &c.wmu, proto.HelloMsg{
		Type: "hello",
		From: from,
	})
}

// Leave sends a leave message and closes the connection.
func (c *Conn) Leave(from string) {
	proto.WriteTextFrame(c.tlsConn, &c.wmu, proto.LeaveMsg{
		Type: "leave",
		From: from,
	})
	c.Close()
}

// Close closes the underlying TLS connection.
func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.closed)
		c.tlsConn.Close()
	})
}

// Done returns a channel that is closed when the connection closes.
func (c *Conn) Done() <-chan struct{} {
	return c.closed
}
