# Configuration

Everything is configured through environment variables. Invalid values cause the server to
exit at startup with a clear message.

## Networking

| Variable | Default | Range | Notes |
|---|---|---|---|
| `PORT` | `8080` | — | HTTP API port |
| `PUBLIC_HOST` | *(empty)* | — | Address advertised to clients. Set to your VPS IP or domain. When empty, the server uses the request `Host` header. |
| `TRUST_PROXY` | `false` | `true`/`false` | Trust `X-Forwarded-For`. Enable **only** behind a reverse proxy you control. |

## Relay ports

| Variable | Default | Range | Notes |
|---|---|---|---|
| `RELAY_PORT_MIN` | `10000` | 1024-65535 | Lowest relay port |
| `RELAY_PORT_MAX` | `10099` | 1024-65535 | Highest relay port |

Each concurrent relay uses **two** ports, so keep
`RELAY_PORT_MAX - RELAY_PORT_MIN + 1 ≥ 2 × MAX_RELAYS`. The server logs a warning at startup
otherwise.

## Limits

| Variable | Default | Range | Notes |
|---|---|---|---|
| `MAX_ROOMS` | `100` | 1-10000 | Max advertised rooms |
| `MAX_RELAYS` | `50` | 1-10000 | Max concurrent relays |
| `MAX_CONNS_PER_RELAY` | `8` | 1-1000 | Max simultaneous joiners per relay. Set to your game's max player count. |
| `RELAY_IDLE_TIMEOUT_SECONDS` | `300` | 5-86400 | Close a bridged session after this much inactivity |
| `RELAY_MAX_BYTES` | `1073741824` | 0-… | Per-session byte cap. `0` = unlimited. **Set a value on public instances.** |
| `RELAY_CONN_RPM` | `120` | 0-1000000 | Per-IP joiner connections per minute. `0` = unlimited. Rate-based, so NAT-shared players aren't penalised. |
| `RATE_LIMIT_RPM` | `200` | 0-1000000 | API requests per minute per IP. `0` disables API rate limiting. |
| `RATE_LIMIT_BURST` | `30` | 1-100000 | API token-bucket burst |

## Security

| Variable | Default | Notes |
|---|---|---|
| `REQUIRE_JOIN_AUTH` | `true` | Require a recent `POST /rooms/{id}/verify` from the joiner's IP before the relay accepts it. This is what makes room passwords meaningful. The server logs a warning if you disable it. |
| `JOIN_AUTH_TTL_SECONDS` | `120` | How long a successful `/verify` authorizes an IP (5-3600) |
| `ALLOW_PRIVATE_HOST_IP` | `false` | Allow rooms created from private/loopback/link-local IPs. Keep `false` in production; set `true` for local testing. |
| `BCRYPT_COST` | `10` | bcrypt cost for room passwords (10-14). Higher is slower and stronger. |

## Admin

| Variable | Default | Notes |
|---|---|---|
| `ADMIN_TOKEN` | *(empty)* | When set, enables `/admin/*` behind `Authorization: Bearer <token>`. When empty, `/admin/*` returns `404`. Use a long random value (`openssl rand -hex 32`). |

## Example `.env` files

### Local development

```ini
PORT=8080
PUBLIC_HOST=localhost
ALLOW_PRIVATE_HOST_IP=true
REQUIRE_JOIN_AUTH=false
RELAY_PORT_MIN=10000
RELAY_PORT_MAX=10099
```

### Production (small community VPS)

```ini
PORT=8080
PUBLIC_HOST=lobby.example.com
RELAY_PORT_MIN=10000
RELAY_PORT_MAX=10199

MAX_ROOMS=200
MAX_RELAYS=100
MAX_CONNS_PER_RELAY=8

RELAY_IDLE_TIMEOUT_SECONDS=300
RELAY_MAX_BYTES=536870912
RELAY_CONN_RPM=120
RATE_LIMIT_RPM=200
RATE_LIMIT_BURST=30

REQUIRE_JOIN_AUTH=true
JOIN_AUTH_TTL_SECONDS=120
ALLOW_PRIVATE_HOST_IP=false
BCRYPT_COST=12

TRUST_PROXY=true
ADMIN_TOKEN=<openssl rand -hex 32>
```

## Tuning notes

- **`MAX_RELAYS` vs port range.** They must agree: `MAX_RELAYS × 2` ≥ port count. Raising
  `MAX_RELAYS` without widening the range means relay requests eventually fail with
  `503 no ports available`.
- **`RELAY_MAX_BYTES`.** A card/turn game uses a few MB per session; 512 MB is generous
  headroom while still stopping a bandwidth sink. Lower it if you want a tighter cap.
- **`RATE_LIMIT_RPM`.** Browsing + a 10s heartbeat is roughly 10 requests/minute per client.
  200/min leaves room for many players behind one NAT.
- **`BCRYPT_COST`.** Cost 10 is ~50-100 ms per hash. Room creation is rare, so 12 is fine if
  you want stronger hashes; concurrent hashing is already capped at `NumCPU`.
- **`TRUST_PROXY`.** Only enable behind a proxy you control. The server uses the rightmost
  `X-Forwarded-For` entry, so an appending proxy (like nginx's default) can't be spoofed.

See [`../SECURITY.md`](../SECURITY.md) for the reasoning behind the security defaults.
