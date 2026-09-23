# Integration guide

How to make your game work with `open-lobby-relay`. The relay forwards raw TCP bytes, so your
game keeps its own protocol — you only add a small amount of host-side plumbing and a couple
of HTTP calls.

## How it fits

```
 Host game process                              Joiner game process
 ┌────────────────────┐                         ┌────────────────────┐
 │ your game server   │                         │ your game client   │
 │ (listens on :PORT) │                         │                    │
 └─────────┬──────────┘                         └─────────┬──────────┘
           │ bridge                                       │ normal connect
 ┌─────────┴──────────┐                         ┌─────────┴──────────┐
 │ host tunnel client │                         │ (no special code)  │
 └─────────┬──────────┘                         └─────────┬──────────┘
           │ outbound                                     │ outbound
           ▼                                              ▼
      relay host port                                relay joiner port
```

- **Host**: register a room, run a persistent tunnel, and on demand bridge a data connection
  to your local game server. Roughly 100 lines in any language.
- **Joiner**: call `POST /rooms/{id}/verify` and then connect to the relay address instead of
  the host's address. Your existing client code is otherwise unchanged.

The full wire format is in [`../PROTOCOL.md`](../PROTOCOL.md). A complete, runnable reference
implementation is in [`example/main.go`](example/main.go).

## Host integration

### 1. Register the room

```http
POST /rooms
Content-Type: application/json

{ "name": "Alice's Game", "mode": "1v1", "version": "1.2.0", "max_players": 4 }
```

Store the returned `id` and `secret`. Never send the `secret` to players.

### 2. Heartbeat

Every ~10 seconds, `POST /heartbeat` with `X-Room-Token: <secret>` and
`{ "id": ..., "player_count": N }`. If you stop for 25 seconds the room and relay are removed.

### 3. Allocate the relay

```http
POST /relay
X-Room-Token: <secret>

{ "room_id": "<id>" }
```

Response:

```json
{ "relay_host": "relay.example.com", "host_port": 10000, "relay_port": 10001 }
```

`host_port` is yours; `relay_port` is what appears in the room listing for joiners.

### 4. Open the tunnel

Connect to `relay_host:host_port` and write the role byte `0x01` followed by the 64-byte
secret. Keep this connection open.

### 5. Serve joiners

Read one byte at a time from the tunnel. Each `0x01` means *a joiner is waiting*. For each
one:

1. connect again to `relay_host:host_port`, write `0x02` + secret;
2. connect to your game server on `127.0.0.1:<game port>`;
3. copy bytes both ways between the two connections.

```go
// Sketch — see docs/example/main.go for the full version.
tunnel := dial(relayHost, hostPort)
tunnel.Write(append([]byte{0x01}, secret...))

for {
    if _, err := io.ReadFull(tunnel, buf[:1]); err != nil { return }
    if buf[0] != 0x01 { continue }
    go func() {
        data := dial(relayHost, hostPort)
        data.Write(append([]byte{0x02}, secret...))
        game := dial("127.0.0.1", gamePort)
        go io.Copy(data, game)
        io.Copy(game, data)
    }()
}
```

### 6. Clean up

On exit, `DELETE /rooms/{id}` with `X-Room-Token`.

### Reconnection

If the tunnel drops, reconnect and repeat step 4. A new `0x01` replaces the previous tunnel,
so recovery is automatic.

## Joiner integration

1. `GET /rooms` and pick a room (filter by `mode`/`version`, show `player_count`/`max_players`,
   note `has_password`).
2. If the room has a password, prompt for it. Call `POST /rooms/{id}/verify` with
   `{ "password": "..." }` (or an empty password for open rooms). **This step is required**
   when `REQUIRE_JOIN_AUTH` is on, because it authorizes the joiner's IP.
3. Connect to `relay_host:relay_port` and run your normal client handshake.

Nothing about the relay is visible to your protocol.

## Language notes

- The host tunnel needs **outbound TCP** plus the ability to read one byte at a time from a
  socket. Every language can do this.
- The bridge is a pair of copy loops (e.g. Go `io.Copy`, Node `pipe`, Python `shutil.copyfileobj`,
  Java two threads). Close both sockets when either side finishes.
- The HTTP calls are plain JSON; no special client library is required.

## Testing locally

Run the server with development settings and use the bundled example:

```bash
# .env: PUBLIC_HOST=localhost, ALLOW_PRIVATE_HOST_IP=true
go run .
go run ./docs/example host           # prints a room id
RELAY_SERVER=http://localhost:8080 go run ./docs/example join <room-id>
```

You should see the joiner's lines echoed back through the relay.

## Room fields reference

| Field | Meaning |
|---|---|
| `id` | Server-generated room id |
| `name` | Display name |
| `mode` | Free-form game mode (≤50 chars) |
| `version` | Free-form build/version (≤50 chars) — good for compatibility checks |
| `has_password` | Whether a password is required |
| `player_count` / `max_players` | Current and maximum players |
| `relay_host` / `relay_port` | Joiner endpoint; present only once the host tunnel is up |
| `use_relay` | True once the relay is ready |

## Checklist

- [ ] Register and heartbeat, and delete the room on exit.
- [ ] Allocate the relay and open the tunnel with `0x01` + secret.
- [ ] Handle `0x01` signals by opening `0x02` data connections and bridging to the game server.
- [ ] Reconnect the tunnel on failure.
- [ ] Call `/verify` before joining.
- [ ] Never expose the room `secret` to clients.
