# matechat

A family chat CLI — no clouds, no accounts, no surveillance.

## What is this?

`matechat` is a command-line chat tool built for families spread across the world. Devices talk **directly to each other** (P2P). A self-hosted broker on a small server acts as a rendezvous point and relay fallback — it never reads your messages.

```
$ matechat
> [mom] anyone home for dinner?
> [you] yes!
> [dad] 20 mins
```

---

## Architecture

The broker has three responsibilities — none of them is relaying plaintext:

1. **Directory** — maintains a live registry of who is online and reachable at what address
2. **Signaling** — coordinates NAT hole-punch attempts between peers
3. **Relay fallback** — forwards opaque encrypted bytes when direct connection fails (symmetric NAT, CGNAT)

```
                  ┌──────────────────────────┐
                  │   Broker (Ubuntu VPS)    │
                  │  - live peer directory   │
                  │  - hole-punch signaling  │
                  │  - relay fallback        │
                  └──────────────────────────┘
                       ▲    ▲    ▲    ▲
              register/│    │    │    │register/
              discover │    │    │    │discover
                       │    │    │    │
           ┌───────────┘    │    │    └──────────────┐
           │                │    │                   │
      [mom/BR]              │    │             [dad/DE]
           │                │    │                   │
           │         [kid1/US]  [kid2/JP]            │
           │                                         │
           └──────── mTLS direct (P2P) ─────────────┘
                  (or via broker relay if NAT blocks)
```

Messages travel **peer-to-peer**, encrypted end-to-end with mTLS. The broker sees only encrypted bytes when acting as relay — it cannot read message content.

Connection strategy (tried in order):
1. **Direct dial** — if the peer has a reachable public address
2. **Hole punching** — broker coordinates a simultaneous dial to punch through NAT
3. **Relay** — broker forwards ciphertext as fallback (symmetric NAT, CGNAT)

Each client maintains **multiple simultaneous connections** — one per online peer.

---

## Security

Authentication uses **mutual TLS (mTLS)** for every connection — client↔broker and client↔client alike.

1. Generate a **family CA** (self-signed root, stays offline on a USB stick)
2. Each device gets a **certificate signed by the family CA**
3. Both sides of every connection verify the peer's cert against the family CA
4. Identity is derived from the certificate CN — never from message payloads

Adding a family member = signing a new cert.
Revoking access = remove their cert from the allowlist and send broker a SIGHUP.

End-to-end encryption is inherent: the mTLS session is established between the two client devices directly. Even when the broker relays packets, it cannot decrypt them — it only sees the outer TLS layer.

---

## Stack

| Concern | Choice | Rationale |
|---|---|---|
| Language | Go | Single binary, ships everywhere, excellent TLS/crypto support |
| Transport | TCP + TLS 1.3 | Simple, mTLS is built-in, no extra deps |
| NAT traversal | TCP hole punching + relay fallback | No third-party infrastructure needed |
| Protocol | length-prefixed frames (JSON + binary) | Readable for control, efficient for files |
| Storage | SQLite (client-side, per device) | Local history, no data on the server |
| TUI | [bubbletea](https://github.com/charmbracelet/bubbletea) | Clean terminal UI, keyboard-driven |
| Cert tooling | `step` CLI (Smallstep) | Dead-simple CA management |

---

## Usage

```sh
# Broker (Ubuntu VPS — runs as systemd service)
matechat serve --cert server.crt --key server.key --ca family-ca.crt

# Client (any device)
matechat --broker chat.example.com:9000 --cert device.crt --key device.key --ca family-ca.crt

# Cert management
matechat certs init              # generate family CA (run once, keep offline)
matechat certs issue --name mom  # sign a new device cert
```

---

## Connectivity

matechat tries three methods in order when connecting to a peer:

| Scenario | Method used |
|---|---|
| Same home network | Direct dial |
| Different homes (residential internet) | Hole punch (works ~80% of the time) |
| Mobile data | Hole punch usually works |
| CGNAT / corporate firewall / strict NAT | Relay fallback |

The relay fallback is always available. All three methods use end-to-end mTLS
between the two client devices — even relay frames are opaque ciphertext to the broker.

No port forwarding is required. The broker observes each client's public IP and
combines it with the client's registered port to give peers a dialable address.

---

## Non-goals

- No mobile app (use SSH + a terminal emulator, or a future TUI wrapper)
- No rich media (text and file attachments are enough)
- No cloud dependencies of any kind
- No message storage on the server — history lives on each device

---

## Status

Working implementation. Broker and client are fully functional: mTLS peer discovery, NAT hole punching, relay fallback, TUI chat, and local SQLite history are all implemented.

## License

TBD
