// Package broker implements the matechat signaling server.
//
// The broker has three responsibilities:
//  1. Directory — maintains a live registry of online peers
//  2. Signaling — coordinates NAT hole-punch attempts
//  3. Relay fallback — forwards opaque encrypted bytes when direct connection fails
//
// The broker never reads or stores chat messages.
package broker

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"

	"matechat/internal/certs"
	"matechat/internal/proto"
)

// PeerEntry represents a connected client in the directory.
type PeerEntry struct {
	Name       string
	ListenAddr string // self-reported by client
	PublicAddr string // observed from conn.RemoteAddr()
	conn       *tls.Conn
	wmu        sync.Mutex // serialize writes to this connection
}

// RelaySession tracks a relay tunnel between two peers.
type RelaySession struct {
	ID string // hex-encoded session ID
	A  *PeerEntry
	B  *PeerEntry
}

// Broker is the matechat signaling server.
type Broker struct {
	tlsConfig *tls.Config
	revokedCN map[string]bool // loaded once at startup

	mu      sync.RWMutex
	dir     map[string]*PeerEntry    // CN → entry
	relays  map[string]*RelaySession // session_id → session
	pending map[string]string        // pending relay: target CN → initiator CN
}

// New creates a new Broker with the given TLS configuration.
func New(tlsCfg *tls.Config, revokedNames map[string]bool) *Broker {
	if revokedNames == nil {
		revokedNames = make(map[string]bool)
	}
	return &Broker{
		tlsConfig: tlsCfg,
		revokedCN: revokedNames,
		dir:       make(map[string]*PeerEntry),
		relays:    make(map[string]*RelaySession),
		pending:   make(map[string]string),
	}
}

// ListenAndServe starts the broker on the given address.
func (b *Broker) ListenAndServe(addr string) error {
	ln, err := tls.Listen("tcp", addr, b.tlsConfig)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()
	log.Printf("broker listening on %s", ln.Addr())

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go b.handleConn(conn.(*tls.Conn))
	}
}

func (b *Broker) handleConn(conn *tls.Conn) {
	defer conn.Close()

	if err := conn.Handshake(); err != nil {
		log.Printf("handshake: %v", err)
		return
	}

	name, err := certs.PeerName(conn)
	if err != nil {
		log.Printf("peer name: %v", err)
		return
	}

	if b.revokedCN[name] {
		log.Printf("rejected revoked peer: %s", name)
		return
	}

	entry := &PeerEntry{
		Name:       name,
		PublicAddr: conn.RemoteAddr().String(),
		conn:       conn,
	}

	log.Printf("peer connected: %s from %s", name, entry.PublicAddr)

	// Read loop
	for {
		frameType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			break
		}

		switch frameType {
		case proto.FrameText:
			b.handleTextFrame(entry, payload)
		case proto.FrameRelay:
			b.handleRelayFrame(entry, payload)
		default:
			log.Printf("unexpected frame type %x from %s", frameType, name)
		}
	}

	b.removePeer(name)
	log.Printf("peer disconnected: %s", name)
}

func (b *Broker) handleTextFrame(entry *PeerEntry, payload []byte) {
	msgType, err := proto.ParseEnvelope(payload)
	if err != nil {
		log.Printf("parse envelope from %s: %v", entry.Name, err)
		return
	}

	switch msgType {
	case "register":
		var msg proto.RegisterMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			log.Printf("unmarshal register: %v", err)
			return
		}
		b.handleRegister(entry, msg)

	case "peers_req":
		b.handlePeersReq(entry)

	case "punch_req":
		var msg proto.PunchReqMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			return
		}
		b.handlePunchReq(entry, msg)

	case "relay_req":
		var msg proto.RelayReqMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			return
		}
		b.handleRelayReq(entry, msg)

	case "relay_accept":
		var msg proto.RelayAcceptMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			return
		}
		b.handleRelayAccept(entry, msg)

	case "leave":
		b.removePeer(entry.Name)

	default:
		log.Printf("unknown message type %q from %s", msgType, entry.Name)
	}
}

func (b *Broker) handleRegister(entry *PeerEntry, msg proto.RegisterMsg) {
	entry.ListenAddr = msg.ListenAddr

	b.mu.Lock()
	// Remove stale entry if same name reconnects
	if old, ok := b.dir[entry.Name]; ok && old.conn != entry.conn {
		old.conn.Close()
	}
	b.dir[entry.Name] = entry
	b.mu.Unlock()

	// Send confirmation
	b.sendTo(entry, proto.RegisteredMsg{Type: "registered"})

	// Send current peer list
	b.handlePeersReq(entry)

	// Notify all other peers about updated peer list
	b.broadcastPeerList(entry.Name)
}

func (b *Broker) handlePeersReq(entry *PeerEntry) {
	b.mu.RLock()
	peers := make([]proto.PeerInfo, 0, len(b.dir))
	for name, pe := range b.dir {
		if name == entry.Name {
			continue
		}
		addr := resolveAddr(pe.ListenAddr, pe.PublicAddr)
		peers = append(peers, proto.PeerInfo{Name: name, Addr: addr})
	}
	b.mu.RUnlock()

	b.sendTo(entry, proto.PeersMsg{Type: "peers", Peers: peers})
}

