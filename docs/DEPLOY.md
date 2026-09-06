# Deployment & Operations Guide

basic-kanban-board-mcp is packaged as a single static Go binary with an embedded SQLite engine and an embedded web interface.

One binary, one SQLite file, one port.

---

## Deployment Architectures

```
                      ┌───────────────────────────────────────────────┐
                      │              kanban serve                     │
                      │                                               │
MCP Agents  ─────────▶│  /mcp            (streamable-HTTP, bearer)   │
Read-Only Agents ────▶│  /mcp/readonly   (read-only tools, bearer)   │
Browser / UI  ───────▶│  /               (HTML + htmx + Alpine.js)   │
Browser Live  ───────▶│  /events         (SSE live card updates)     │
Monitoring  ─────────▶│  /healthz        (liveness probe)            │
                      │  /readyz         (readiness probe)           │
                      │                                               │
                      │  SQLite DB (WAL mode, busy_timeout 5000ms)    │
                      └───────────────────────────────────────────────┘
```

### The Single-Writer Discipline
SQLite is run in WAL (Write-Ahead Logging) mode with pure Go (`modernc.org/sqlite`). Only **one** `kanban serve` process may open the database file at any time.
- All AI agents and MCP clients communicate over HTTP.
- Even if an agent runs on the same machine, it should connect over HTTP (`http://127.0.0.1:8080/mcp`) rather than opening the database directly.
- The `kanban mcp --stdio` command functions as an HTTP client when `--url` is set.

---

## 1. Local Single-User Deployment (Zero Config)

The default configuration is tailored for running on a developer's workstation:

```bash
# Start the server with defaults
kanban serve
```

By default:
- **Binds to:** `127.0.0.1:8080` (safe against coffee-shop Wi-Fi exposure).
- **Data directory:** `./data/` (creates `./data/kanban.db`).
- **Auth:** Mandatory (`--auth required`).

### First-Run Startup Banner
When started with a fresh database, the server automatically generates an `admin` token and prints the connection banner:

```text
basic-kanban-board-mcp v1.0.0
  Board:        http://127.0.0.1:8080
  Agent setup:  http://127.0.0.1:8080/agent-setup
  Data:         ./data
  Bind:         127.0.0.1:8080
  Admin token (shown once, now in your logs — rotate with `kanban token rotate admin`):
    kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq
  Paste into your MCP client:
  {
    "mcpServers": {
      "kanban": {
        "type": "http",
        "url": "http://127.0.0.1:8080/mcp",
        "headers": {
          "Authorization": "Bearer kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq"
        }
      }
    }
  }
```

Open `http://127.0.0.1:8080/` in your browser. Log in using the printed admin token.

---

## 2. Remote / LAN Deployment (Behind Reverse Proxy with TLS)

Because authentication relies on HTTP bearer tokens, **transmitting tokens across a local network or the internet over cleartext HTTP is unsafe**. When hosting remotely, terminate TLS in front of kanban using a reverse proxy.

### The Two-Minute Caddy Path (Recommended)

Caddy provides automatic HTTPS, modern HTTP/2, and zero-configuration streaming for SSE.

#### 1. Caddyfile
```caddy
kanban.example.com {
    reverse_proxy 127.0.0.1:8080 {
        header_up X-Forwarded-Proto https
    }
}
```

#### 2. Start Kanban
```bash
kanban serve \
  --addr 127.0.0.1:8080 \
  --base-url https://kanban.example.com \
  --trusted-proxies 127.0.0.1/32
```

---

### Nginx Configuration

When deploying behind Nginx, you must disable proxy buffering so Server-Sent Events (`/events`) stream in real-time to browsers:

```nginx
server {
    listen 443 ssl http2;
    server_name kanban.example.com;

    ssl_certificate /etc/letsencrypt/live/kanban.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/kanban.example.com/privkey.pem;

    client_max_body_size 2M;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;

        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;

        # Mandatory for SSE live updates:
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 24h;
    }
}
```

---

## 3. Deliberate Startup Refusals

