# matechat — Design

Sequence diagrams for each use case.

**Participants:**
- **Client A / B** — any device running `matechat`
- **Broker** — the self-hosted Ubuntu server (directory + signaling + relay fallback)
- **CA** — family certificate authority (offline tool, not a running service)
- **SQLite** — local database on each client device (not on the broker)

The broker stores **no messages**. All history lives on client devices.

---

## 1. New device onboarding

The admin generates the family CA once, then issues one cert per device. Transfer is out-of-band (scp, USB, etc.).

```mermaid
sequenceDiagram
    actor Admin
    participant CA as Family CA (offline)
    participant Device

    Admin->>CA: matechat certs init
    CA-->>Admin: family-ca.crt + family-ca.key (keep offline)

    Admin->>CA: matechat certs issue --name mom
    CA-->>Admin: mom.crt + mom.key (signed by family CA)

    Admin-->>Device: transfer mom.crt + mom.key + family-ca.crt (out-of-band)
    Note over Device: Device is ready to connect
```

---

## 2. Client registers with broker

On startup, each client establishes an mTLS connection to the broker and registers its reachable address. The broker never stores messages — only the live peer directory.

```mermaid
sequenceDiagram
    participant C as Client (mom)
    participant B as Broker

    C->>B: TCP connect
    B->>C: TLS: send server.crt
    C->>B: TLS: send mom.crt
    B->>B: verify mom.crt against family-ca.crt
    C->>C: verify server.crt against family-ca.crt

    alt cert invalid or untrusted
        B-->>C: TLS handshake failure (connection closed)
    else cert valid
        C->>B: {type:"register", listen_addr:"203.0.113.5:9100"}
        B->>B: directory[mom] = {addr:"203.0.113.5:9100", connected_at:...}
        B-->>C: {type:"registered"}
        B-->>C: {type:"peers", peers:[{name:"dad", addr:"..."}, ...]}
    end
```

---

## 3. Peer discovery

Clients can query the broker at any time for the current list of online peers.

```mermaid
sequenceDiagram
    participant C as Client (mom)
    participant B as Broker

    C->>B: {type:"peers_req"}
    B-->>C: {type:"peers", peers:[{name:"dad", addr:"1.2.3.4:9100"}, {name:"kid", addr:"5.6.7.8:9200"}]}
    Note over C: attempt connections to each peer
```

---

## 4. P2P connection — direct (happy path)

When a peer has a reachable public address, the client dials directly. The broker is not involved in message flow.

```mermaid
sequenceDiagram
    participant A as Client A (mom/BR)
    participant B as Broker
    participant P as Client B (dad/DE)

    A->>B: {type:"peers_req"}
    B-->>A: {type:"peers", peers:[{name:"dad", addr:"1.2.3.4:9100"}]}

    A->>P: TCP connect to 1.2.3.4:9100
    P->>A: TLS: send dad.crt
    A->>P: TLS: send mom.crt
    P->>P: verify mom.crt against family-ca.crt
    A->>A: verify dad.crt against family-ca.crt

    Note over A,P: Direct mTLS session established — broker not in path
    A->>P: {type:"hello", from:"mom"}
    P-->>A: {type:"hello", from:"dad"}
```

---

## 5. P2P connection — NAT hole punching

When both peers are behind NAT, the broker coordinates a simultaneous dial to punch through.

```mermaid
sequenceDiagram
    participant A as Client A (mom/BR)
    participant B as Broker
    participant P as Client B (dad/DE)

    A->>B: {type:"punch_req", target:"dad"}
    B->>B: look up dad's registered addr + broker-observed public addr
    B-->>P: {type:"punch_notify", initiator:"mom", addr:"203.0.113.5:9101"}
    B-->>A: {type:"punch_notify", target:"dad",   addr:"198.51.100.8:9200"}

    Note over A,P: Both dial each other simultaneously
    A->>P: TCP SYN to 198.51.100.8:9200
    P->>A: TCP SYN to 203.0.113.5:9101

    Note over A,P: NAT entries created on both sides — connection established
    A->>P: TLS handshake (mTLS, family CA)
    P->>A: TLS handshake (mTLS, family CA)
    Note over A,P: Direct mTLS session established
```

---

## 6. P2P connection — relay fallback

When direct dial and hole punching both fail (symmetric NAT, CGNAT), the broker relays encrypted frames. It forwards ciphertext only — it cannot decrypt the content.

```mermaid
sequenceDiagram
    participant A as Client A (mom/BR)
    participant B as Broker
    participant P as Client B (dad/DE)

    Note over A,P: Direct and hole-punch attempts timed out
    A->>B: relay_req target=dad
    B-->>P: relay_notify peer=mom
    P-->>B: relay_accept peer=mom
    B-->>A: relay_ready session=xyz

    Note over A,B: broker forwards opaque frames — no decryption
    A->>B: RELAY frame session=xyz, ciphertext for dad
    B->>P: RELAY frame session=xyz, ciphertext for dad
    P->>B: RELAY frame session=xyz, ciphertext for mom
    B->>A: RELAY frame session=xyz, ciphertext for mom

    Note over B: sees session_id + ciphertext only
```

---

## 7. Send a message (P2P)

Messages travel directly between peers over the established mTLS session. Each recipient stores the message in their local SQLite.

```mermaid
sequenceDiagram
    participant A as Client A (mom)
    participant B as Client B (dad)
    participant C as Client C (kid)
    participant DBA as SQLite (mom's device)
    participant DBB as SQLite (dad's device)
    participant DBC as SQLite (kid's device)

    Note over A: mom types a message
    A->>DBA: INSERT message (from:"mom", body:"dinner?", ts:...)
    A->>B: {type:"msg", from:"mom", body:"dinner?", ts:...}
    A->>C: {type:"msg", from:"mom", body:"dinner?", ts:...}
    B->>DBB: INSERT message
    C->>DBC: INSERT message
```

