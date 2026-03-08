package proto

import (
	"bytes"
	"testing"
)

func TestTextFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	msg := ChatMsg{Type: "msg", From: "mom", Body: "hello", TS: 1234567890}

	if err := WriteTextFrame(&buf, nil, msg); err != nil {
		t.Fatalf("WriteTextFrame: %v", err)
	}

	ft, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameText {
		t.Fatalf("expected FrameText, got %x", ft)
	}

	typ, err := ParseEnvelope(payload)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if typ != "msg" {
		t.Fatalf("expected type=msg, got %q", typ)
	}
}

func TestBinaryFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	tid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	data := []byte("chunk data here")

	if err := WriteBinaryFrame(&buf, nil, tid, 2, 10, data); err != nil {
		t.Fatalf("WriteBinaryFrame: %v", err)
	}

	ft, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameBinary {
		t.Fatalf("expected FrameBinary, got %x", ft)
	}

	gotTID, gotIdx, gotCount, gotData, err := ParseBinaryFrame(payload)
	if err != nil {
		t.Fatalf("ParseBinaryFrame: %v", err)
	}
	if gotTID != tid {
		t.Fatalf("transferID mismatch")
	}
	if gotIdx != 2 || gotCount != 10 {
		t.Fatalf("chunk idx/count mismatch: %d/%d", gotIdx, gotCount)
	}
	if !bytes.Equal(gotData, data) {
		t.Fatalf("data mismatch")
	}
}

func TestRelayFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	sid := [16]byte{0xAA, 0xBB}
	payload := []byte("opaque tls record")

	if err := WriteRelayFrame(&buf, nil, sid, payload); err != nil {
		t.Fatalf("WriteRelayFrame: %v", err)
	}

	ft, raw, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameRelay {
		t.Fatalf("expected FrameRelay, got %x", ft)
	}

	gotSID, gotData, err := ParseRelayFrame(raw)
	if err != nil {
		t.Fatalf("ParseRelayFrame: %v", err)
	}
	if gotSID != sid {
		t.Fatalf("sessionID mismatch")
	}
	if !bytes.Equal(gotData, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestMultipleFrames(t *testing.T) {
	var buf bytes.Buffer

	msg1 := HelloMsg{Type: "hello", From: "alice"}
	msg2 := ChatMsg{Type: "msg", From: "bob", Body: "hi", TS: 999}

	WriteTextFrame(&buf, nil, msg1)
	WriteTextFrame(&buf, nil, msg2)

	ft1, p1, err := ReadFrame(&buf)
	if err != nil || ft1 != FrameText {
		t.Fatalf("frame 1: type=%x err=%v", ft1, err)
	}
	typ1, _ := ParseEnvelope(p1)
	if typ1 != "hello" {
		t.Fatalf("expected hello, got %s", typ1)
	}

	ft2, p2, err := ReadFrame(&buf)
	if err != nil || ft2 != FrameText {
		t.Fatalf("frame 2: type=%x err=%v", ft2, err)
	}
	typ2, _ := ParseEnvelope(p2)
	if typ2 != "msg" {
		t.Fatalf("expected msg, got %s", typ2)
	}
}
