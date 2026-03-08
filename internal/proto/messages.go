package proto

// Envelope is used for initial dispatch on the "type" field.
type Envelope struct {
	Type string `json:"type"`
}

// --- Client ↔ Broker messages ---

type RegisterMsg struct {
	Type       string `json:"type"` // "register"
	ListenAddr string `json:"listen_addr"`
}

type RegisteredMsg struct {
	Type string `json:"type"` // "registered"
}

type PeerInfo struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

type PeersMsg struct {
	Type  string     `json:"type"` // "peers"
	Peers []PeerInfo `json:"peers"`
}

type PeersReqMsg struct {
	Type string `json:"type"` // "peers_req"
}

type PunchReqMsg struct {
	Type   string `json:"type"` // "punch_req"
	Target string `json:"target"`
}

type PunchNotifyMsg struct {
	Type      string `json:"type"` // "punch_notify"
	Initiator string `json:"initiator,omitempty"`
	Target    string `json:"target,omitempty"`
	Addr      string `json:"addr"`
}

type RelayReqMsg struct {
	Type   string `json:"type"` // "relay_req"
	Target string `json:"target"`
}

type RelayNotifyMsg struct {
	Type string `json:"type"` // "relay_notify"
	Peer string `json:"peer"`
}

type RelayAcceptMsg struct {
	Type string `json:"type"` // "relay_accept"
	Peer string `json:"peer"`
}

type RelayReadyMsg struct {
	Type      string `json:"type"` // "relay_ready"
	Target    string `json:"target"`
	SessionID string `json:"session_id"`
}

// --- Client ↔ Client messages ---

type HelloMsg struct {
	Type string `json:"type"` // "hello"
	From string `json:"from"`
}

type ChatMsg struct {
	Type string `json:"type"` // "msg"
	From string `json:"from"`
	Body string `json:"body"`
	TS   int64  `json:"ts"`
}

type SyncReqMsg struct {
	Type    string `json:"type"` // "sync_req"
	SinceTS int64  `json:"since_ts"`
}

type SyncMsg struct {
	Type     string    `json:"type"` // "sync"
	Messages []ChatMsg `json:"messages"`
}

type UploadStartMsg struct {
	Type       string `json:"type"` // "upload_start"
	TransferID string `json:"transfer_id"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	Chunks     int    `json:"chunks"`
}

// --- Shared ---

type LeaveMsg struct {
	Type string `json:"type"` // "leave"
	From string `json:"from,omitempty"`
}
