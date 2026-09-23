# Troubleshooting

Symptom → likely cause → fix. For the design behind these, see
[`ARCHITECTURE.md`](ARCHITECTURE.md).

## The room is listed but has no relay info

**Cause:** the host's tunnel isn't connected yet. A room only advertises `relay_host` /
`relay_port` / `use_relay` once the relay has authenticated the host tunnel.

**Fix:** make sure the host opened a connection to `relay_host:host_port` and wrote
`0x01` + the room secret. Check the server log for `host tunnel established`.
If the host never logs in, the relay port may be unreachable from the host — see
*Clients can't reach the relay* below.

## A joiner is rejected immediately

**Cause:** join authorization is on (`REQUIRE_JOIN_AUTH=true`, the default) and the joiner's
IP isn't authorized, or its HTTP IP doesn't match its relay TCP IP.

**Fix:** the client must call `POST /rooms/{id}/verify` **before** connecting. If it already
does and still fails, the joiner is likely behind a network where the API and relay use
different source addresses (rare dual-stack or CGNAT setups). Options:

- make sure `PUBLIC_HOST` is a hostname with a single address family, or an IPv4 literal, so
  clients use the same family for both the API and the relay;
- or set `REQUIRE_JOIN_AUTH=false` (you lose password enforcement at the relay).

Check the server log: it prints `rejecting unauthorized joiner <ip>`.

## `503 no ports available`

**Cause:** the relay port range is exhausted. Each relay uses two ports.

**Fix:** widen `RELAY_PORT_MAX` (and the container/firewall range) so that
`RELAY_PORT_MAX - RELAY_PORT_MIN + 1 ≥ 2 × MAX_RELAYS`. Also check for leaked relays under
`GET /admin/relays`.

## `503 max relays reached` / `503 max rooms reached`

**Cause:** `MAX_RELAYS` or `MAX_ROOMS` hit.

**Fix:** raise them (and widen the port range for relays), or remove stale rooms. Restarting
clears everything since state is in-memory.

## `503 server busy` on room creation or password check

**Cause:** the bcrypt concurrency limiter is saturated (a flood of password work).

**Fix:** normally transient. If constant, lower `BCRYPT_COST` and check for abuse via
`/metrics` and the logs.

## `429 rate limit exceeded`

**Cause:** the per-IP API rate limit was hit.

**Fix:** raise `RATE_LIMIT_RPM`/`RATE_LIMIT_BURST`, or note that many players behind one NAT
share the bucket. Responses include `Retry-After`.

## The room disappears after a while

**Cause:** heartbeats stopped. A room with no heartbeat for 25 seconds is removed and its
relay closed.

**Fix:** send `POST /heartbeat` every ~10 seconds with a valid `X-Room-Token`. Note a room id
alone is not enough — heartbeats are authenticated.

## Clients can't reach the relay

**Cause:** firewall or `PUBLIC_HOST`.

**Fix:**

- Open the relay range: `ufw allow 10000:10199/tcp` (match your `RELAY_PORT_MIN/MAX`).
- Set `PUBLIC_HOST` to your public IP or domain, otherwise the advertised address comes from
  the request `Host` header and may be wrong behind a proxy.
- Verify from another machine: `nc -vz YOUR_HOST 10000`.

## The API works locally but not from outside

**Cause:** the API is bound behind a proxy, or the firewall only allows localhost.

**Fix:** open the API port (or the proxy's `80`/`443`), and confirm the server listens on all
interfaces (it binds `:<PORT>`).

## Wrong client IP / rate limiting affects everyone

**Cause:** behind a reverse proxy without `TRUST_PROXY`, every request looks like it comes
from the proxy, so they share one rate-limit bucket and join-auth may misbehave.

**Fix:** set `TRUST_PROXY=true` **only** when the API is behind a proxy you control. The
server uses the rightmost `X-Forwarded-For` entry, which appends safely.

## The host tunnel keeps reconnecting

**Cause:** the connection to the relay is being dropped (network, firewall, or the relay
process restarting).

**Fix:** reconnect is automatic and a new `0x01` replaces the old tunnel. If it loops, check
the relay logs and whether the port is reachable. `RELAY_IDLE_TIMEOUT_SECONDS` does not apply
to the tunnel (only to bridged sessions).

## A bridged game drops mid-session

**Cause:** idle timeout or byte cap.

**Fix:** raise `RELAY_IDLE_TIMEOUT_SECONDS` (default 300s) and/or `RELAY_MAX_BYTES` (default
1 GiB). The relay log prints the session end; a quota hit logs a copy error mentioning the
quota.

## Where are the logs?

```bash
docker compose logs -f          # Docker
journalctl -u open-lobby-relay -f   # systemd
```

Room creation/cleanup, tunnel establishment, join rejection, and session start/end are all
logged with the room id.
