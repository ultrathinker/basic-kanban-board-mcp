# Security Architecture & Trust Model

basic-kanban-board-mcp coordinates multiple AI coding agents across multiple machines, with human developers viewing and directing work in a browser.

This document describes the security trust model, cryptographic guarantees, and transport defenses.

---

## 1. The Trust Model

### Actor Identity Comes from the Bearer Token
In basic-kanban-board-mcp, **there is no `actor` parameter on any MCP tool, REST endpoint, or web form**.

- Every bearer token is created with a unique `name` (e.g. `claude@workstation`, `codex@server`, `alice`).
- When a client authenticates with a token, that token's name becomes the immutable `actor` identity for that entire request.
- All task creations (`created_by`), task mutations (`updated_by`), lease claims (`claimed_by`), audit notes (`author`), and event streams are attributed directly to the authenticated token.
- An agent cannot forge or impersonate another agent's identity.

### Multi-Agent, Single-Tenant Architecture
The server is designed for single-tenant multi-agent operations:
- A developer, team, or homelab runs a single `kanban` instance.
- Agents running on local machines, remote servers, or CI environments connect using individually provisioned bearer tokens.
- Tokens can be restricted to specific projects (`project_keys`), allowing separation between concurrent initiatives.

---

## 2. Bearer Tokens & Cryptographic Invariants

### 256-Bit Cryptographic Entropy
Every token secret is generated using Go's standard `crypto/rand`:
- Secrets contain 32 random bytes (256 bits of entropy).
- Encoded using lowercase Base32 (unpadded) with a `kbn_` prefix: `kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq`.
- Base32 avoids shell escaping and URL encoding ambiguities.

### Unsalted SHA-256 Storage
The database stores only the SHA-256 hash of the token secret, never the plaintext secret.

> **Cryptographic Invariant:** Unsalted hashing is safe *specifically and only* because the token secret contains 256 bits of pure cryptographic entropy. There are no rainbow tables or precomputed dictionaries for a 2^256 keyspace. A human-chosen password requires salting and slow hashing (e.g. bcrypt/argon2); a 256-bit random secret does not.

### Constant-Time Hash Verification
When verifying incoming credentials (`Authorization: Bearer kbn_...` or `X-API-Key: kbn_...`):
1. The server computes the candidate secret's SHA-256 digest.
2. It looks up the token in SQLite by exact hash match.
3. It performs a constant-time comparison (`crypto/subtle.ConstantTimeCompare`) against the stored hash as defense-in-depth against timing side-channels.
4. Unknown tokens return HTTP 403 Forbidden without leaking whether the token format was valid.

### Credential Redaction in Logs
- The server automatically strips credentials from log messages and diagnostics.
- `Authorization: Bearer kbn_...` and `X-API-Key: kbn_...` headers are redacted to `kbn_…(redacted)`.
- Session cookies (`kanban_session=...`) are sanitized in access logs.
- The `kanban doctor` command redacts bearer tokens and endpoint URLs by default.

---

## 3. Scopes & Authorization

Token capabilities are governed by three hierarchical scopes:

```
┌────────────────────────────────────────────────────────┐
│                        admin                           │
│  ┌──────────────────────────────────────────────────┐  │
│  │                     write                        │  │
│  │  ┌────────────────────────────────────────────┐  │  │
│  │  │                  read                      │  │  │
│  │  └────────────────────────────────────────────┘  │  │
│  └──────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────┘
```

- **`read`**: Grants access to read the board (`board_get`), fetch tasks (`task_get`), peek ready work (`task_next` with `action: "peek"`), and request ephemeral SSE tickets. Mutations are blocked.
- **`write`**: **Implies `read`**. Permits full task lifecycle operations: create tasks (`task_create`), update task fields and status (`task_update`), claim and start work (`task_next` with `action: "start"` or `"claim"`), manage leases (`task_claim`), add/remove dependency links (`task_link`), and archive tasks (`task_remove`).
- **`admin`**: **Implies `write` and `read`**. Unlocks administrative operations:
  - Minting, rotating, and revoking tokens (`kanban token` / web admin UI).
  - Creating and modifying project definitions and column layouts (`project_upsert`).
  - Overriding workflow constraints (WIP limits, dependencies, strict acceptance) via `force: true` with a mandatory reason.
  - Stealing active leases held by other agents (`task_claim` with `force: true`).
  - Hard-deleting tasks via CLI (`kanban task purge`).

### Project Scoping
Tokens can be bounded to an explicit list of project keys (`project_keys: ["BMB", "INFRA"]`). When set, any read or mutation targeting another project is refused with `code: "forbidden"`.

---

## 4. Web Sessions, CSRF, and Bearer Exemption

The web interface (`http://127.0.0.1:8080/`) uses browser cookie sessions.