The binary enforces strict startup rules to prevent unintentional security lapses. If your configuration violates one of these rules, the server **refuses to boot** and explains how to resolve the issue.

### Refusal 1: `--auth off` on a Non-Loopback Bind (`auth_off_remote`)

```text
kanban: refusing to start: --auth=off is only allowed on loopback listeners; "0.0.0.0" is reachable from the network. Auth off + remote bind = anyone on the network can read and mutate your board. Pass --auth to override.
```

- **Why it exists:** Binding to `0.0.0.0` or a public IP without authentication would allow anyone on your local network or the internet to read, modify, or delete your kanban board.
- **How to fix:**
  - If you intend to run without authentication, bind strictly to loopback: `--addr 127.0.0.1:8080`.
  - If you are deploying on a network interface, enable auth: `--auth required`.

### Refusal 2: Remote Bind with Insecure HTTP (`insecure_http_missing`)

```text
kanban: refusing to start: listener 0.0.0.0:8080 is reachable from the network and --base-url is "http://kanban.local" (scheme http). A bearer token over plain HTTP is not protected. Put a TLS terminator in front (the two-minute Caddy path is in docs/DEPLOY.md) or pass --insecure-http to acknowledge the risk. Pass --insecure-http to override.
```

- **Why it exists:** Bearer tokens transmitted over unencrypted HTTP can be intercepted by packet sniffers or rogue access points.
- **How to fix:**
  - Place a TLS terminator (e.g. Caddy or Nginx) in front, and set `--base-url https://...`.
  - If you are operating on a fully isolated homelab VLAN or testing locally, pass `--insecure-http` to explicitly acknowledge and override this guard rail.

---

## 4. Complete CLI Flags & Environment Variables

Every command-line flag corresponds to a `KANBAN_*` environment variable. Command-line flags always take precedence over environment variables.

| Flag | Environment Variable | Default | Description |
|---|---|---|---|
| `--addr` | `KANBAN_ADDR` | `127.0.0.1:8080` | Listen address (`host:port`). Loopback by default. |
| `--data` | `KANBAN_DATA` | `./data` | Directory containing SQLite database files. |
| `--base-url` | `KANBAN_BASE_URL` | `""` | Public origin URL (e.g. `https://kanban.example.com`). Used for redirects, agent snippets, and cookie security. |
| `--auth` | `KANBAN_AUTH` | `required` | Authentication mode: `required` or `off`. `off` is only allowed on loopback. |
| `--insecure-http` | `KANBAN_INSECURE_HTTP` | `false` | Acknowledges cleartext HTTP on non-loopback listeners. |
| `--trusted-proxies` | `KANBAN_TRUSTED_PROXIES` | `""` | Comma-separated CIDRs (e.g. `127.0.0.1/32,10.0.0.0/8`) trusted to forward `X-Forwarded-*` headers. |
| `--admin-token` | `KANBAN_ADMIN_TOKEN` | `""` | Pre-defined secret for the bootstrap `admin` token. |
| `--admin-token-file` | `KANBAN_ADMIN_TOKEN_FILE`| `""` | File path containing the bootstrap admin token secret (ideal for Docker/Kubernetes secrets). |
| `--claim-ttl` | `KANBAN_CLAIM_TTL` | `1h` | Default task lease TTL. Clamped between `1m` and `24h`. |
| `--log-level` | `KANBAN_LOG_LEVEL` | `info` | Logging verbosity: `debug`, `info`, `warn`, `error`. |
| `--log-format` | `KANBAN_LOG_FORMAT` | `text` | Logging output structure: `text` or `json`. |
| `--demo` | `KANBAN_DEMO` | `false` | Seeds sample projects and tasks on first startup. |

---

## 5. Docker Deployment

A lightweight, multi-arch container image is published to GitHub Container Registry:

```bash
docker run -d \
  --name kanban \
  -p 8080:8080 \
  -v kanban-data:/data \
  -e KANBAN_BASE_URL="https://kanban.example.com" \
  -e KANBAN_TRUSTED_PROXIES="10.0.0.0/8,172.16.0.0/12,192.168.0.0/16" \
  -e KANBAN_ADMIN_TOKEN="kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq" \
  ghcr.io/ultrathinker/basic-kanban-board-mcp:latest
```

