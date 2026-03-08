package peer

import (
	"encoding/hex"
	"io"
	"net"
	"sync"
	"time"

	"matechat/internal/proto"
)

// relayConn implements net.Conn over the broker relay path.
// The broker forwards opaque frames between two peers identified by sessionID.
// A TLS session is established ON TOP of this relay conn, so the broker
// only sees ciphertext.
type relayConn struct {
	sessionID [16]byte
	broker    net.Conn
	brokerWmu *sync.Mutex

	// readCh is fed by the broker read loop when relay frames arrive.
	readCh  chan []byte
	readBuf []byte // leftover bytes from a partially consumed chunk

	closed    chan struct{}
	closeOnce sync.Once
}

func newRelayConn(sessionID [16]byte, broker net.Conn, brokerWmu *sync.Mutex) *relayConn {
	return &relayConn{
		sessionID: sessionID,
		broker:    broker,
		brokerWmu: brokerWmu,
		readCh:    make(chan []byte, 64),
		closed:    make(chan struct{}),
	}
}

func (rc *relayConn) Read(b []byte) (int, error) {
	// Drain any leftover bytes from previous read first
	if len(rc.readBuf) > 0 {
		n := copy(b, rc.readBuf)
		rc.readBuf = rc.readBuf[n:]
		return n, nil
	}

	select {
	case data, ok := <-rc.readCh:
		if !ok {
			return 0, io.EOF
		}
		n := copy(b, data)
		if n < len(data) {
			rc.readBuf = make([]byte, len(data)-n)
			copy(rc.readBuf, data[n:])
		}
		return n, nil
	case <-rc.closed:
		return 0, io.EOF
	}
}

func (rc *relayConn) Write(b []byte) (int, error) {
	select {
	case <-rc.closed:
		return 0, net.ErrClosed
	default:
	}
	if err := proto.WriteRelayFrame(rc.broker, rc.brokerWmu, rc.sessionID, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (rc *relayConn) Close() error {
	rc.closeOnce.Do(func() {
		close(rc.closed)
	})
	return nil
}

// feed is called by the broker read loop to deliver incoming relay data.
func (rc *relayConn) feed(data []byte) {
	chunk := make([]byte, len(data))
	copy(chunk, data)
	select {
	case rc.readCh <- chunk:
	case <-rc.closed:
	}
}

// net.Conn boilerplate — addr and deadline methods
func (rc *relayConn) LocalAddr() net.Addr              { return relayAddr("local") }
func (rc *relayConn) RemoteAddr() net.Addr             { return relayAddr("remote") }
func (rc *relayConn) SetDeadline(t time.Time) error    { return nil }
func (rc *relayConn) SetReadDeadline(t time.Time) error  { return nil }
func (rc *relayConn) SetWriteDeadline(t time.Time) error { return nil }

// sessionHex returns the hex-encoded session ID (matches broker's format).
func (rc *relayConn) sessionHex() string {
	return hex.EncodeToString(rc.sessionID[:])
}

type relayAddr string

func (a relayAddr) Network() string { return "relay" }
func (a relayAddr) String() string  { return string(a) }
