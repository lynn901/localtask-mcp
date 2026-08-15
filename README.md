# localtask-mcp

A Go MCP (Model Context Protocol) server exposing host-management capabilities —
shell execution, file read/write, directory listing, system info, process/disk/memory
stats, and kubectl — as MCP tools.

It is a Go reimplementation of the original `server.py` (a zero-dependency HTTP JSON
API), now exposed through the standard MCP protocol so any MCP-compatible client
(Claude Code, Claude Desktop, custom clients) can drive it.

- **Language**: Go 1.21+ (built on 1.26), zero non-test deps beyond the official MCP SDK
- **SDK**: [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) v1.7
- **Transports**: stdio (default, for local clients) + streamable HTTP (`-http`)

## Build

```bash
# requires Go on PATH (see "Install Go" below if you don't have it)
go build -o localtask-mcp .

# verify
./localtask-mcp -h
```

Run tests (integration test launches the server over stdio and exercises every tool):

```bash
go test -v -run TestTools ./...
```

## Tools

| Tool         | Description                                            | Key params |
|--------------|--------------------------------------------------------|------------|
| `exec`       | Run a shell command via `sh -c`; returns exit/stdout/stderr | `command` (req), `timeout` (def 30) |
| `read`       | Read a UTF-8 text file                                 | `path` (req) |
| `write`      | Write UTF-8 text to a file (overwrites; creates dirs)  | `path` (req), `content` (req) |
| `write_bytes`| Write binary to a file (overwrites; creates dirs)     | `path` (req), `contentHex` xor `contentBase64` |
| `list`       | List directory contents (`type\tname\tsize` per line) | `path` (def `.`) |
| `info`       | Host info: hostname, platform, time, user, cwd        | — |
| `ps`         | Top 20 processes by memory, or filter by name         | `name` (optional, sanitized) |
| `df`         | Disk usage (`df -h`)                                  | — |
| `mem`        | Memory usage (`free -h`)                               | — |
| `k8s`        | Run a kubectl command (def `kubectl get nodes`)      | `command` (def), `timeout` (def 30) |
| `download`   | Read a file as base64 (binary-safe)                   | `path` (req) |

Tool schemas (required vs optional fields) are derived from the Go struct tags:
fields without `omitempty` are **required** and are validated by the SDK before the
handler runs. The `ps` tool's `name` argument is whitelisted with
`^[a-zA-Z0-9._-]+$` to prevent shell injection through `grep`.

## Run

### stdio (default — for local MCP clients)

```bash
./localtask-mcp
```

Reads JSON-RPC from stdin, writes to stdout, logs to stderr. This is the mode
MCP clients spawn automatically.

### streamable HTTP

