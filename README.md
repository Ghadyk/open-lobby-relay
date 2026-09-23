# open-lobby-relay

A self-hostable **lobby server + reverse-tunnel TCP relay** for online games.

It solves two problems at once:

- **Discovery** — a small HTTP API where game hosts register rooms and players browse them.
- **NAT traversal** — a TCP relay that lets players connect to a host without either side
  opening a port or revealing their IP address to the other.

The relay is deliberately **game-agnostic**: it forwards raw TCP bytes and makes no
assumption about your protocol. Joiners need no handshake at all — they just connect and
speak their game's protocol. Anything that needs to make a TCP connection reachable behind
NAT can integrate with it.

## How it works

Both the host and the joiners connect **outbound** to the relay — neither side has to accept
an incoming connection, so no port forwarding is needed.

```
 [Host]                      [open-lobby-relay]                     [Joiner]
   |                                 |                                 |
   |-- POST /rooms (register) ------>|                                 |
   |-- POST /relay (allocate) ------>|                                 |
   |-- host tunnel (host port) ----->|                                 |
   |                                 |<-- connect (joiner port) -------|
   |<-- "new joiner" signal ---------|                                 |
   |-- data conn (host port) ------->|<==== bidirectional proxy =====>|
   |-- bridge to local game server ->|                                 |
```

The relay never dials either peer, so it cannot be pointed at an arbitrary target: it only
ever bridges a joiner to the host's own data connection.

Each relay uses **two ports** on the relay host:

- the **host port**, which only the host connects to (framed and authenticated with the
  room secret), and
- the **joiner port**, advertised in the room listing, where every incoming connection is a
  joiner.

## Quick start

### Docker

```bash
cat > .env << 'EOF'
PUBLIC_HOST=your.public.host.or.ip
RELAY_PORT_MIN=10000
RELAY_PORT_MAX=10099
EOF

docker compose up -d
```

### From source

```bash
go build -o dist/open-lobby-relay .
./dist/open-lobby-relay
```

The HTTP API listens on `8080` by default.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP API port |
| `PUBLIC_HOST` | *(empty)* | Address clients use to reach the relay. Falls back to the request `Host` header. |
| `RELAY_PORT_MIN` | `10000` | Lowest relay port |
| `RELAY_PORT_MAX` | `10099` | Highest relay port. Each relay uses **2 ports**, so size the range at ≥ 2× concurrent relays. |
| `MAX_ROOMS` | `100` | Max advertised rooms |
| `MAX_RELAYS` | `50` | Max concurrent relays |
| `MAX_CONNS_PER_RELAY` | `8` | Max simultaneous joiners per relay |
| `RELAY_IDLE_TIMEOUT_SECONDS` | `300` | Close a bridged session after this much inactivity |
| `RELAY_MAX_BYTES` | `1073741824` | Per-session byte cap (0 = unlimited). Bounds bandwidth abuse. |
| `RELAY_CONN_RPM` | `120` | Per-IP joiner connection rate (0 = unlimited) |
| `TRUST_PROXY` | `false` | Trust `X-Forwarded-For` (only behind a trusted reverse proxy) |
| `ALLOW_PRIVATE_HOST_IP` | `false` | Allow rooms created from private/loopback IPs (local dev) |
| `RATE_LIMIT_RPM` | `200` | API requests per minute per IP (0 = disabled) |
| `RATE_LIMIT_BURST` | `30` | API token-bucket burst |
| `BCRYPT_COST` | `10` | bcrypt cost for room passwords (10-14) |
| `REQUIRE_JOIN_AUTH` | `true` | Require a recent `/verify` from the same IP before a joiner is accepted |
| `JOIN_AUTH_TTL_SECONDS` | `120` | How long a successful `/verify` authorizes an IP |
| `ADMIN_TOKEN` | *(empty)* | Enables the `/admin` API (Bearer auth). Unset = disabled. |

Invalid values cause the server to exit at startup.

## API

See [`PROTOCOL.md`](PROTOCOL.md) for the full specification. In short:

- `GET /rooms` — list rooms
- `POST /rooms` — register a room (returns `id` + `secret`)
- `POST /rooms/{id}/verify` — check a room password and authorize this IP to join
- `POST /heartbeat` — keep a room alive (needs `X-Room-Token`)
- `DELETE /rooms/{id}` — remove a room (needs `X-Room-Token`)
- `POST /relay` — allocate a relay (needs `X-Room-Token`)
- `GET /health`, `GET /metrics`
- `GET/POST/DELETE /admin/...` — operator controls (needs `ADMIN_TOKEN`)

## Deployment

1. Run the relay on a host with a public IP and enough open ports.
2. Set `PUBLIC_HOST` to that host's address so room listings advertise it.
3. Open the API port and the relay port range on the firewall; keep the range no larger
   than you need. With the default 100-port range you get ~50 concurrent relays.
4. Put a reverse proxy (Caddy, nginx) in front of the HTTP API for TLS, and set
   `TRUST_PROXY=true`. **Do not** proxy the relay ports — they are raw TCP.

File descriptors: each relay uses 2 listening sockets plus 2 per active session. Raise
`ulimit -n` (or `LimitNOFILE` in systemd) for busy instances.

## Security

See [`SECURITY.md`](SECURITY.md) for the threat model and operator checklist.

## License

MIT
