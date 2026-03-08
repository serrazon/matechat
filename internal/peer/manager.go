package peer

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"matechat/internal/certs"
	"matechat/internal/proto"
	"matechat/internal/store"
)

// Manager manages all peer connections and the broker connection.
type Manager struct {
	selfName   string
	listenAddr string

	clientTLS *tls.Config // for dialing broker + peers
	serverTLS *tls.Config // for accepting peer connections

	store *store.Store

	// Broker connection
	broker    *tls.Conn
	brokerMu  sync.Mutex
	brokerWmu sync.Mutex

	// Connected peers
	peers map[string]*Conn
	pmu   sync.RWMutex

	// Callbacks to TUI
	onMessage   func(proto.ChatMsg)
	onPeerJoin  func(name string)
	onPeerLeave func(name string)

	// Signaling channels from broker read loop
	peersCh     chan proto.PeersMsg
	punchCh     chan proto.PunchNotifyMsg
	relayReadyCh chan proto.RelayReadyMsg

	// Listener for incoming peer connections
	listener net.Listener

	// Active relay connections keyed by hex session ID
	relayConns map[string]*relayConn
	relayMu    sync.Mutex
}

// NewManager creates a peer Manager.
func NewManager(selfName, listenAddr string,
	clientTLS, serverTLS *tls.Config,
	st *store.Store,
	onMessage func(proto.ChatMsg),
	onPeerJoin, onPeerLeave func(string),
) *Manager {
	return &Manager{
		selfName:     selfName,
		listenAddr:   listenAddr,
		clientTLS:    clientTLS,
		serverTLS:    serverTLS,
		store:        st,
		peers:        make(map[string]*Conn),
		onMessage:    onMessage,
		onPeerJoin:   onPeerJoin,
		onPeerLeave:  onPeerLeave,
		peersCh:      make(chan proto.PeersMsg, 8),
		punchCh:      make(chan proto.PunchNotifyMsg, 8),
		relayReadyCh: make(chan proto.RelayReadyMsg, 8),
		relayConns:   make(map[string]*relayConn),
	}
}

// ConnectBroker establishes an mTLS connection to the broker.
func (m *Manager) ConnectBroker(addr string) error {
	conn, err := tls.Dial("tcp", addr, m.clientTLS)
	if err != nil {
		return fmt.Errorf("connect broker: %w", err)
	}

	m.brokerMu.Lock()
	m.broker = conn
	m.brokerMu.Unlock()

	// Start broker read loop
	go m.brokerReadLoop()

	return nil
}

// Start registers with the broker, discovers peers, and starts the listener.
func (m *Manager) Start() error {
	// Start listener FIRST (synchronously) so m.listener is set
	// before peerUpdateLoop can trigger hole-punch dials.
	ln, err := tls.Listen("tcp", m.listenAddr, m.serverTLS)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	m.listener = ln
	log.Printf("listening for peers on %s", ln.Addr())
	go m.acceptLoop()

	// Register with broker
	if err := m.register(); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Process peer updates from broker
	go m.peerUpdateLoop()

	return nil
}

func (m *Manager) register() error {
	return proto.WriteTextFrame(m.broker, &m.brokerWmu, proto.RegisterMsg{
		Type:       "register",
		ListenAddr: m.listenAddr,
	})
}

// brokerReadLoop reads frames from the broker and dispatches signaling messages.
func (m *Manager) brokerReadLoop() {
	for {
		frameType, payload, err := proto.ReadFrame(m.broker)
		if err != nil {
			log.Printf("broker connection lost: %v", err)
			return
		}

		if frameType == proto.FrameRelay {
			m.handleBrokerRelayFrame(payload)
			continue
		}

		if frameType != proto.FrameText {
			continue
		}

		msgType, err := proto.ParseEnvelope(payload)
		if err != nil {
			continue
		}

		switch msgType {
		case "registered":
			log.Printf("registered with broker as %s", m.selfName)

		case "peers":
			var msg proto.PeersMsg
			if err := json.Unmarshal(payload, &msg); err == nil {
				select {
				case m.peersCh <- msg:
				default:
				}
			}

		case "punch_notify":
			var msg proto.PunchNotifyMsg
			if err := json.Unmarshal(payload, &msg); err == nil {
				select {
				case m.punchCh <- msg:
				default:
				}
			}

		case "relay_ready":
			var msg proto.RelayReadyMsg
			if err := json.Unmarshal(payload, &msg); err == nil {
				select {
				case m.relayReadyCh <- msg:
				default:
				}
			}

		case "relay_notify":
			// Another peer wants to relay through us; accept asynchronously
			// so the broker read loop is not blocked during the TLS handshake.
			var msg proto.RelayNotifyMsg
			if err := json.Unmarshal(payload, &msg); err == nil {
				go m.handleRelayNotify(msg)
			}
		}
	}
}

