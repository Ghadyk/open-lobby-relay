# Security

This document describes what the relay protects against by design, what it cannot, and how
to run it safely.

## Prevented by design

- **Not an open proxy.** There is no destination parameter anywhere. A joiner is always
  bridged to the host's own data connection, and the host's data connection is bridged to
  its own local game server. An attacker cannot point the relay at a third-party host or an
  internal service.
- **No reflection or amplification.** A joiner only counts after a completed TCP handshake
  with the relay, so source addresses cannot be spoofed, and the relay copies bytes 1:1
  rather than amplifying them.
- **No tunnel hijack or data injection.** Host connections are authenticated with the
  per-room secret using a constant-time comparison. Knowing a relay port is not enough.
- **Room passwords are enforced at the relay.** With `REQUIRE_JOIN_AUTH=true` (default) a
  joiner's IP must have passed `POST /rooms/{id}/verify` recently, so a client cannot skip
  the password check and connect straight to the joiner port.
- **Bounded resources.** Concurrency (rooms, relays, connections, pending host handshakes),
  request bodies, per-IP API rate, per-IP joiner connection rate, per-session bytes, idle time,
  and bcrypt work are all capped. Unauthenticated host connections must present the secret
  within a few seconds and are limited per relay.

## Not preventable (know the trade-off)

- **A host can bridge to any local port.** Because the relay is protocol-agnostic, a host
  chooses what its data connections bridge to. It could tunnel a non-game protocol. There is
  no way to detect this without inspecting traffic, which would defeat the design.
  **If you operate a public instance, this is your moderation problem**: publish terms, keep
  an eye on `/metrics`, and use the admin API / kill switch.
- **A determined host or joiner can abuse their own bandwidth.** Per-session byte caps and
  per-IP limits bound this, but they trade off against legitimate use.

## Do not bake secrets into game clients

Any secret shipped inside a client — an "app key," a shared password, a signing key — is
extractable (decompilation, memory inspection, or simply observing the wire). A shared client
secret provides no real protection, cannot be kept secret, and cannot be rotated without
shipping a new build.

The only meaningful credentials are **server-issued, per-room, expiring capabilities**: the
room `secret`, and the per-IP join authorization. Keep the operator token (`ADMIN_TOKEN`)
server-side only and never ship it.

## Operator checklist

- **TLS**: terminate HTTPS for the API at a reverse proxy (Caddy, nginx). Set
  `TRUST_PROXY=true` only when the API is behind a proxy you control. Never proxy the relay
  ports.
- **Firewall**: expose only the API port (or the proxy) and the relay range you need. Each
  relay uses two ports, so size `RELAY_PORT_MAX - RELAY_PORT_MIN + 1` at ≥ 2 × expected
  concurrent relays.
- **Least privilege**: run as a non-root user (the provided Dockerfile does), with CPU and
  memory limits and a raised file-descriptor limit.
- **Defaults**: keep `ALLOW_PRIVATE_HOST_IP=false` and `REQUIRE_JOIN_AUTH=true`. The server
  logs a warning if join auth is disabled.
- **Set `ADMIN_TOKEN`** to a long random value and use the admin API to remove rooms/relays
  and ban IPs. Without it, `/admin` is disabled.
- **Monitor**: scrape `/metrics` (rooms, relays, active connections, bytes) and log room
  creation/cleanup. Watch for port exhaustion and abnormal byte counts.
- **Abuse response**: `POST /admin/relays/close-all` is a kill switch; `POST /admin/bans`
  bans an IP.
- **Least data**: the server keeps no persistent state. Restarting clears everything.

## Reporting

Open an issue on the repository for security-relevant bugs. Do not include secrets or
personal data.
