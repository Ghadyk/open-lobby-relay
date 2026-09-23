# Relay protocol

This document specifies the HTTP API and the relay wire protocol, so any game can implement
an integration. The relay itself is protocol-agnostic: once a joiner's TCP connection has
been paired with a host data connection, all bytes are forwarded verbatim in both directions.

## Concepts

- **Room** — an advertised game. The server assigns it an `id` and a per-room `secret`.
- **Secret** — a 64-character hex string returned once at room creation. It authenticates the
  room owner for heartbeats, deletion, and relay allocation, and it is the host's relay
  credential. Never expose it to joiners.
- **Relay** — two TCP ports allocated for a room:
  - the **host port**, used only by the host, and
  - the **joiner port**, advertised to players in the room listing.

## HTTP API

All request and response bodies are JSON. Errors return a non-2xx status and
`{"error":"..."}`.

### `POST /rooms`

Register a room.

Request:

```json
{
  "name": "My Game",
  "mode": "1v1",
  "version": "1.2.0",
  "has_password": false,
  "password": "",
  "max_players": 4
}
```

`mode`/`version` are free-form strings (≤50 chars). `password` (≤72 chars) is required only
when `has_password` is true.

Response `201`:

```json
{
  "id": "9f...",
  "secret": "2b...",
  "name": "My Game",
  "mode": "1v1",
  "version": "1.2.0",
  "has_password": false,
  "max_players": 4
}
```

Store `id` and `secret`. The `secret` is returned only here.

### `GET /rooms`

Returns an array of rooms. `secret`, `password`, and the host's address are never included.
A room only contains `relay_host` and `relay_port` once its host tunnel is connected, so a
listed relay is always usable.

```json
[
  {
    "id": "9f...",
    "name": "My Game",
    "mode": "1v1",
    "version": "1.2.0",
    "has_password": false,
    "player_count": 1,
    "max_players": 4,
    "relay_host": "relay.example.com",
    "relay_port": 10001,
    "use_relay": true
  }
]
```

### `POST /rooms/{id}/verify`

Check a room password (send `{"password":""}` for rooms without one).

```json
{ "password": "secret" }
```

Returns `200 {"valid":true}` or `403 {"valid":false}`.

On success the caller's IP is authorized to join that room through the relay for
`JOIN_AUTH_TTL_SECONDS`. **Clients must call this before connecting to the joiner port**
when `REQUIRE_JOIN_AUTH` is on (the default).

### `POST /heartbeat`

Keep a room alive. Send every ~10 seconds; a room with no heartbeat for 25 seconds is
removed and its relay closed. Requires the room token.

```http
POST /heartbeat HTTP/1.1
Content-Type: application/json
X-Room-Token: <secret>

{ "id": "9f...", "player_count": 3, "mode": "1v1" }
```

### `DELETE /rooms/{id}`

Remove a room and its relay. Requires `X-Room-Token`. Returns `200`.

### `POST /relay`

Allocate the host and joiner ports for a room. Requires `X-Room-Token`. Re-requesting closes
the previous relay.

Request: `{ "room_id": "9f..." }`

Response `201`:

```json
{ "relay_host": "relay.example.com", "host_port": 10000, "relay_port": 10001 }
```

`relay_host` + `host_port` are for the host; `relay_host` + `relay_port` (the joiner port)
is what appears in the room listing.

## Relay wire protocol

### Host port (authenticated + framed)

The host opens two kinds of connections to `host_port`. Each begins with a 1-byte role
followed by the 64-byte room secret:

| Role byte | Meaning |
|---|---|
| `0x01` | **Tunnel** — a single, persistent control connection. |
| `0x02` | **Data** — one connection per joiner, opened on demand. |

```
+------+--------------------------------------+----------------...
| role | room secret (64 bytes, hex)          |  (bridge traffic)
+------+--------------------------------------+----------------...
```

The relay verifies the secret with a constant-time comparison and closes the connection on
mismatch.

- **Tunnel**: after authenticating, the host reads this connection. Whenever a joiner
  arrives the relay writes a single `0x01` byte, meaning *open one data connection now*. If
  the host reconnects with a new `0x01`, it replaces the previous tunnel.
- **Data**: after authenticating, the relay bridges this connection to the waiting joiner.

### Joiner port (no handshake)

Any connection to `relay_port` is a joiner. **There is no framing and no handshake** — the
client immediately speaks its game's protocol. The relay signals the host to open a data
connection and then proxies bytes between the joiner and that data connection.

Each joiner is paired with the next available host data connection. The relay only forwards
bytes; it never inspects them.

## Integration guide

### Host

1. `POST /rooms` → store `id` and `secret`.
2. Every ~10s, `POST /heartbeat` with `X-Room-Token`.
3. `POST /relay` with `X-Room-Token` → `relay_host`, `host_port`, `joiner_port`.
4. Open a persistent TCP connection to `relay_host:host_port`, write `0x01` + secret.
5. Loop reading one byte at a time. On each `0x01`, open a new TCP connection to
   `relay_host:host_port`, write `0x02` + secret, connect to your local game server
   (`127.0.0.1:<game port>`), and bridge the two.
6. On exit, `DELETE /rooms/{id}` with `X-Room-Token`.

If the tunnel connection drops, reconnect and repeat step 4; the relay replaces the old
tunnel.

### Joiner

1. `GET /rooms` and pick a room.
2. If `has_password`, prompt and `POST /rooms/{id}/verify` with the password; otherwise call
   it with an empty password. This authorizes your IP.
3. Connect to `relay_host:relay_port` and speak your game's protocol.

## Errors

| Status | Meaning |
|---|---|
| `400` | Malformed body or failed validation |
| `401` | Missing `X-Room-Token` |
| `403` | Wrong token, or a banned / unauthorized joiner |
| `404` | Unknown room |
| `429` | Rate limited |
| `503` | Lobby full, no relay ports, or server busy |
