# Architecture

This document explains how `open-lobby-relay` works internally. For the wire-level details
see [`../PROTOCOL.md`](../PROTOCOL.md).

## Components

| Component | Responsibility |
|---|---|
| **Lobby API** | HTTP service: rooms, heartbeats, passwords, relay allocation, admin |
| **Relay** | Per-room pair of TCP listeners that bridge joiners to the host |
| **Host client** | Game-side code that registers a room and runs the outbound tunnel |
| **Joiner client** | Game-side code that finds a room and connects to its relay |

The server keeps everything in memory: a map of rooms and a map of relays, both guarded by
a single `RWMutex`. There is no database; a restart clears the lobby.

## The two-port model

Each relay opens **two** TCP ports on the relay host:

```
                     host port                 joiner port
                   (secret-framed)          (no handshake)
                        ▲                         ▲
          host connects │                         │ joiners connect
                        │                         │
                 ┌──────┴─────────────────────────┴──────┐
                 │              Relay process            │
                 │  hostTunnel ──signal──▶ open data conn │
                 │  joiner  ◀────bytes────▶  data conn    │
                 └────────────────────────────────────────┘
```

- **Host port** — only the host connects here. Every connection starts with a 1-byte role
  (`0x01` tunnel, `0x02` data) followed by the 64-byte room secret. Because no joiner ever
  uses this port, the framing is unambiguous.
- **Joiner port** — every connection is a joiner. There is no handshake, so the relay makes
  **no assumption about the game's protocol**. The joiner just connects and starts talking.

Splitting the ports is what makes the relay game-agnostic: a single shared port would require
guessing whether a connection is a host or a joiner from its first bytes.

## Room lifecycle

```
 POST /rooms ─────────────▶ room created (state: no relay)
      │
      │  heartbeats every ~10s (X-Room-Token) keep LastSeen fresh
      ▼
 POST /relay ─────────────▶ two ports allocated, listeners started
      │                      room is NOT advertised yet
      │
 host connects to host port ─▶ tunnel established
      ▼
 room advertised with relay_host + relay_port   ◀── joiners can now see & use it
      │
 DELETE /rooms/{id}   or   no heartbeat for 25s
      ▼
 relay closed, ports freed, room removed
```

Key points:

- A room is only advertised **with relay info once the host tunnel is connected**, so a
  listed relay is always usable. Before that, `use_relay` is false and `relay_host`/`relay_port`
  are empty.
- If the host tunnel drops, the relay is **unadvertised again** until the host reconnects with a
  new `0x01` (the listeners stay up, so the same host port is reused). This means a dead tunnel
  is never listed as joinable. `POST /admin/relays/close-all` clears every room's advertisement
  the same way.
- `DELETE /rooms/{id}`, an admin delete, and heartbeat expiry all funnel through the same
  cleanup: close both listeners, cancel active bridges, free both ports.

## Relay data flow

### 1. Host opens the tunnel

The host connects to `relay_host:host_port`, writes `0x01` + secret. The relay authenticates
(constant-time compare) and stores the connection as the room's tunnel. If the host
reconnects later with another `0x01`, the new connection replaces the old one — this is how
the host recovers from a network blip.

### 2. A joiner arrives

A joiner connects to `relay_host:joiner_port`. The relay:

1. checks the joiner's IP against the runtime ban list,
2. applies the per-IP joiner connection rate limit,
3. checks the join authorization (see below),
4. checks the per-relay connection cap,
5. writes a single `0x01` byte to the host tunnel (“open a data connection now”),
6. waits (up to 15s) for the host's data connection.

### 3. The host opens a data connection

The host reads the `0x01` signal, connects to `relay_host:host_port` again, writes `0x02` +
secret, connects to its own game server on `127.0.0.1:<game port>`, and bridges the two.

### 4. The relay bridges

The relay pairs the waiting joiner with the host's data connection and copies bytes both ways
until either side closes, the relay is cancelled, the session goes idle for
`RELAY_IDLE_TIMEOUT_SECONDS`, or `RELAY_MAX_BYTES` is exhausted.

```
 joiner ──bytes──▶ relay ──bytes──▶ data conn ──bytes──▶ host game server
 joiner ◀─bytes── relay ◀─bytes── data conn ◀─bytes── host game server
```

The relay never parses the bytes and never dials a peer.

## Join authorization

Because joiners send no handshake, the relay cannot verify a password inline. Instead:

1. A joiner calls `POST /rooms/{id}/verify` (empty password for open rooms).
2. On success the server records the **caller's IP** as authorized for
   `JOIN_AUTH_TTL_SECONDS`.
3. The relay accepts a joiner only if its source IP has a live authorization for that room.

This means a client cannot skip the password and connect straight to the joiner port, and
blind connection floods are rejected before consuming a relay slot. It requires the joiner's
HTTP IP and relay TCP IP to match (normally true; see
[`TROUBLESHOOTING.md`](TROUBLESHOOTING.md) if not). Set `REQUIRE_JOIN_AUTH=false` to disable.

## Concurrency and resources

| State | Guard |
|---|---|
| `rooms`, `relays`, `usedPorts` | `s.mu` (`RWMutex`) |
| current host tunnel | `Relay.mu` |
| per-IP API limiters | `s.ipMu` |
| per-IP joiner limiters | `s.relayConnMu` |
| join authorizations | `s.joinMu` |
| per-relay connection count, byte total | atomics |
| goroutine tracking | `Relay.wg`, `s.relaysWg` |

Each relay runs two accept loops (host, joiner) and one goroutine per connection. A bridge is
two `io.Copy` goroutines plus a small watchdog that closes both sockets when the relay is
cancelled, so shutdown never hangs on an in-flight game. Unauthenticated connections to the
host port are capped per relay (`MAX_CONNS_PER_RELAY + 1`) and must present the secret within a
few seconds, so a flood of silent host connections cannot spawn unbounded goroutines.

## Shutdown

On `SIGINT`/`SIGTERM` the server stops accepting HTTP requests (draining in-flight ones for up
to 30s), then closes every relay: both listeners, the tunnel, and all active bridges. It then
waits for the relay goroutines to finish.

## Ports and file descriptors

- HTTP API: 1 port (`PORT`).
- Relay: 2 ports per concurrent relay, from `RELAY_PORT_MIN`..`RELAY_PORT_MAX`.
- File descriptors: each relay holds 2 listening sockets plus 2 per active session, plus 1 per
  HTTP request. Raise `ulimit -n` for busy instances.

## Limits at a glance

| Limit | Env var | Default |
|---|---|---|
| Rooms | `MAX_ROOMS` | 100 |
| Concurrent relays | `MAX_RELAYS` | 50 |
| Joiners per relay | `MAX_CONNS_PER_RELAY` | 8 |
| Idle session timeout | `RELAY_IDLE_TIMEOUT_SECONDS` | 300 |
| Bytes per session | `RELAY_MAX_BYTES` | 1 GiB |
| Joiner connections per IP | `RELAY_CONN_RPM` | 120/min |
| API requests per IP | `RATE_LIMIT_RPM` | 200/min |
| Concurrent bcrypt ops | *(built in)* | `NumCPU` |
