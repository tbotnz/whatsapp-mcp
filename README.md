# whatsapp-mcp

Let Claude read, search and send on your personal WhatsApp account.
No message database, no third-party service. One Go bridge, one small Python MCP server, both on your machine.

[![security](https://github.com/tbotnz/whatsapp-mcp/actions/workflows/security.yml/badge.svg)](https://github.com/tbotnz/whatsapp-mcp/actions/workflows/security.yml)

```
Claude ──MCP (stdio, or HTTPS + OAuth)──► MCP server ──loopback REST + token──► bridge ◄──WhatsApp Web protocol──► your phone
```

* **Memory only.** Messages live in the bridge's memory and nowhere else. Nothing a message
  contains is written to disk. Restart the bridge and the cache is empty.
* **History on demand.** Older messages are fetched from your phone when Claude asks for them,
  the same way WhatsApp Web does when you scroll up.
* **Locked down by default.** Loopback-only API with a bearer token, media and recipient
  allow-lists, send rate limit, send audit log, optional read-only mode.
* **Local or remote.** stdio for Claude Desktop and Cursor, or HTTPS behind a built-in OAuth 2.1
  server so claude.ai can use it as a custom connector.
* **No lock-in.** Standard MCP on top of [whatsmeow](https://github.com/tulir/whatsmeow), the
  WhatsApp Web multidevice library. Runs anywhere Go and Python or Docker run.

![WhatsApp MCP](./example-use.png)

What it cannot do: protect you from prompt injection. Anyone who can message you can try to
instruct the agent, and this server is exposed to
[the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/) like any MCP
server with private data and a way to send. Read-only mode and the recipient controls are the real
defences; see [Security](#security).

Forked from [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) and reworked around
a memory-only cache, an authenticated bridge and remote access.

## Layout

| Path | What |
|---|---|
| `whatsapp-bridge/` | Go bridge: WhatsApp session, QR login, in-memory message cache, authenticated loopback REST API. The only component that holds WhatsApp data |
| `whatsapp-mcp-server/` | Python MCP server: translates tool calls into bridge requests. Holds no data. stdio or streamable HTTP with OAuth 2.1 |
| `docker-compose.yml` | Both components as hardened containers, for the claude.ai setup |
| `docker-compose.vps.yml` | Overlay for a host where another stack already owns 80/443 |
| `deploy/shared-host/` | Walkthrough and nginx config for that overlay |
| `SECURITY.md` | Threat model, reporting, hardening checklist |

## Getting started

Three steps with Docker Compose. Needs Docker and an HTTPS hostname that reaches port 8000 on this
machine; [Remote access](#remote-access-from-claudeai) lists the ways to get one.

**1. Configure and start.**

```bash
git clone https://github.com/tbotnz/whatsapp-mcp.git && cd whatsapp-mcp
cp .env.example .env          # set WHATSAPP_MCP_PUBLIC_URL=https://your-host
docker compose up -d --build
```

**2. Scan the QR code** with WhatsApp on your phone (Settings → Linked Devices).

```bash
docker compose logs -f bridge
```

**3. Add the connector in claude.ai.** Settings → Connectors → Add custom connector, URL
`https://your-host/mcp`, client ID and secret empty. The consent page asks for an approval code:

```bash
docker compose logs mcp
```

That's it. Ask Claude "what did I miss in the family group today?" then "tell Alice I'm running
late". The connector works in claude.ai on the web, on your phone and in Claude Desktop.

No hostname, or only want it on your own machine? See
[Running without Docker](#running-without-docker).

## Docker

What `docker compose up` gives you:

* Builds the bridge with CGO inside the Go image. No C toolchain needed on the host.
* Both containers run unprivileged, read-only root filesystem, all capabilities dropped,
  `no-new-privileges`.
* Session keys, API token, OAuth state and audit logs sit in one named volume (`store`) that only
  the two containers mount. Back it up like a password database.
* The bridge's port is not published at all. The MCP server is published on `127.0.0.1:8000` only.
  Inside compose, network isolation replaces the bridge's loopback `Host` check.
* Both have a `GET /health` probe. Compose waits for the bridge before starting the server.
* The bridge waits `WHATSAPP_QR_TIMEOUT` seconds (default 180) for a scan, then exits so the
  restart prints a fresh code. Once paired, the session persists in the volume.

**Sharing a host.** If another stack already owns 80/443, add the `docker-compose.vps.yml`
overlay. It publishes no host ports and attaches the MCP server to the existing reverse proxy's
Docker network instead. Walkthrough in [deploy/shared-host](deploy/shared-host/README.md).

## Remote access from claude.ai

claude.ai (web, mobile, or a Claude Desktop custom connector) needs the server on HTTPS and behind
OAuth. A small, strict OAuth 2.1 authorization server is built in, so no identity provider is
needed. You bring the hostname and TLS in front of port 8000. Any of:

* **Cloudflare Tunnel**, no inbound ports. Create a tunnel with a public hostname pointing at
  `http://mcp:8000`, put its token in `.env` as `CLOUDFLARE_TUNNEL_TOKEN`, and start with
  `docker compose --profile tunnel up -d --build` instead. A Cloudflare Access policy in front is a
  good extra layer.
* **Tailscale Funnel**: `tailscale funnel 8000`. `tailscale serve` is tailnet only and claude.ai
  cannot reach it.
* **Caddy or nginx** with Let's Encrypt, proxying to `127.0.0.1:8000` and forwarding the original
  `Host` header.
* **An nginx container that already owns 80/443**: see "Sharing a host" above.

`WHATSAPP_MCP_PUBLIC_URL` is `https://your-host`, with no `/mcp` and no trailing slash. A plain
`http` URL is refused unless `WHATSAPP_MCP_ALLOW_INSECURE_HTTP=1` (local testing only).

To approve from your phone without console access, run
`docker compose run --rm mcp python oauth.py hash-password` and put the result in `.env` as
`WHATSAPP_MCP_OWNER_PASSWORD_HASH`. The consent page then takes the password as well as the code.

When you stop using it, remove the connector in claude.ai or run `oauth.py revoke-all`.

How it stays safe:

* **Only you can approve.** The consent page needs a one-time code printed to the server console.
  8 characters from a 32-symbol alphabet, 5 minutes, 5 attempts, and 20 failures in 15 minutes lock
  the endpoint. Or a password, if you set the hash.
* **Only claude.ai can be a client.** Dynamic registration is on because claude.ai requires it,
  but every redirect URI must be on the allow-list (`https://claude.ai/api/mcp/auth_callback` and
  `https://claude.com/api/mcp/auth_callback` by default). A stolen authorization code cannot be
  delivered anywhere else.
* **PKCE (S256) is mandatory**, `state` is passed through, and the `resource` indicator is bound
  to this server's `/mcp` URL so a token cannot be replayed against another MCP server.
* **Two scopes.** `whatsapp:read` is always granted, `whatsapp:send` only if you tick the box. The
  send tools check scopes on every call, and `WHATSAPP_MCP_READ_ONLY=1` removes them regardless.
* **Short-lived, hashed tokens.** Access 1 hour, refresh 30 days with rotation on every use. Only
  SHA-256 hashes are stored (`store/mcp_oauth_state.json`, mode 600). `/revoke` is supported.
  `oauth.py revoke-all` logs every client out, `oauth.py status` lists live sessions (prefix with
  `uv run python` or `docker compose exec mcp python`).
* **DNS-rebinding protection.** Requests whose `Host` is not the public hostname, or whose
  `Origin` is not claude.ai, are rejected.
* **Audit trail.** Registrations, authorization requests, consent grants, denials and failures,
  token issue, refresh and revoke are appended as JSON lines to `store/mcp_auth_audit.log`.

## Running without Docker

For Claude Code, Claude Desktop or Cursor on the same machine. They start the MCP server
themselves over stdio, so nothing is exposed and no hostname is needed. Needs Go 1.26+ and
[uv](https://docs.astral.sh/uv/) (uv fetches Python for you).

**1. Start the bridge and scan the QR code** with WhatsApp on your phone (Settings → Linked Devices).

```bash
git clone https://github.com/tbotnz/whatsapp-mcp.git
cd whatsapp-mcp/whatsapp-bridge && go run .
```

**2. Connect Claude.** Leave the bridge running and, in a second terminal, from the repo root:

```bash
claude mcp add whatsapp -- uv --directory "$PWD/whatsapp-mcp-server" run main.py
```

That is Claude Code. For Claude Desktop or Cursor, put this in
`~/Library/Application Support/Claude/claude_desktop_config.json` or `~/.cursor/mcp.json` and
restart the app (`which uv` gives the command, `pwd` the path):

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/path/to/uv",
      "args": ["--directory", "/path/to/whatsapp-mcp/whatsapp-mcp-server", "run", "main.py"]
    }
  }
}
```

**3. Ask Claude.** "What did I miss in the family group today?" then "tell Alice I'm running late".

Good to know:

* The bridge has to stay running. The session lasts about 20 days before WhatsApp may ask you to
  scan again.
* FFmpeg is optional, only for sending voice messages.
* The bridge pins a patched Go toolchain in `go.mod`; `go` downloads it for you.
* On first run the bridge writes a random API token to `whatsapp-bridge/store/api_token`. The MCP
  server reads it from there, so there is nothing to configure.

Hardening options go in an `env` block in the same config (or `--env KEY=value` with
`claude mcp add`):

```json
"env": {
  "WHATSAPP_MCP_READ_ONLY": "1",
  "WHATSAPP_MEDIA_DIRS": "/Users/you/Pictures:/Users/you/Documents/to-send"
}
```

To serve claude.ai without Docker, set `WHATSAPP_MCP_TRANSPORT=http` and
`WHATSAPP_MCP_PUBLIC_URL`, then run `uv run python main.py` in `whatsapp-mcp-server/`.

**Windows**: the session store uses `go-sqlite3`, which needs CGO and a C compiler. Install one
with [MSYS2](https://www.msys2.org/) (add `ucrt64\bin` to `PATH`,
[guide](https://code.visualstudio.com/docs/cpp/config-mingw)), then
`go env -w CGO_ENABLED=1` before `go run .`. Without it you get
`Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`

## Tools

| Tool | What |
|---|---|
| `search_contacts` | Search contacts by name or phone number |
| `list_chats` | List chats with metadata |
| `get_chat` | Information about one chat |
| `get_direct_chat_by_contact` | Find the direct chat with a contact |
| `get_contact_chats` | All chats involving a contact |
| `list_messages` | Messages with optional filters and context |
| `get_last_interaction` | Most recent message with a contact |
| `get_message_context` | Messages around a specific message |
| `fetch_older_messages` | Ask the phone for older messages in a chat and add them to the cache. Phone must be online |
| `download_media` | Download the media of a message, returns the local path |
| `send_message` | Send text to a phone number or group JID |
| `send_file` | Send an image, video, document or raw audio file |
| `send_audio_message` | Send audio as a playable voice message |

**Sending media.** Files must be under `whatsapp-bridge/store/` or a directory listed in
`WHATSAPP_MEDIA_DIRS`. Voice messages must be `.ogg` Opus. With FFmpeg installed other formats
(MP3, WAV, ...) are converted automatically; without it, `send_file` still sends the raw audio, it
just will not show as a playable voice message.

**Receiving media.** Only the metadata and decryption info of incoming media are kept in memory.
`download_media` takes the `message_id` and `chat_jid` shown next to the message, saves the file
into `whatsapp-bridge/store/` and returns the path. Downloaded files are the only message content
that ever reaches disk, and only because you asked for them.

## What is stored where

* **Messages: memory only.** The bridge caches what arrives while it runs plus whatever you fetch
  from the phone, capped by `WHATSAPP_MAX_CACHED_MESSAGES` (default 50,000, oldest evicted first).
* **Older history: your phone.** WhatsApp servers only relay end-to-end encrypted traffic, so
  there is no server-side store to query. `fetch_older_messages` asks the phone for the next batch.
* **On disk, in `whatsapp-bridge/store/`:** `whatsapp.db` (device identity and encryption keys,
  needed to stay paired), `api_token`, `send_audit.log` (metadata only), `chat_index.json`, and any
  media you explicitly download.
* **`chat_index.json` is metadata only**: chat JIDs, display names, timestamps and the IDs of the
  oldest and newest message seen per chat, so the bridge can still ask the phone for older history
  after a restart. No message text, no media keys. `WHATSAPP_PERSIST_CHAT_INDEX=0` turns it off;
  history fetching then only works for chats that received a message since startup.
* **Upgrading from an older version?** Delete `whatsapp-bridge/store/messages.db`. It still holds
  plaintext history and is no longer read.

## Security

This gives an AI agent read access to your private messages and the ability to send as you. The
threat model and an operator checklist are in [SECURITY.md](SECURITY.md).

### What is enforced

* **Loopback only.** The bridge API binds to `127.0.0.1` and rejects requests whose `Host` is not
  a loopback address (blocks DNS rebinding). It also requires `Content-Type: application/json` and
  a bearer token, which together stop a web page in your browser from calling it.
* **Bearer token.** Generated on first run into `whatsapp-bridge/store/api_token` (mode 600) and
  required on every call. Set `WHATSAPP_API_TOKEN` on both sides to use your own.
* **Media allow-list.** Files can only be sent from `whatsapp-bridge/store/` plus
  `WHATSAPP_MEDIA_DIRS` (colon-separated, semicolon on Windows). Symlinks are resolved first. The
  bridge enforces it independently of the MCP server and before any filesystem access, so the API
  cannot be used to probe for files. Without it a prompt-injected agent could send `~/.ssh/id_rsa`
  to an attacker.
* **Safe downloads.** Filenames chosen by remote senders are sanitized and downloads are confined
  to `whatsapp-bridge/store/`.
* **Recipient controls.** `WHATSAPP_ALLOWED_RECIPIENTS` limits who the agent can ever message.
  `WHATSAPP_SEND_KNOWN_CHATS_ONLY=1` limits sending to chats already in your history, so an
  injected instruction cannot open a conversation with a stranger. Both off by default; either one
  is a strong defence against text exfiltration.
* **Recipient validation.** Phone numbers must be digits only, JIDs must be on a known WhatsApp
  server.
* **Send audit log.** Every send attempt is appended as a JSON line to
  `whatsapp-bridge/store/send_audit.log`: time, recipient, kind, media filename, message length,
  outcome. No message bodies. `WHATSAPP_DISABLE_AUDIT_LOG=1` turns it off.
* **Rate limit.** At most `WHATSAPP_SEND_RATE_LIMIT` sends per minute (default 20, 0 disables).
* **Size limits.** Media over `WHATSAPP_MAX_MEDIA_BYTES` (default 64 MiB) is refused for sending
  and downloading, messages over 64k characters are rejected, request bodies are capped at 1 MiB,
  and the HTTP server has read, write and idle timeouts.
* **Output hardening.** Contact and group names are flattened to one printable line and
  multi-line message bodies are indented before the agent sees them, so a sender cannot forge a
  fake "From: Me" line inside the tool output.
* **File permissions.** `store/` is mode 700; the session database, token and downloaded media are
  mode 600. `whatsapp.db` holds your Signal session keys: anyone who can read it can act as your
  WhatsApp account.
* **One enforcement point.** The MCP server has no data access of its own. Every read goes through
  the bridge's authenticated API, where policy, limits and logging live. Pagination and fetch sizes
  from the agent are clamped server-side.
* **Quiet logs.** The bridge logs message metadata (chat, direction, type), never bodies.
* **Read-only mode.** With `WHATSAPP_MCP_READ_ONLY=1` the send tools are not registered at all.
* **Remote mode is OAuth-only.** Every MCP request needs a token issued after you approve the
  connection. Health and OAuth discovery are the only unauthenticated routes.
* **Tool annotations.** Read tools are marked `readOnlyHint`, send tools `openWorldHint`, so
  clients that support it can ask for confirmation.
* **Dependencies.** Go and Python dependencies are pinned and scanned. CI
  (`.github/workflows/security.yml`) runs `gofmt`, `go vet`, the Go tests, `govulncheck`, the
  Python end-to-end tests, `pip-audit` and a compose build on every push and weekly. Dependabot
  keeps both ecosystems current.

### What is not covered

* **Prompt injection.** Message content returned to the agent is untrusted. The server says so in
  its instructions, but that is advice, not enforcement. Read-only mode, the recipient controls and
  the media allow-list are the real controls.
* **Encryption at rest of the session.** `whatsapp.db` is not encrypted. Full-disk encryption and
  the 600/700 permissions protect it, and whoever copies it can request your history from your
  phone just as the bridge does.
* **Process memory.** While the bridge runs, cached messages are plaintext in its memory, like any
  WhatsApp client.
* **Other processes running as you.** Anything under your user account can read the token and the
  session database, or talk to the bridge API.
* **The hosted client.** In remote mode Anthropic's servers hold a valid token for as long as you
  keep the connector, and messages you ask about pass through claude.ai like any other connector
  data.

## Configuration

| Variable | Where | Default | Purpose |
|---|---|---|---|
| `WHATSAPP_API_TOKEN` | both | file `store/api_token` | Shared secret for the bridge API (min 32 chars) |
| `WHATSAPP_MEDIA_DIRS` | both | *(none)* | Extra directories files may be sent from |
| `WHATSAPP_MCP_READ_ONLY` | MCP server | `0` | `1` disables all send tools |
| `WHATSAPP_BRIDGE_ADDR` | bridge | `127.0.0.1:8080` | Listen address. Non-loopback values disable the Host check and expose the API to the network; not recommended |
| `WHATSAPP_API_BASE_URL` | MCP server | `http://127.0.0.1:8080/api` | Where the MCP server finds the bridge |
| `WHATSAPP_BRIDGE_STORE` | MCP server | `../whatsapp-bridge/store` | Where to find the bridge's `api_token`; also the default media allow-list entry |
| `WHATSAPP_MAX_CACHED_MESSAGES` | bridge | `50000` | In-memory cache size; oldest messages are evicted first |
| `WHATSAPP_PERSIST_CHAT_INDEX` | bridge | `1` | `0` disables the metadata-only `chat_index.json` |
| `WHATSAPP_HISTORY_TIMEOUT` | bridge | `45` | Seconds to wait for the phone to answer a history request |
| `WHATSAPP_ALLOWED_RECIPIENTS` | bridge | *(none)* | Comma-separated numbers/JIDs the agent may message; empty means no restriction |
| `WHATSAPP_SEND_KNOWN_CHATS_ONLY` | bridge | `0` | `1` only allows sending to chats already in your history |
| `WHATSAPP_DISABLE_AUDIT_LOG` | bridge | `0` | `1` stops writing `store/send_audit.log` |
| `WHATSAPP_SEND_RATE_LIMIT` | bridge | `20` | Max sends per minute, `0` to disable |
| `WHATSAPP_MAX_MEDIA_BYTES` | bridge | `67108864` | Max size of a file to send or download |
| `WHATSAPP_API_TIMEOUT` | MCP server | `180` | Seconds to wait for the bridge |
| `WHATSAPP_QR_TIMEOUT` | bridge | `180` | Seconds to wait for a QR scan before exiting (containers restart and show a fresh code) |
| `WHATSAPP_MCP_TRANSPORT` | MCP server | `stdio` | `http` serves streamable HTTP behind OAuth for claude.ai |
| `WHATSAPP_MCP_PUBLIC_URL` | MCP server | *(required in http mode)* | `https://host` claude.ai connects to; `/mcp` is appended |
| `WHATSAPP_MCP_HOST` / `WHATSAPP_MCP_PORT` | MCP server | `127.0.0.1` / `8000` | Listen address in http mode |
| `WHATSAPP_MCP_ALLOWED_REDIRECTS` | MCP server | claude.ai/claude.com callbacks | Comma-separated OAuth redirect URIs clients may register |
| `WHATSAPP_MCP_OWNER_PASSWORD_HASH` | MCP server | *(none)* | PBKDF2 hash from `oauth.py hash-password`; lets you approve without console access |
| `WHATSAPP_MCP_ACCESS_TOKEN_TTL` / `WHATSAPP_MCP_REFRESH_TOKEN_TTL` | MCP server | `3600` / `2592000` | Token lifetimes in seconds |
| `WHATSAPP_MCP_EXTRA_ALLOWED_HOSTS` | MCP server | *(none)* | Additional `Host` values to accept, e.g. `mcp:8000` inside compose |
| `WHATSAPP_MCP_ALLOW_INSECURE_HTTP` | MCP server | `0` | Permit a plain-http public URL (local testing only) |
| `WHATSAPP_MCP_BIND` | compose | `127.0.0.1` | Host address the MCP port is published on |
| `WHATSAPP_MCP_EDGE_NETWORK` | compose (vps overlay) | *(required)* | Docker network of the existing reverse proxy |
| `CLOUDFLARE_TUNNEL_TOKEN` | compose (tunnel profile) | *(none)* | Token for the bundled cloudflared service |

## Tests

```bash
cd whatsapp-bridge && go test ./...
cd whatsapp-mcp-server && uv run python tests/e2e_test.py .
cd whatsapp-mcp-server && uv run python tests/oauth_e2e_test.py .
docker compose build
```

The Python tests start the MCP server against a stub bridge, over stdio and over HTTP with the
full OAuth flow (discovery, registration, PKCE, consent, scopes, refresh rotation, revocation).
No WhatsApp account needed.

## Troubleshooting

**Setup**

* **uv permission errors**: add uv to your `PATH` or use its full path in the client config.
* **Nothing works**: the bridge and the MCP server both have to be running.
* **"Bridge API token not found"** or **HTTP 401**: start the bridge once so it creates
  `whatsapp-bridge/store/api_token`, or set the same `WHATSAPP_API_TOKEN` on both sides. If you
  moved the store directory, set `WHATSAPP_BRIDGE_STORE` for the MCP server.

**Pairing and history**

* **No QR code**: restart the bridge, and check your terminal can render QR codes.
* **No QR code and it just connects**: the session is still valid. That is expected.
* **Device limit reached**: remove a linked device in WhatsApp on your phone (Settings → Linked
  Devices).
* **No messages yet**: right after pairing the phone pushes recent history, which can take a few
  minutes. After a restart the cache starts empty; new messages appear as they arrive and
  `fetch_older_messages` pulls older ones.
* **"no cached messages for this chat to anchor the request"**: the bridge can only ask for
  messages older than one it has seen. Wait for a message in that chat, or keep
  `WHATSAPP_PERSIST_CHAT_INDEX` on so anchors survive restarts.
* **"timed out waiting for the phone"**: the phone is offline, asleep with background data off, or
  WhatsApp was force-stopped. Open WhatsApp on the phone and retry.
* **Out of sync, cannot connect or decrypt**: delete `whatsapp-bridge/store/whatsapp.db` and
  restart the bridge to pair again.

**Sending**

* **"media_path is outside the allowed media directories"**: add the directory to
  `WHATSAPP_MEDIA_DIRS` on both the bridge and the MCP server, restart both.
* **"Send rate limit exceeded"**: raise `WHATSAPP_SEND_RATE_LIMIT`.
* **"recipient ... is not in WHATSAPP_ALLOWED_RECIPIENTS"** or **"is not an existing chat"**: the
  recipient policy refused the send. Adjust `WHATSAPP_ALLOWED_RECIPIENTS` or
  `WHATSAPP_SEND_KNOWN_CHATS_ONLY` and restart the bridge.

**claude.ai**

* **Connector failed to authorize**: look for `client_rejected` in `docker compose logs mcp` (the
  callback is not on `WHATSAPP_MCP_ALLOWED_REDIRECTS`). Check `WHATSAPP_MCP_PUBLIC_URL` matches the
  hostname in the browser exactly, scheme and port included, and that your proxy forwards the
  original `Host` header. A mismatch is rejected as DNS rebinding.
* **"This connection was authorized without the 'whatsapp:send' scope"**: the send box was left
  unticked. Remove and re-add the connector and tick it, or keep it read-only.
* **Lost the approval code**: it is printed once per request. Start the connection again from
  claude.ai, or set `WHATSAPP_MCP_OWNER_PASSWORD_HASH`.

For Claude Desktop itself, see the
[MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues).

## License

MIT (see `LICENSE`).
