# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Hard constraints

- **All network communication must be TLS-encrypted — no exceptions.** No plaintext fallback, no `--insecure` flag, no unencrypted health-check endpoints. This applies to local development too: use self-signed certs from `matechat certs init`, never skip TLS to "make testing easier".

## Project

`matechat` is a family chat CLI written in Go. A single binary serves both roles:
- **broker** (`matechat serve`) — runs on a self-hosted Ubuntu VPS; acts as peer directory, NAT hole-punch signaling coordinator, and encrypted relay fallback. Stores **no messages**.
- **client** (`matechat`) — TUI chat interface; connects to broker for peer discovery, then communicates **directly peer-to-peer** with each online family member. Stores history in local SQLite.

Authentication is **mTLS only** — no passwords, no accounts. Every connection (client↔broker and client↔client) uses the same family CA.

## Commands

```sh
go build ./...                        # build all packages
go test ./...                         # run all tests
go test ./internal/broker/...         # run tests for a specific package
go run ./cmd/matechat serve           # start broker locally for development
go run ./cmd/matechat -- --broker localhost:9000 --cert device.crt --key device.key --ca family-ca.crt
```

Go is installed at `/opt/homebrew/bin/go` (not in the default PATH for non-interactive shells). Use full path or add `/opt/homebrew/bin` to `~/.zshrc`.

## Architecture

```
cmd/
  matechat/main.go      # entry point, subcommand dispatch (serve / certs / default client)
internal/
  broker/               # server: mTLS listener, peer directory, hole-punch signaling, relay fallback
  client/               # TUI client logic (bubbletea model), manages multiple peer connections
  peer/                 # P2P connection lifecycle: direct dial, hole punch, relay upgrade
  proto/                # wire protocol: length-prefixed frames (JSON + binary + relay)
  store/                # SQLite-backed local message history (client-side)
  certs/                # CA init, cert issuance helpers (wraps crypto/x509)
```

## Wire protocol

Three frame types, distinguished by a 1-byte discriminator after the 4-byte length:

```
Text frame:   [4-byte BE uint32 length][0x01][JSON bytes]          — control + chat messages
Binary frame: [4-byte BE uint32 length][0x02][transfer_id:16B][chunk_idx:4B][chunk_count:4B][raw bytes]  — file chunks
Relay frame:  [4-byte BE uint32 length][0x03][session_id:16B][opaque TLS record]  — broker relay path
```

Two separate JSON vocabularies:
- **Client↔Broker**: `register`, `registered`, `peers`, `peers_req`, `punch_req`, `punch_notify`, `relay_req`, `relay_notify`, `relay_accept`, `relay_ready`, `leave`
- **Client↔Client**: `hello`, `msg`, `sync_req`, `sync`, `upload_start`, `leave`

Identity (`from`) is always derived from the TLS peer certificate CN, never from the JSON payload. Relay frames are forwarded by the broker without decryption — the inner payload is an opaque TLS record from the client↔client mTLS session.

## TLS / cert conventions

- Family CA cert: `family-ca.crt` (kept offline)
- Server cert: signed by family CA, CN = hostname
- Device certs: signed by family CA, CN = human-readable name (e.g. `mom`, `dads-phone`)
- The broker loads `--ca` as the only trusted root; standard system CAs are not used
- `matechat certs init` generates the CA; `matechat certs issue --name <name>` signs a device cert

## Key dependencies

- [`github.com/charmbracelet/bubbletea`](https://github.com/charmbracelet/bubbletea) — TUI framework
- [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite) — pure-Go SQLite (no CGo required)
- [`github.com/spf13/cobra`](https://github.com/spf13/cobra) — CLI subcommands
