# Admin guide

Operator endpoints for running a public instance: see what's live, remove abusive rooms, and
ban IPs.

## Enabling

Set `ADMIN_TOKEN` to a long random value (`openssl rand -hex 32`) and restart:

```bash
# Docker
echo "ADMIN_TOKEN=$(openssl rand -hex 32)" >> .env
docker compose up -d

# systemd: add it to /etc/open-lobby-relay.env and restart
```

When `ADMIN_TOKEN` is empty, every `/admin` path returns `404` — the API is simply absent.

## Authentication

Send the token as a bearer header:

```bash
export ADMIN_TOKEN=your-token
export LOBBY=http://localhost:8080

curl -H "Authorization: Bearer $ADMIN_TOKEN" "$LOBBY/admin/rooms"
```

A missing or wrong token returns `401`. The comparison is constant-time.

## Endpoints

### List rooms

```bash
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "$LOBBY/admin/rooms"
```

Returns the same shape as `GET /rooms` (secrets and password hashes stripped).

### Delete a room

Removes the room and closes its relay.

```bash
curl -s -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" "$LOBBY/admin/rooms/<room-id>"
```

`200` on success, `404` if the room is already gone.

### List relays

Live relays with their ports and current traffic:

```bash
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "$LOBBY/admin/relays"
```

```json
[
  { "room_id": "9f...", "host_port": 10000, "joiner_port": 10001,
    "connections": 2, "bytes_proxied": 84321 }
]
```

### Close all relays (kill switch)

Cancels every active session and closes every relay. Use in an emergency.

```bash
curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" "$LOBBY/admin/relays/close-all"
```

Rooms remain registered, but every room's relay advertisement is cleared, so no joiner sees a
stale relay. A host must call `POST /relay` again and reconnect its tunnel to be listed with a
relay. Delete the rooms too if you want them gone.

### List bans

```bash
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "$LOBBY/admin/bans"
```

```json
{ "203.0.113.7": "permanent", "198.51.100.4": "2026-01-01T00:00:00Z" }
```

### Ban an IP

Banned IPs are refused when creating rooms and when connecting to any relay.

```bash
# permanent
curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"ip":"203.0.113.7"}' "$LOBBY/admin/bans"

# temporary (1 hour)
curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"ip":"203.0.113.7","seconds":3600}' "$LOBBY/admin/bans"
```

`seconds` omitted or `0` means permanent. An invalid IP returns `400`.

### Unban an IP

```bash
curl -s -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"ip":"203.0.113.7"}' "$LOBBY/admin/bans"
```

`200` on success, `404` if the IP wasn't banned.

## Metrics

`GET /metrics` is **not** admin-gated (it contains no sensitive data) and is rate-limited like
the rest of the API. Scrape it for dashboards:

```bash
curl -s "$LOBBY/metrics"
```

```json
{ "rooms": 3, "relays": 2, "active_connections": 4, "bytes_proxied": 1234567 }
```

Useful alerts: `relays` near `MAX_RELAYS` (port/lobby exhaustion), and unexpected jumps in
`bytes_proxied`.

## Moderation workflows

**A room with an offensive name:**

```bash
# find it
curl -s "$LOBBY/rooms" | jq '.[] | {id, name}'
# remove it
curl -s -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" "$LOBBY/admin/rooms/<id>"
```

**A host tunnelling something that isn't a game / abusing bandwidth:**

1. Look at `GET /admin/relays` for a room with an outsized `bytes_proxied`.
2. Delete the room and ban the host IP.

**Under attack (many connections):**

1. `POST /admin/relays/close-all` to drop current sessions.
2. `POST /admin/bans` the source IPs.
3. Optionally lower `RELAY_CONN_RPM` / `RATE_LIMIT_RPM` and restart.

**A client can't join though the room is listed:** check `REQUIRE_JOIN_AUTH`. The client must
call `POST /rooms/{id}/verify` before connecting — see
[`TROUBLESHOOTING.md`](TROUBLESHOOTING.md).

## Notes

- Bans and rooms are **in memory**; they do not survive a restart.
- The admin API is separate from the room API and is only reachable if `ADMIN_TOKEN` is set;
  keep it on a trusted network or behind TLS.
- Log lines include room id/name on creation and cleanup — useful for auditing, but be mindful
  that room names are user-supplied.