### Session Cookies
- Cookie name: `kanban_session`.
- Properties: `HttpOnly`, `SameSite=Lax`.
- The `Secure` attribute is automatically enabled when `--base-url` is HTTPS or when a trusted reverse proxy forwards `X-Forwarded-Proto: https`.
- Lifespans: 7 days idle timeout, 30 days absolute timeout.

### Cross-Site Request Forgery (CSRF) Protection
Browser-driven state mutations (form POSTs and htmx fragment calls) are protected by a double-submit CSRF cookie pattern:
1. On page load, the server issues an opaque, cryptographically random CSRF token via cookie (`kanban_csrf`).
2. HTML templates include this token in a hidden `<input type="hidden" name="csrf_token" value="...">` field or in the `X-CSRF-Token` header.
3. Every mutating UI endpoint (`/fragments/*`, `/admin/*`, `/logout`) validates that the request value matches the cookie value using constant-time comparison.

### Bearer Token Exemption (Deliberate Design)
API requests authenticated via `Authorization: Bearer ...` or `X-API-Key: ...` are **exempt from CSRF validation by design**.

> **Why bearer calls are exempt:**
> CSRF attacks exploit ambient browser credentials (cookies or HTTP basic auth) that browsers automatically attach to cross-origin requests.
>
> Browsers **never** automatically attach custom headers such as `Authorization: Bearer ...` to cross-origin requests. A third-party malicious website cannot induce a user's browser to send a custom bearer header without explicit CORS preflight permission (`OPTIONS`), and **CORS is disabled by default** in basic-kanban-board-mcp. A client holding its own bearer token cannot be CSRF'd.

---

## 5. Transport Security & Network Hardening

### Ephemeral SSE Connection Tickets (`/events/ticket`)
Browser EventSource cannot set custom HTTP request headers. Placing long-lived bearer tokens into URL query strings (`/events?token=kbn_...`) would leak tokens into reverse-proxy access logs, browser histories, and referrer headers.

Instead, agents and browsers use short-lived tickets:
1. The client sends an authenticated POST request:
   ```http
   POST /events/ticket
   Authorization: Bearer kbn_...
   ```
2. The server responds with a random, single-use ticket:
   ```json
   { "ticket": "7q4e9..." }
   ```
3. The client connects to the SSE stream using the ticket:
   ```http
   GET /events?ticket=7q4e9...
   ```
4. The ticket expires in **60 seconds** and is atomically consumed on first use.

### 1 MB Request Body Cap
To protect against memory exhaustion attacks, all authenticated routes enforce a strict **1 MB body cap** (`MaxRequestBodyBytes = 1 << 20`):
1. **Pre-flight rejection:** If `Content-Length` exceeds 1 MB, the server returns HTTP 413 Payload Too Large immediately without reading bytes.
2. **Streaming limit:** `http.MaxBytesReader` bounds the stream during ingestion to protect against chunked-encoding overruns.

### Rate Limiting (Token Bucket)
Rate limits are enforced at the HTTP middleware layer:
- **API Token Rate Limit:** **600 requests per minute** per token.
  *Important:* A 100-item batch write (`task_create` or `task_update`) counts as **1 request**. Batching is rewarded, not penalized.
- **Login Rate Limit:** **20 attempts per minute** per client IP on `/login`.
- **Memory Bounding:** The rate limiter tracks keys in an in-memory cache capped at 4 096 entries, automatically evicting oldest expired buckets to prevent memory starvation from IP spraying.

### Trusted Proxy Configuration
When deployed behind a load balancer or reverse proxy (e.g. Caddy, Nginx, Cloudflare), pass the proxy CIDR to `--trusted-proxies`:

```bash
kanban serve --trusted-proxies 127.0.0.1/32,10.0.0.0/8
```

- `X-Forwarded-For` is parsed **right-to-left**; untrusted IP hops are discarded.
- `X-Forwarded-Proto` is trusted only when received directly from a trusted proxy IP.
- **Hard Rule:** Proxy headers are used *only* for logging, rate-limiting keys, and cookie security flags. **Proxy headers are never used for authentication or authorization decisions.**

### Content Security Policy (CSP) & Markdown Sanitization
- **Strict CSP:** The web server sends:
  ```text
  Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
  ```
  Inline scripts are completely prohibited. All JavaScript is vendored and served locally from `/static/vendor/`.
- **Clickjacking Defense:** `X-Frame-Options: DENY` and `frame-ancestors 'none'`.
- **MIME Sniffing Prevention:** `X-Content-Type-Options: nosniff`.
- **Markdown Sanitization:** Task bodies and notes are rendered via `goldmark` and sanitized through `bluemonday.UGCPolicy()` before insertion into the DOM.

---

## 6. Vulnerability Reporting

If you identify a security vulnerability in basic-kanban-board-mcp:

1. **Do not open a public GitHub issue.**
2. Send an email with reproduction steps and proof-of-concept to the repository maintainer (see `README.md` / repository profile for contact details).
3. Security reports are reviewed promptly, and fixes will be tagged and released in accordance with responsible disclosure practices.