HTTP mode requires **multi-key bearer auth** (see [Authentication](#authentication))
and optionally runs over **TLS** (see [TLS](#tls)).

```bash
# plain HTTP, two keys with labels
./localtask-mcp -http 127.0.0.1:8011 -tokens "$(openssl rand -hex 32):alice,$(openssl rand -hex 32):bob"

# HTTPS with an auto-generated self-signed cert (prints fingerprint to stderr)
./localtask-mcp -http 127.0.0.1:8443 -tls-selfsigned -tokens "$TOKEN:alice"

# HTTPS with your own cert files (e.g. Let's Encrypt)
./localtask-mcp -http 127.0.0.1:8443 -cert /path/fullchain.pem -key /path/privkey.pem -tokens "$TOKEN:alice"

# stateless: no sessions, one-shot per request (GET/DELETE return 405)
./localtask-mcp -http 127.0.0.1:8443 -tls-selfsigned -tokens "$TOKEN:alice" -stateless
```

MCP endpoint: `POST /mcp` (`Authorization: Bearer <key>`, `Accept: application/json, text/event-stream`).
Health/identity: `GET /` (also requires a valid key).

`MaxRequestBodyBytes` is disabled (`-1`) so large file writes / command outputs are
not rejected with HTTP 413 — appropriate for trusted local use only.

## Authentication

| Transport | Auth |
|-----------|------|
| stdio     | **none** — trusts the local process that spawned it |
| HTTP      | **multi-key bearer** (required) |

HTTP requests must carry `Authorization: Bearer <key>`. Keys are loaded once at
startup from any of these (later sources are appended, all are accepted):

| Source | Flag / env | Format |
|--------|------------|--------|
| inline | `-tokens` flag or `MCP_TOKENS` env | comma-separated `key` or `key:label` |
| file   | `-keys <path>` | JSON array of `{"key","label"?,"revoked"?}` |
| legacy | `MCP_TOKEN` env (only if nothing else set) | single bare token, backward compat |

Comparison is constant-time (`crypto/subtle`, all keys compared every request so
the count/identity isn't leaked by timing). Missing/invalid keys get `401` with a
`WWW-Authenticate: Bearer` challenge. Revoked keys (`"revoked":true` in the JSON
file) are skipped at load. If HTTP mode starts with zero active keys, it refuses
(exit 2).

Generate strong keys:

```bash
openssl rand -hex 32   # 256-bit hex secret per key
```

`keys.json` example (one valid, one revoked):

```json
[
  {"key":"a1b2...","label":"alice"},
  {"key":"c3d4...","label":"ci-bot","revoked":true}
]
```

> Each key grants **full host control** (arbitrary shell + any file R/W as the
> server's OS user). Treat keys like root credentials. To rotate a key, edit
> `keys.json` and restart (no hot reload).

## Encrypting keys.json (embedded key)

To avoid leaving bearer keys in a plaintext file, you can encrypt `keys.json`
with **AES-256-GCM**, where the decryption key is baked into the binary at build
time (not on disk, not in an env var). **The plaintext source is deleted once
the ciphertext is verified to round-trip** — keep your own backup of
`keys.json` elsewhere before encrypting.

**Threat model & limits.** This protects against the keys file being read by
anyone who has **the file but not the binary** — e.g. backups, snapshots, git
leaks, or another same-user process that reads the file. It does **not** protect
against someone who has **both the binary and the file** (the embed key can be
extracted from the binary with `strings`/disassembly), nor against root/runtime
memory dumps. For a stronger model, use a TLS-terminating reverse proxy + short
keys from a secrets manager instead.

### Setup (three steps)

```bash
# 1. Build the binary WITH the embed key injected via -ldflags.
EK="$(openssl rand -hex 32)"   # 64 hex chars = 32 bytes for AES-256
echo "embedKey=$EK (keep this for rebuilding)"  # not stored anywhere else
go build -ldflags "-X main.embedKey=$EK" -o localtask-mcp .

# 2. Encrypt keys.json → keys.json.enc (the plaintext source is then DELETED).
#    Keep your own backup of keys.json first — this directory will hold no
#    plaintext after this step.
./localtask-mcp -encrypt-keys keys.json keys.json.enc

# 3. Run the server; -keys transparently decrypts using the baked-in key.
./localtask-mcp -http 127.0.0.1:8443 -tls-selfsigned -keys keys.json.enc
```

`-encrypt-keys` forms:
- `./localtask-mcp -encrypt-keys <in>` — encrypts `<in>` **in place** (overwrites with ciphertext). The path is kept (now ciphertext), not removed.
- `./localtask-mcp -encrypt-keys <in> <out>` — reads plaintext `<in>`, writes ciphertext `<out>`, then **deletes** `<in>` after verifying `<out>` round-trips. This is the form for a systemd deployment (edit `keys.json`, produce `keys.json.enc`, the plaintext is consumed).
- `-keep` — in the two-arg form, do **not** delete `<in>` (use it to refresh `<out>` from a master `keys.json` you keep elsewhere).
- Re-encrypting an already-encrypted file is refused.
- The written ciphertext is decrypted back and compared to the original before the plaintext is removed, so a source is never deleted in favor of an undecryptable file.

If you rebuild without `-ldflags` (no embed key), the server refuses to decrypt
an encrypted file and exits. A different embed key fails the GCM tag check
(=decryption error).

### Rotating bearer keys (without changing the embed key)

```bash
# Edit your own plaintext copy of keys.json, then re-encrypt to keys.json.enc:
./localtask-mcp -encrypt-keys keys.json keys.json.enc
# Restart the server (e.g. systemctl --user restart localtask-mcp).
```

### Changing the embed key

```bash
EK2="$(openssl rand -hex 32)"
go build -ldflags "-X main.embedKey=$EK2" -o localtask-mcp .
# Re-encrypt every keys file with the NEW binary:
./localtask-mcp -encrypt-keys keys.json keys.json.enc
```

### If the server can't decrypt at startup

When `-keys` points to a missing or undecryptable file, the server prints an error
and exits non-zero. Under systemd (`Restart=on-failure`) it will keep restarting
and failing until you provide a valid encrypted keys file — no service is exposed
in that state.

## Deployment with systemd (HTTP, no TLS, encrypted keys)

A user-level systemd unit runs the server on loopback HTTP, loading keys from the
encrypted `keys.json.enc`. No sudo needed.

The unit file lives **in this project** (`localtask-mcp.service`) and is the
single source of truth — the deployed copy is a **symlink** to it, so editing
the file here and running `daemon-reload` is all you ever do.

`localtask-mcp.service` (in the project root):

```ini
[Unit]
Description=localtask MCP server (HTTP, no TLS, encrypted keys)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/home/yuan/localtask
ExecStart=/home/yuan/localtask/localtask-mcp -http 127.0.0.1:8011 -keys /home/yuan/localtask/keys.json.enc
Restart=on-failure
RestartSec=3s

[Install]
WantedBy=default.target
```

Install and run (the symlink keeps the project file as the source of truth):

```bash
mkdir -p ~/.config/systemd/user
ln -sf "$PWD/localtask-mcp.service" ~/.config/systemd/user/localtask-mcp.service
systemctl --user daemon-reload
systemctl --user enable --now localtask-mcp
systemctl --user status localtask-mcp          # see "active (running)"
journalctl --user -u localtask-mcp -f           # follow logs
```

To change the unit, edit `localtask-mcp.service` in the project, then
`systemctl --user daemon-reload && systemctl --user restart localtask-mcp` —
the symlink means the deployed copy tracks the project file automatically.

Workflow for key changes (the embed key is already baked into the binary):

```bash
# edit your own plaintext keys.json copy, then:
./localtask-mcp -encrypt-keys keys.json keys.json.enc   # plaintext source is consumed (deleted)
systemctl --user restart localtask-mcp
```

Notes:
- The binary must have been built with the embed key (`-ldflags "-X main.embedKey=..."`); a plain build can't decrypt `keys.json.enc` and the service will fail to start.
- To run the service while you're not logged in (e.g. at boot), enable lingering: `loginctl enable-linger $USER` (may need admin rights on some systems).
- A hardened variant adds `NoNewPrivileges=true`, `ProtectSystem=strict`, `ProtectHome=read-only`, `ReadWritePaths=/home/yuan/localtask` — but since the server legitimately runs shell commands and writes files anywhere, do not sandbox the filesystem more broadly (it would break the exec/write tools).

## RPM packages (RHEL-family Linux, server deployment)

For deploying to a RHEL/CentOS/Fedora/Rocky/Alma server, build RPMs with
[nfpm](https://github.com/goreleaser/nfpm). The packaged binary has a **fixed
embed key baked in** (via `-ldflags`); every host installing the RPM shares that
embed key, while each host keeps its own `keys.json.enc` (encrypted with that
key) in `/etc/localtask`. The RPM installs a **system-level** systemd unit
(`/usr/lib/systemd/system/localtask-mcp.service`) so the service starts at boot
without a logged-in user (no `loginctl enable-linger` needed).

Build (from the repo root; needs `go` and `nfpm` on PATH):

```bash
# EMBED_KEY must match the one you (will) encrypt keys.json.enc with.
# 64 hex chars = 32 bytes for AES-256.
EMBED_KEY=<your-embed-key> ./packaging/build-rpm.sh
# → dist/localtask-mcp-<ver>-1.x86_64.rpm
#   dist/localtask-mcp-<ver>-1.aarch64.rpm
```

Install on a target host:

```bash
sudo rpm -ivh dist/localtask-mcp-<ver>-1.<arch>.rpm
# rpm prints the post-install message with the keys setup steps (below).

# 1) Create bearer keys (each grants FULL host control) — keys travel in
#    cleartext over plain HTTP, so only bind 0.0.0.0 on a trusted network,
#    or add TLS.
echo -n '[{"key":"'$(openssl rand -hex 32)'","label":"alice"}]' | \
  sudo tee /etc/localtask/keys.json >/dev/null
sudo chmod 640 /etc/localtask/keys.json

# 2) Encrypt with the package binary (embed key is baked in). The plaintext
#    source is deleted after the ciphertext is verified to round-trip.
sudo /usr/local/bin/localtask-mcp -encrypt-keys /etc/localtask/keys.json /etc/localtask/keys.json.enc

# 3) (Optional) tune listen addr/port/TLS in the installed unit:
#       sudo vi /usr/lib/systemd/system/localtask-mcp.service
#    Defaults: bind 0.0.0.0:8011, plain HTTP.

# 4) Enable + start:
sudo systemctl daemon-reload
sudo systemctl enable --now localtask-mcp
sudo journalctl -u localtask-mcp -f

# 5) RHEL firewall (if bound off-loopback):
sudo firewall-cmd --permanent --add-port=8011/tcp && sudo firewall-cmd --reload
```

SELinux: the service runs shell commands and writes files anywhere as its user
(root by default), which enforcing SELinux may flag. If it fails to start, check
AVC denials with `sudo ausearch -m AVC -ts recent` and generate a policy with
`audit2allow` — do **not** blanket-disable SELinux with `setenforce 0`.

To remove: `sudo rpm -e localtask-mcp` (the pre-remove script stops+disables the
service). Config under `/etc/localtask` (incl. `keys.json.enc`) is kept — remove
manually if desired.

## TLS

HTTPS is available two ways:

**1. Self-signed (auto-generated).** Pass `-tls-selfsigned`. The server generates
a fresh ECDSA (P-256) self-signed certificate valid for 1 year (for `localhost`
and `127.0.0.1`) and prints its **SHA-256 fingerprint** to stderr:

```
localtask-mcp: TLS self-signed cert SHA-256 fingerprint:
  3a7650c9002c3ccd0ab00afc0d00452b10fbfc8c1f1e6f593caa30cb36b1e4d1
```

Pin that fingerprint in your client (skip CA verification and compare the cert
fingerprint). Best for local / trusted-LAN use where you don't have a domain.

**2. Your own cert files.** Pass `-cert <pem>` and `-key <pem>` (e.g. a Let's
Encrypt fullchain/privkey). Use this when you have a real domain + CA-issued cert.

No `-tls-selfsigned`, `-cert`, or `-key` → plain HTTP. Min TLS version is 1.2.

## Client configuration

### Claude Code (`~/.claude.json` / project `.mcp.json`)

stdio (recommended for local host management — no token needed):

```json
{
  "mcpServers": {
    "localtask": {
      "command": "/abs/path/to/localtask-mcp"
    }
  }
}
```

streamable HTTP (key passed as a header). Works for plain HTTP or HTTPS:

```json
{
  "mcpServers": {
    "localtask": {
      "url": "https://127.0.0.1:8443/mcp",
      "headers": {
        "Authorization": "Bearer <your-key>"
      }
    }
  }
}
```

> For self-signed certs, configure your client to skip CA verification and pin
> the SHA-256 fingerprint printed at startup. The exact mechanism depends on the
> client (e.g. env var, a `tls`/`--insecure` option, or a custom CA bundle
> containing the printed cert).

Or via CLI:

```bash
# stdio (no key)
claude mcp add localtask /abs/path/to/localtask-mcp

# HTTPS with a bearer key
claude mcp add --transport http localtask https://127.0.0.1:8443/mcp \
  --header "Authorization: Bearer <your-key>"
```

### Mapping from the old `server.py`

| `server.py` action | MCP tool     | Notes |
|--------------------|--------------|-------|
| `exec`             | `exec`       | same semantics, `sh -c` + timeout |
| `read`             | `read`       | UTF-8 only; binary → use `download` |
| `write`            | `write`      | now creates parent dirs |
| `PUT /upload`      | `write_bytes`| binary upload via hex/base64 (PUT raw body isn't an MCP concept) |
| `list`             | `list`       | tab-separated output |
| `info`             | `info`       | platform is now `os/arch` |
| `ps`               | `ps`         | same name whitelist |
| `df` / `mem`       | `df` / `mem` | identical |
| `k8s`              | `k8s`        | same default + timeout |
| — (new)            | `download`   | binary-safe read as base64 |

## Security

This server runs **arbitrary shell commands** and reads/writes **any file** reachable
by its OS user. It is intended for trusted local/single-user use (a personal
automation bridge between an MCP client and your host).

- **HTTP transport** is multi-key bearer authenticated ([Authentication](#authentication))
  and can run over **TLS** ([TLS](#tls)) — an improvement over the original
  `server.py`, which had no auth and no TLS. Prefer `-tls-selfsigned` (or your own
  cert) so the bearer key isn't sent in cleartext. Still: bind to loopback
  (`-http 127.0.0.1:…`) on shared hosts, and never expose `-http 0.0.0.0:…` to an
  untrusted network without transport-level protection in front.
- **stdio transport** is unauthenticated by design (it trusts the local process
  that spawned it). Prefer it for local use — no listening socket at all.
- Each key grants full host control; treat keys like root credentials. Rotate by
  editing `keys.json` and restarting.
- The SDK also applies DNS-rebinding / cross-origin protection by default
  (`DisableLocalhostProtection` is left on).

## Install Go (if needed)

```bash
# official toolchain to ~/.local/go (user-space, no sudo)
VER=1.26.6  # or latest from https://go.dev/dl
curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go$VER.linux-amd64.tar.gz"
mkdir -p ~/.local && tar -C ~/.local -xzf /tmp/go.tar.gz
export PATH="$HOME/.local/go/bin:$PATH"
go version
```

Optionally add to your shell profile:
```bash
export PATH="$HOME/.local/go/bin:$HOME/go/bin:$PATH"
export GOPROXY="https://goproxy.cn,https://proxy.golang.org,direct"  # use a mirror near you
```