func (m *Manager) handleBrokerRelayFrame(payload []byte) {
	sessionID, data, err := proto.ParseRelayFrame(payload)
	if err != nil {
		log.Printf("parse broker relay frame: %v", err)
		return
	}
	sid := hex.EncodeToString(sessionID[:])

	m.relayMu.Lock()
	rc, ok := m.relayConns[sid]
	m.relayMu.Unlock()

	if !ok {
		log.Printf("relay frame for unknown session %s", sid)
		return
	}
	rc.feed(data)
}

// peerUpdateLoop processes peer list updates from the broker.
func (m *Manager) peerUpdateLoop() {
	for msg := range m.peersCh {
		for _, pi := range msg.Peers {
			m.pmu.RLock()
			_, connected := m.peers[pi.Name]
			m.pmu.RUnlock()

			if !connected {
				go m.connectPeer(pi.Name, pi.Addr)
			}
		}
	}
}

// connectPeer attempts to establish a P2P connection to a peer.
// Tries: direct dial → hole punch → relay fallback.
func (m *Manager) connectPeer(name, addr string) {
	// Already connected?
	m.pmu.RLock()
	if _, ok := m.peers[name]; ok {
		m.pmu.RUnlock()
		return
	}
	m.pmu.RUnlock()

	// 1. Try direct dial
	conn, err := m.dialDirect(name, addr)
	if err != nil {
		log.Printf("direct dial to %s (%s) failed: %v", name, addr, err)

		// 2. Try hole punching
		conn, err = m.dialHolePunch(name)
		if err != nil {
			log.Printf("hole punch to %s failed: %v", name, err)

			// 3. Try relay
			conn, err = m.dialRelay(name)
			if err != nil {
				log.Printf("relay to %s failed: %v — giving up", name, err)
				return
			}
		}
	}

	m.addPeer(conn)
}

