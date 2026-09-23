# open-lobby-relay

A self-hostable **lobby server + reverse-tunnel TCP relay** for online games. It gives your
game a public room browser and lets players connect to a host behind a router — with **no
port forwarding and no IP exposure** — without the relay knowing anything about your
protocol.

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

## Why it works

- **Both sides connect outbound.** Neither the host nor the joiner has to accept an incoming
  connection, so no port forwarding is needed and neither party learns the other's address.
- **Not an open proxy.** There is no destination parameter. A joiner is always bridged to the
  host's own data connection, which bridges to the host's own game server. The relay never
  dials a peer.
- **Protocol-agnostic.** Once paired, bytes are forwarded verbatim. Joiners need **no
  handshake** — they connect to the joiner port and immediately speak your game's protocol.
- **Self-hosted.** Run your own instance; there is no central service. All state is in
  memory and a restart simply clears the lobby.

## Features

- Room discovery (register, list, browse), heartbeats, optional passwords.
- Multi-joiner reverse-tunnel relay with host authentication and joiner gating.
- Per-IP rate limiting, request-size limits, bounded bcrypt, connection and bandwidth caps.
- Operator admin API: list/kill rooms and relays, runtime IP bans, a global kill switch.
- Health and metrics endpoints, graceful shutdown, Docker image with a healthcheck.

## Quick start

### Docker (recommended)

```bash
cat > .env << 'EOF'
PUBLIC_HOST=your.public.host.or.ip
RELAY_PORT_MIN=10000
RELAY_PORT_MAX=10099
EOF

docker compose up -d
curl http://localhost:8080/health
```

### From source

```bash
go build -o dist/open-lobby-relay .
./dist/open-lobby-relay
```

The HTTP API listens on `8080` by default. See
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for a production setup.

## Try it locally

The relay ships with a runnable example that plays both sides against a TCP echo. Run the
server with local-development settings first:

```bash
# terminal 1 — allow rooms created from localhost
ALLOW_PRIVATE_HOST_IP=true go run .
```

```bash
# terminal 2 — prints a room id
go run ./docs/example host
# terminal 3 — use the room id printed above
go run ./docs/example join <room-id>
```

## Documentation

| Guide | What's inside |
|---|---|
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | How it works: components, relay flow, the two-port model, lifecycle, concurrency |
| [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) | Deploying to a VPS: Docker, systemd, firewall, TLS, scaling, upgrades |
| [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) | Every environment variable, with tuning guidance and example `.env` files |
| [`PROTOCOL.md`](PROTOCOL.md) | The HTTP API and relay wire protocol specification |
| [`docs/INTEGRATION.md`](docs/INTEGRATION.md) | Developer guide: integrating your game (host and joiner) |
| [`docs/ADMIN.md`](docs/ADMIN.md) | Operator guide: admin API, moderation, bans, kill switch |
| [`docs/TROUBLESHOOTING.md`](docs/TROUBLESHOOTING.md) | Common problems and fixes |
| [`SECURITY.md`](SECURITY.md) | Threat model, what's protected, operator hardening checklist |

## Requirements

- A host with a public IP (a small VPS is plenty).
- One open TCP port for the API (`8080`) and a range for relays. Each relay uses **two**
  ports, so a 100-port range supports ~50 concurrent games.
- Go 1.21+ to build from source.

## License

MIT — see [`LICENSE`](LICENSE).