### Docker Details
- **Base image:** `gcr.io/distroless/static-debian12:nonroot` (no shell, rootless user `nonroot`).
- **Entrypoint:** `["kanban", "serve", "--addr", "0.0.0.0:8080", "--data", "/data"]`
- **Volume:** `/data` (owns `kanban.db`, `kanban.db-wal`, `kanban.db-shm`).
- **Healthcheck:** Configured using `kanban healthcheck` (probes `/healthz` directly without requiring `curl` or a shell).

### Docker Compose Example

```yaml
services:
  kanban:
    image: ghcr.io/ultrathinker/basic-kanban-board-mcp:latest
    container_name: kanban
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - kanban_data:/data
    environment:
      - KANBAN_BASE_URL=https://kanban.example.com
      - KANBAN_TRUSTED_PROXIES=127.0.0.1/32
      - KANBAN_ADMIN_TOKEN_FILE=/run/secrets/admin_token
    secrets:
      - admin_token

volumes:
  kanban_data:

secrets:
  admin_token:
    file: ./secrets/admin_token.txt
```

---

## 6. Data Directory Layout & Online Backups

### Directory Layout
Within the directory specified by `--data` (default `./data/`):
```text
data/
├── kanban.db         # Primary SQLite database file
├── kanban.db-wal     # Write-ahead log (WAL) containing recent uncommitted/checkpointed writes
└── kanban.db-shm     # Shared-memory index for concurrent readers
```

### Online Backups (`kanban backup`)

Because SQLite operates in WAL mode, **simply copying `kanban.db` while the server is running can produce a corrupted copy**.

Use the `kanban backup` CLI command, which executes SQLite's atomic `VACUUM INTO`:

```bash
# Create an atomic, point-in-time snapshot
kanban backup --dir /var/backups/kanban --data ./data
```

Output:
```text
Wrote /var/backups/kanban/20260906-180000.db
```

- Safe to run on live, concurrent production databases.
- The snapshot file is completely self-contained (checkpoints WAL into a single standard SQLite file).
- Restore is as simple as stopping `kanban serve` and copying the backup file over `data/kanban.db`.

---

## 7. Diagnostics & Maintenance

### System Diagnostics (`kanban doctor`)

The `kanban doctor` command inspects configuration, verifies network bind settings, and queries SQLite health metrics:

```bash
kanban doctor --data ./data
```

Example output:
```text
Bind address:     127.0.0.1:8080
Base URL:         https://kanban.example.com
Auth:             required
TLS required:     false
Insecure HTTP:    false
Trusted proxies:  1
Log:              info/text
Demo seed:        false
Claim TTL:        1h0m0s
Data path:        ./data/kanban.db
Journal mode:     wal
WAL bytes:        32768
Page count:       124
Migration:        3
Avg write (µs):   412
MCP endpoint:     https://kanban.example.com/mcp (Authorization: Bearer <redacted>)
```

### Healthcheck Endpoints

- **`GET /healthz`**: Quick liveness probe. Returns HTTP 200 `{"ok":true}`.
- **`GET /readyz`**: Readiness probe. Verifies database connectivity and returns HTTP 200 `{"ready":true}`.
- **CLI Healthcheck**: `kanban healthcheck --addr 127.0.0.1:8080` (exits 0 if healthy, 1 on failure).

### Hard Task Purging (`kanban task purge`)

Over MCP, only soft archiving (`task_remove`) is permitted. To permanently hard-delete tasks from storage, an administrator must use the CLI:

```bash
# Permanently deletes tasks BMB-1 and BMB-2
kanban task purge --yes BMB-1 BMB-2 --data ./data
```

### Upgrades & Migrations

- Upgrading is drop-in: download the new static binary and restart the service.
- Database migrations execute automatically during startup under an exclusive transaction lock.
- To inspect migrations without booting the server:
  ```bash
  kanban migrate --dry-run --data ./data
  ```