func (m *Manager) dialDirect(name, addr string) (*Conn, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	rawConn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsCfg := m.clientTLS.Clone()
	tlsCfg.ServerName = name
	tlsConn := tls.Client(rawConn, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	tlsConn.SetDeadline(time.Time{})

	peerName, err := certs.PeerName(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("peer name: %w", err)
	}
	if peerName != name {
		tlsConn.Close()
		return nil, fmt.Errorf("expected peer %s, got %s", name, peerName)
	}

	return newConn(name, tlsConn, m.store, m.onMessage, m.handlePeerLeave), nil
}

func (m *Manager) dialHolePunch(name string) (*Conn, error) {
	// Request hole punch via broker
	proto.WriteTextFrame(m.broker, &m.brokerWmu, proto.PunchReqMsg{
		Type:   "punch_req",
		Target: name,
	})

	// Wait for punch_notify from broker
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	var targetAddr string
	for {
		select {
		case notify := <-m.punchCh:
			if notify.Target == name {
				targetAddr = notify.Addr
				goto punch
			}
			// Not for us, put it back
			select {
			case m.punchCh <- notify:
			default:
			}
		case <-timer.C:
			return nil, fmt.Errorf("hole punch timeout")
		}
	}

punch:
	// Attempt simultaneous dial (no LocalAddr — SO_REUSEPORT not available in stdlib)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	rawConn, err := dialer.Dial("tcp", targetAddr)
	if err != nil {
		return nil, fmt.Errorf("hole punch dial: %w", err)
	}

	tlsCfg := m.clientTLS.Clone()
	tlsCfg.ServerName = name
	tlsConn := tls.Client(rawConn, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("hole punch TLS: %w", err)
	}
	tlsConn.SetDeadline(time.Time{})

	peerName, err := certs.PeerName(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	if peerName != name {
		tlsConn.Close()
		return nil, fmt.Errorf("hole punch: expected %s, got %s", name, peerName)
	}

	return newConn(name, tlsConn, m.store, m.onMessage, m.handlePeerLeave), nil
}

func (m *Manager) dialRelay(name string) (*Conn, error) {
	// Request relay via broker
	if err := proto.WriteTextFrame(m.broker, &m.brokerWmu, proto.RelayReqMsg{
		Type:   "relay_req",
		Target: name,
	}); err != nil {
		return nil, fmt.Errorf("send relay_req: %w", err)
	}

	// Wait for relay_ready from broker
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	var ready proto.RelayReadyMsg
	for {
		select {
		case r := <-m.relayReadyCh:
			if r.Target == name {
				ready = r
				goto gotReady
			}
			// Not for us — put back
			select {
			case m.relayReadyCh <- r:
			default:
			}
		case <-timer.C:
			return nil, fmt.Errorf("relay timeout waiting for relay_ready")
		}
	}

gotReady:
	// Decode session ID
	sidBytes, err := hex.DecodeString(ready.SessionID)
	if err != nil || len(sidBytes) != 16 {
		return nil, fmt.Errorf("invalid session ID %q", ready.SessionID)
	}
	var sessionID [16]byte
	copy(sessionID[:], sidBytes)

	// Create relay conn and register it so the broker read loop can feed it
	rc := newRelayConn(sessionID, m.broker, &m.brokerWmu)

	m.relayMu.Lock()
	m.relayConns[ready.SessionID] = rc
	m.relayMu.Unlock()

	// Establish a TLS session over the relay conn (end-to-end encryption).
	// The broker only forwards opaque bytes — it cannot decrypt this.
	tlsCfg := m.clientTLS.Clone()
	tlsCfg.ServerName = name
	tlsConn := tls.Client(rc, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		rc.Close()
		m.relayMu.Lock()
		delete(m.relayConns, ready.SessionID)
		m.relayMu.Unlock()
		return nil, fmt.Errorf("relay TLS handshake: %w", err)
	}

	peerName, err := certs.PeerName(tlsConn)
	if err != nil || peerName != name {
		tlsConn.Close()
		m.relayMu.Lock()
		delete(m.relayConns, ready.SessionID)
		m.relayMu.Unlock()
		return nil, fmt.Errorf("relay peer name mismatch: got %q, want %q", peerName, name)
	}

	log.Printf("relay session %s established with %s", ready.SessionID, name)
	return newConn(name, tlsConn, m.store, m.onMessage, m.handlePeerLeave), nil
}

// acceptLoop accepts incoming P2P connections from the already-started listener.
func (m *Manager) acceptLoop() {
	for {
		rawConn, err := m.listener.Accept()
		if err != nil {
			log.Printf("peer accept: %v", err)
			return
		}
		go m.handleIncoming(rawConn.(*tls.Conn))
	}
}

func (m *Manager) handleIncoming(tlsConn *tls.Conn) {
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("incoming peer handshake: %v", err)
		tlsConn.Close()
		return
	}

	name, err := certs.PeerName(tlsConn)
	if err != nil {
		log.Printf("incoming peer name: %v", err)
		tlsConn.Close()
		return
	}

	// Already connected to this peer?
	// Apply lexicographic tie-breaking: the peer with the smaller name acts as
	// the server (accepts incoming). If we already have a connection and the
	// incoming peer's name is smaller than ours, close the existing outgoing
	// and accept this incoming one so both sides end up using the same TCP conn.
	m.pmu.RLock()
	_, exists := m.peers[name]
	m.pmu.RUnlock()
	if exists {
		if name < m.selfName {
			// Peer has smaller name — prefer their incoming over our outgoing.
			m.pmu.Lock()
			if existing, ok := m.peers[name]; ok {
				existing.Close()
				delete(m.peers, name)
			}
			m.pmu.Unlock()
			// Fall through to addPeer below.
		} else {
			log.Printf("already connected to %s, rejecting incoming (we are the acceptor)", name)
			tlsConn.Close()
			return
		}
	}

	c := newConn(name, tlsConn, m.store, m.onMessage, m.handlePeerLeave)
	m.addPeer(c)
}

// handleRelayNotify accepts a relay session offered by the broker and completes
// the TLS server handshake over the relay transport so both sides have a
// fully encrypted P2P connection through the broker.
func (m *Manager) handleRelayNotify(msg proto.RelayNotifyMsg) {
	// Tell the broker we accept
	if err := proto.WriteTextFrame(m.broker, &m.brokerWmu, proto.RelayAcceptMsg{
		Type: "relay_accept",
		Peer: msg.Peer,
	}); err != nil {
		log.Printf("relay_accept send: %v", err)
		return
	}

	// Wait for relay_ready
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	var ready proto.RelayReadyMsg
	for {
		select {
		case r := <-m.relayReadyCh:
			if r.Target == msg.Peer {
				ready = r
				goto gotReady
			}
			// Not ours — put it back
			select {
			case m.relayReadyCh <- r:
			default:
			}
		case <-timer.C:
			log.Printf("relay accept: timeout waiting for relay_ready from %s", msg.Peer)
			return
		}
	}

gotReady:
	sidBytes, err := hex.DecodeString(ready.SessionID)
	if err != nil || len(sidBytes) != 16 {
		log.Printf("relay accept: invalid session ID %q", ready.SessionID)
		return
	}
	var sessionID [16]byte
	copy(sessionID[:], sidBytes)

	rc := newRelayConn(sessionID, m.broker, &m.brokerWmu)
	m.relayMu.Lock()
	m.relayConns[ready.SessionID] = rc
	m.relayMu.Unlock()

	// Accept TLS as server over the relay transport (end-to-end encrypted;
	// broker only forwards opaque bytes).
	tlsConn := tls.Server(rc, m.serverTLS)
	if err := tlsConn.Handshake(); err != nil {
		rc.Close()
		m.relayMu.Lock()
		delete(m.relayConns, ready.SessionID)
		m.relayMu.Unlock()
		log.Printf("relay accept TLS handshake from %s: %v", msg.Peer, err)
		return
	}

	peerName, err := certs.PeerName(tlsConn)
	if err != nil || peerName != msg.Peer {
		tlsConn.Close()
		m.relayMu.Lock()
		delete(m.relayConns, ready.SessionID)
		m.relayMu.Unlock()
		log.Printf("relay accept: peer name mismatch: got %q want %q", peerName, msg.Peer)
		return
	}

	log.Printf("relay session %s accepted from %s", ready.SessionID, peerName)
	c := newConn(peerName, tlsConn, m.store, m.onMessage, m.handlePeerLeave)
	m.addPeer(c)
}

func (m *Manager) addPeer(c *Conn) {
	m.pmu.Lock()
	// Double-check: another goroutine may have added the same peer
	if _, exists := m.peers[c.PeerName]; exists {
		m.pmu.Unlock()
		c.Close()
		return
	}
	m.peers[c.PeerName] = c
	m.pmu.Unlock()

	log.Printf("connected to peer: %s", c.PeerName)

	if m.onPeerJoin != nil {
		m.onPeerJoin(c.PeerName)
	}

	// Send hello + start read loop
	c.SendHello(m.selfName)
	go c.readLoop()
}

func (m *Manager) handlePeerLeave(name string) {
	m.pmu.Lock()
	delete(m.peers, name)
	m.pmu.Unlock()

	log.Printf("peer left: %s", name)
	if m.onPeerLeave != nil {
		m.onPeerLeave(name)
	}
}

// Broadcast sends a chat message to all connected peers.
func (m *Manager) Broadcast(body string) error {
	ts := time.Now().UnixMilli()

	// Store locally first
	m.store.InsertMessage(m.selfName, body, ts)

	m.pmu.RLock()
	defer m.pmu.RUnlock()

	var firstErr error
	for _, c := range m.peers {
		msg := proto.ChatMsg{
			Type: "msg",
			From: m.selfName,
			Body: body,
			TS:   ts,
		}
		if err := proto.WriteTextFrame(c.tlsConn, &c.wmu, msg); err != nil {
			log.Printf("send to %s: %v", c.PeerName, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// OnlinePeers returns the names of all connected peers.
func (m *Manager) OnlinePeers() []string {
	m.pmu.RLock()
	defer m.pmu.RUnlock()

	names := make([]string, 0, len(m.peers))
	for name := range m.peers {
		names = append(names, name)
	}
	return names
}

// Shutdown cleanly disconnects from all peers and the broker.
func (m *Manager) Shutdown() {
	m.pmu.Lock()
	for _, c := range m.peers {
		c.Leave(m.selfName)
	}
	m.peers = make(map[string]*Conn)
	m.pmu.Unlock()

	m.brokerMu.Lock()
	if m.broker != nil {
		proto.WriteTextFrame(m.broker, &m.brokerWmu, proto.LeaveMsg{Type: "leave"})
		m.broker.Close()
	}
	m.brokerMu.Unlock()

	if m.listener != nil {
		m.listener.Close()
	}
}

// SelfName returns this client's identity (from cert CN).
func (m *Manager) SelfName() string {
	return m.selfName
}
