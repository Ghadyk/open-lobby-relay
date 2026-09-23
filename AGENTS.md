# AGENTS.md — open-lobby-relay

## Overview

A self-hostable lobby server + reverse-tunnel TCP relay for online games. Hosts register
rooms and open an outbound tunnel; joiners connect to a relay port and are bridged to the
host's own game server. The relay is protocol-agnostic and never dials a peer.

## File structure

- `main.go` — entry point
- `config.go` — env config + validation
- `room.go` — `Room` model
- `relay.go` — two-listener relay (host + joiner), framing, bridging, quotas
- `server.go` — HTTP handlers, middleware, join auth, rate limiters, port allocation
- `admin.go` — operator endpoints (Bearer `ADMIN_TOKEN`)
- `bans.go` — runtime IP ban store
- `server_test.go` — test suite
- `README.md`, `PROTOCOL.md`, `SECURITY.md` — docs

## Key design decisions

- **Reverse tunnel**: both host and joiners connect outbound; nothing accepts inbound. The
  relay signals the host over a persistent tunnel and the host opens a data connection per
  joiner. It never dials a peer, so it is not an open proxy.
- **Two ports per relay**: a *host port* (only the host connects; framing = `role byte` +
  64-byte secret) and a *joiner port* (every connection is a joiner, no handshake). This
  removes any assumption about the game protocol.
- **Host auth**: role `0x01` = tunnel, `0x02` = data; the room secret is verified with
  `subtle.ConstantTimeCompare`. A reconnecting `0x01` replaces the tunnel.
- **Join auth**: `POST /rooms/{id}/verify` records the caller IP for `JoinAuthTTL`; the relay
  rejects joiners whose IP is not authorized (`REQUIRE_JOIN_AUTH`).
- **Bounded resources**: `MAX_ROOMS`, `MAX_RELAYS`, `MAX_CONNS_PER_RELAY`, per-IP API rate,
  per-IP joiner connection rate, per-session byte cap, idle timeout, and a
  `runtime.NumCPU()`-sized bcrypt semaphore.
- **In-memory only**: a restart clears rooms, secrets, and relays.

## Concurrency model

- `s.mu` (RWMutex) — rooms, relays, usedPorts
- `relay.mu` — the current host tunnel
- `s.ipMu` / `s.relayConnMu` — per-IP limiters
- `s.joinMu` — join authorizations
- `relay.connCount`, `relay.bytesProxied`, `s.bytesProxied` — atomic
- `relay.wg` / `s.relaysWg` — per-relay and global goroutine tracking
- `closeRelayLocked` requires `s.mu`; `allocatePortPair` requires `s.mu`

## Build & test

```bash
GOTOOLCHAIN=local go build -o dist/open-lobby-relay .
GOTOOLCHAIN=local go test -v -count=1 ./...
GOTOOLCHAIN=local go vet ./...
gofmt -l .
golangci-lint run
gosec ./...
```

Use `GOTOOLCHAIN=local` (the local toolchain may be newer than `go.mod`'s `go` directive).

## Environment variables

See the README table. Key ones: `PUBLIC_HOST`, `RELAY_PORT_MIN/MAX` (2 ports per relay),
`ADMIN_TOKEN`, `REQUIRE_JOIN_AUTH`, `JOIN_AUTH_TTL_SECONDS`, `RELAY_MAX_BYTES`,
`RELAY_IDLE_TIMEOUT_SECONDS`, `RELAY_CONN_RPM`.

## Deployment

- Docker: create a `.env` (at least `PUBLIC_HOST`), then `docker compose up -d`.
- Open the API port and the relay port range; put TLS in front of the API only.
- Keep protocol/security documentation in sync with `relay.go` and `server.go`.