// resolveAddr combines the self-reported listen address with the broker-observed
// public address. If the client registered with ":9100" (no host), we prepend
// the IP from the observed public address so peers get a dialable "1.2.3.4:9100".
func resolveAddr(listenAddr, publicAddr string) string {
	if listenAddr == "" {
		return publicAddr
	}
	if listenAddr[0] == ':' {
		host, _, err := net.SplitHostPort(publicAddr)
		if err == nil && host != "" {
			return net.JoinHostPort(host, listenAddr[1:])
		}
	}
	return listenAddr
}

func (b *Broker) handlePunchReq(entry *PeerEntry, msg proto.PunchReqMsg) {
	b.mu.RLock()
	target, ok := b.dir[msg.Target]
	b.mu.RUnlock()

	if !ok {
		log.Printf("punch_req from %s: target %s not found", entry.Name, msg.Target)
		return
	}

	// Notify target about the punch request with initiator's public address
	b.sendTo(target, proto.PunchNotifyMsg{
		Type:      "punch_notify",
		Initiator: entry.Name,
		Addr:      entry.PublicAddr,
	})

	// Notify initiator with target's public address
	b.sendTo(entry, proto.PunchNotifyMsg{
		Type:   "punch_notify",
		Target: msg.Target,
		Addr:   target.PublicAddr,
	})
}

func (b *Broker) handleRelayReq(entry *PeerEntry, msg proto.RelayReqMsg) {
	b.mu.RLock()
	target, ok := b.dir[msg.Target]
	b.mu.RUnlock()

	if !ok {
		log.Printf("relay_req from %s: target %s not found", entry.Name, msg.Target)
		return
	}

	b.mu.Lock()
	b.pending[msg.Target] = entry.Name
	b.mu.Unlock()

	b.sendTo(target, proto.RelayNotifyMsg{
		Type: "relay_notify",
		Peer: entry.Name,
	})
}

func (b *Broker) handleRelayAccept(entry *PeerEntry, msg proto.RelayAcceptMsg) {
	b.mu.Lock()
	initiatorName, ok := b.pending[entry.Name]
	if !ok || initiatorName != msg.Peer {
		b.mu.Unlock()
		log.Printf("relay_accept from %s: no matching pending relay for peer %s", entry.Name, msg.Peer)
		return
	}
	delete(b.pending, entry.Name)

	initiator, ok := b.dir[initiatorName]
	if !ok {
		b.mu.Unlock()
		log.Printf("relay_accept: initiator %s no longer connected", initiatorName)
		return
	}

	sessionID := generateSessionID()
	session := &RelaySession{
		ID: sessionID,
		A:  initiator,
		B:  entry,
	}
	b.relays[sessionID] = session
	b.mu.Unlock()

	// Notify both sides
	b.sendTo(initiator, proto.RelayReadyMsg{
		Type:      "relay_ready",
		Target:    entry.Name,
		SessionID: sessionID,
	})
	b.sendTo(entry, proto.RelayReadyMsg{
		Type:      "relay_ready",
		Target:    initiatorName,
		SessionID: sessionID,
	})

	log.Printf("relay session %s established: %s <-> %s", sessionID, initiatorName, entry.Name)
}

func (b *Broker) handleRelayFrame(entry *PeerEntry, payload []byte) {
	sessionID, data, err := proto.ParseRelayFrame(payload)
	if err != nil {
		log.Printf("parse relay frame from %s: %v", entry.Name, err)
		return
	}

	sid := fmt.Sprintf("%x", sessionID)

	b.mu.RLock()
	session, ok := b.relays[sid]
	b.mu.RUnlock()

	if !ok {
		log.Printf("relay frame from %s: unknown session %s", entry.Name, sid)
		return
	}

	// Forward to the other side
	var target *PeerEntry
	if session.A.Name == entry.Name {
		target = session.B
	} else if session.B.Name == entry.Name {
		target = session.A
	} else {
		log.Printf("relay frame from %s: not part of session %s", entry.Name, sid)
		return
	}

	if err := proto.WriteRelayFrame(target.conn, &target.wmu, sessionID, data); err != nil {
		log.Printf("relay forward to %s: %v", target.Name, err)
	}
}

func (b *Broker) removePeer(name string) {
	b.mu.Lock()

	// Remove from directory
	if entry, ok := b.dir[name]; ok {
		entry.conn.Close()
		delete(b.dir, name)
	}

	// Clean up any relay sessions involving this peer
	for sid, session := range b.relays {
		if session.A.Name == name || session.B.Name == name {
			delete(b.relays, sid)
			log.Printf("relay session %s terminated (peer %s left)", sid, name)
		}
	}

	// Clean up pending relay requests
	delete(b.pending, name)
	for target, initiator := range b.pending {
		if initiator == name {
			delete(b.pending, target)
		}
	}

	b.mu.Unlock()

	// Notify remaining peers
	b.broadcastPeerList("")
}

func (b *Broker) broadcastPeerList(excludeName string) {
	b.mu.RLock()
	entries := make([]*PeerEntry, 0, len(b.dir))
	for _, e := range b.dir {
		entries = append(entries, e)
	}
	b.mu.RUnlock()

	for _, e := range entries {
		if e.Name == excludeName {
			continue
		}
		b.handlePeersReq(e)
	}
}

func (b *Broker) sendTo(entry *PeerEntry, msg any) {
	if err := proto.WriteTextFrame(entry.conn, &entry.wmu, msg); err != nil {
		log.Printf("send to %s: %v", entry.Name, err)
	}
}

func generateSessionID() string {
	var buf [16]byte
	rand.Read(buf[:])
	return fmt.Sprintf("%x", buf)
}
