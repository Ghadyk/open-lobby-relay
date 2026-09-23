# Deployment

A complete guide to running `open-lobby-relay` in production. A small VPS (1 vCPU, 512 MB) is
enough for a community-sized lobby.

## Requirements

- A host with a **public IP** (or a hostname pointing at one).
- Docker + Docker Compose (recommended), or Go 1.27+ to build from source.
- Open TCP ports: the API port and the relay port range.
- Optional: a domain name and a reverse proxy for TLS.

> **Two ports per relay.** Each concurrent game uses two ports from
> `RELAY_PORT_MIN..RELAY_PORT_MAX`. A 100-port range (the default) supports ~50 simultaneous
> games. Size `RELAY_PORT_MAX - RELAY_PORT_MIN + 1` at **≥ 2 × expected concurrent relays**.

## Get the code

```bash
git clone https://github.com/YOUR_USERNAME/open-lobby-relay.git
cd open-lobby-relay
```

## Option A — Docker Compose (recommended)

1. Create a `.env` file next to `docker-compose.yml`:

   ```bash
   cat > .env << 'EOF'
   PUBLIC_HOST=203.0.113.10
   RELAY_PORT_MIN=10000
   RELAY_PORT_MAX=10199
   ADMIN_TOKEN=change-me-to-a-long-random-string
   EOF
   ```

   - `PUBLIC_HOST` is the address clients use to reach the relay. Set it to your VPS IP or
     domain. Without it the server falls back to the request `Host` header.
   - Generate the admin token with `openssl rand -hex 32`.

2. Make sure `docker-compose.yml`'s published port range matches your `.env`:

   ```yaml
   ports:
     - "8080:8080"
     - "10000-10199:10000-10199"
   ```

3. Start it:

   ```bash
   docker compose up -d --build
   ```

4. Verify:

   ```bash
   curl http://localhost:8080/health
   docker compose logs -f
   ```

## Option B — Docker (manual)

```bash
docker build -t open-lobby-relay .
docker run -d --name relay \
  --restart unless-stopped \
  -p 8080:8080 \
  -p 10000-10199:10000-10199 \
  -e PUBLIC_HOST=203.0.113.10 \
  -e RELAY_PORT_MIN=10000 \
  -e RELAY_PORT_MAX=10199 \
  -e ADMIN_TOKEN=change-me \
  --memory 256m \
  open-lobby-relay
```

## Option C — Bare metal with systemd

```bash
# build
go build -o /usr/local/bin/open-lobby-relay .

# dedicated user
sudo useradd --system --no-create-home --shell /usr/sbin/nologin relay
```

Create `/etc/open-lobby-relay.env`:

```ini
PUBLIC_HOST=203.0.113.10
RELAY_PORT_MIN=10000
RELAY_PORT_MAX=10199
ADMIN_TOKEN=change-me-to-a-long-random-string
```

Create `/etc/systemd/system/open-lobby-relay.service`:

```ini
[Unit]
Description=open-lobby-relay
After=network-online.target
Wants=network-online.target

[Service]
User=relay
Group=relay
EnvironmentFile=/etc/open-lobby-relay.env
ExecStart=/usr/local/bin/open-lobby-relay
Restart=on-failure
RestartSec=2
# relay sessions need file descriptors; raise the limit
LimitNOFILE=65535
# light hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now open-lobby-relay
sudo systemctl status open-lobby-relay
```

## Firewall

Open only the API port and the relay range.

```bash
# ufw
sudo ufw allow 8080/tcp
sudo ufw allow 10000:10199/tcp
sudo ufw enable

# firewalld
sudo firewall-cmd --permanent --add-port=8080/tcp
sudo firewall-cmd --permanent --add-port=10000-10199/tcp
sudo firewall-cmd --reload
```

If you put a reverse proxy in front for TLS, open `80`/`443` instead of `8080` and keep
`8080` closed to the public. **Never** proxy the relay ports — on the relay they are raw TCP.

## TLS with a reverse proxy

The API speaks plain HTTP; terminate TLS at a proxy. Do **not** enable `TRUST_PROXY` unless the
API is behind a proxy you control (it makes the server trust `X-Forwarded-For`).

### Caddy

`Caddyfile`:

```
lobby.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Run Caddy, set `TRUST_PROXY=true`, and point clients at `https://lobby.example.com`.

### nginx

```nginx
server {
    listen 443 ssl;
    server_name lobby.example.com;

    # ssl_certificate / ssl_certificate_key ...

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

With nginx's `$proxy_add_x_forwarded_for` the header is appended, so a client-supplied value
ends up first. The server deliberately reads the **rightmost** entry (the one your proxy
added), so this is safe.

## File descriptor limits

Each relay holds 2 listening sockets plus 2 per active session. For Docker add `--ulimit
nofile=65535:65535`; for systemd it is the `LimitNOFILE` line above. Check the host limit with
`ulimit -n`.

## Sizing and scaling

- **Bandwidth** is the real constraint — every byte flows through the relay. Budget for it.
- **Memory**: ~10 MB base plus ~100 KB per active session.
- **CPU**: I/O bound; the relay only copies bytes.
- Scale vertically first. One medium VPS handles a large community. If you outgrow a single
  host, run multiple instances behind a load balancer for the API and shard the relay port
  ranges — but note the API is stateful (in-memory), so route each room's joiners to the same
  instance that allocated its relay (e.g. by `relay_host`).

## Upgrading

```bash
git pull
docker compose up -d --build   # or: go build && systemctl restart open-lobby-relay
```

A restart clears all rooms and relays; hosts simply re-register on their next heartbeat.

## Logs and monitoring

```bash
docker compose logs -f          # or: journalctl -u open-lobby-relay -f
```

Scrape `GET /metrics` (JSON) for graphs and alerts:

```json
{ "rooms": 3, "relays": 2, "active_connections": 4, "bytes_proxied": 1234567 }
```

Watch for port exhaustion (`relays` near `MAX_RELAYS`) and abnormal `bytes_proxied`.

## Verifying the deployment

From another machine:

```bash
curl http://YOUR_HOST:8080/health
curl http://YOUR_HOST:8080/rooms
```

Then run the bundled example (see the [README](../README.md#try-it-locally)) against
`RELAY_SERVER=http://YOUR_HOST:8080` to confirm a full relay round trip.

## Next steps

- Tune limits and security: [`CONFIGURATION.md`](CONFIGURATION.md), [`../SECURITY.md`](../SECURITY.md)
- Operator tooling: [`ADMIN.md`](ADMIN.md)
- Something's wrong: [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md)