---

## 8. History sync on connect

When two peers connect, they exchange their latest message timestamps and sync any gaps. No broker involved.

```mermaid
sequenceDiagram
    participant A as Client A (mom)
    participant B as Client B (dad)
    participant DBA as SQLite (mom)
    participant DBB as SQLite (dad)

    Note over A,B: mTLS session just established
    A->>B: {type:"sync_req", since_ts: 1710000000000}
    B->>DBB: SELECT messages WHERE ts > 1710000000000
    DBB-->>B: rows
    B-->>A: {type:"sync", messages:[...]}
    A->>DBA: INSERT missing messages
    B->>A: {type:"sync_req", since_ts: 1709999000000}
    A->>DBA: SELECT messages WHERE ts > 1709999000000
    DBA-->>A: rows
    A-->>B: {type:"sync", messages:[...]}
    B->>DBB: INSERT missing messages
```

---

## 9. File transfer (P2P direct)

Files transfer directly between peers. No broker involvement — not even as relay unless the transport falls back. The sender notifies all peers; interested peers pull directly.

```mermaid
sequenceDiagram
    participant A as Client A (mom, sender)
    participant B as Client B (dad)
    participant C as Client C (kid)

    A->>B: {type:"upload_start", transfer_id:"uuid", filename:"photo.jpg", size:204800, chunks:4}
    A->>C: {type:"upload_start", transfer_id:"uuid", filename:"photo.jpg", size:204800, chunks:4}

    loop for each 64 KB chunk (i = 0..3)
        A->>B: BINARY frame [transfer_id(16b)][chunk_idx(4b)][chunk_count(4b)][raw bytes]
        A->>C: BINARY frame [transfer_id(16b)][chunk_idx(4b)][chunk_count(4b)][raw bytes]
    end

    B->>B: reassemble → photo.jpg saved locally
    C->>C: reassemble → photo.jpg saved locally
```

---

## 10. Graceful disconnect

Client notifies both the broker (to remove from directory) and all connected peers.

```mermaid
sequenceDiagram
    participant C as Client (dad)
    participant B as Broker
    participant P as Peers

    C->>B: {type:"leave"}
    B->>B: remove "dad" from directory
    C->>P: {type:"leave", from:"dad"}
    Note over C,P: TCP connections closed
```

---

## 11. Unexpected disconnect (network drop)

```mermaid
sequenceDiagram
    participant C as Client (dad)
    participant B as Broker
    participant P as Peers

    C--xB: connection lost
    B->>B: read error / EOF — remove "dad" from directory
    B->>B: notify relayed sessions (if any) that peer is gone

    C--xP: peer connections drop
    P->>P: read error / EOF on each connection
    Note over P: display "dad went offline"
```

---

## 12. Access revocation

Revocation is enforced at the TLS handshake level. Existing sessions are not forcibly dropped (a broker restart evicts them if needed).

```mermaid
sequenceDiagram
    actor Admin
    participant CA as Family CA (offline)
    participant B as Broker
    participant P as All Peers
    participant RC as Revoked Client

    Admin->>CA: matechat certs revoke --name old-phone
    CA-->>Admin: cert removed from allowlist
    Admin->>B: SIGHUP (reload CA config)
    Admin->>P: distribute updated family-ca config (out-of-band)

    RC->>B: TCP connect + TLS: send old-phone.crt
    B->>B: verify → revoked
    B-->>RC: TLS handshake failure

    RC->>P: TCP connect + TLS: send old-phone.crt
    P->>P: verify → revoked
    P-->>RC: TLS handshake failure
```

---

## Wire frame reference

### Client ↔ Broker protocol

```
Text frame: [4-byte BE uint32 length][0x01][JSON bytes]
```

| `type`           | Direction        | Key fields |
|------------------|------------------|------------|
| `register`       | client → broker  | `listen_addr` |
| `registered`     | broker → client  | |
| `peers`          | broker → client  | `peers[]{name, addr}` |
| `peers_req`      | client → broker  | |
| `punch_req`      | client → broker  | `target` |
| `punch_notify`   | broker → client  | `initiator` or `target`, `addr` |
| `relay_req`      | client → broker  | `target` |
| `relay_notify`   | broker → client  | `peer` |
| `relay_accept`   | client → broker  | `peer` |
| `relay_ready`    | broker → client  | `target`, `session_id` |
| `leave`          | client → broker  | |

### Client ↔ Client protocol

```
Text frame:   [4-byte BE uint32 length][0x01][JSON bytes]
Binary frame: [4-byte BE uint32 length][0x02][transfer_id: 16B][chunk_idx: 4B BE][chunk_count: 4B BE][raw bytes]
```

| `type`          | Direction         | Key fields |
|-----------------|-------------------|------------|
| `hello`         | client → client   | `from` |
| `msg`           | client → client   | `from`, `body`, `ts` |
| `sync_req`      | client → client   | `since_ts` |
| `sync`          | client → client   | `messages[]` |
| `upload_start`  | client → client   | `transfer_id`, `filename`, `size`, `chunks` |
| `leave`         | client → client   | `from` |

### Relay frames (via broker fallback)

```
[4-byte BE uint32 length][0x03][session_id: 16B][encrypted payload]
```

The payload is an opaque TLS record from the originating client's mTLS session. The broker forwards it without decryption.
