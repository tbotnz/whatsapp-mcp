# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal Whatsapp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to your **personal WhatsApp account** directly via the Whatsapp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). Messages are held **in memory only** by the bridge process and are sent to an LLM (such as Claude) only when the agent accesses them through tools (which you control). Nothing a message contains is written to disk. Older history is fetched from your phone on demand, the same way WhatsApp Web does it.

Here's an example of what you can do when it's connected to Claude.

![WhatsApp MCP](./example-use.png)

> To get updates on this and other projects I work on [enter your email here](https://docs.google.com/forms/d/1rTF9wMBTN0vPfzWuQa2BjfGKdKIpTbyeKxhPMcEzgyI/preview)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that project injection could lead to private data exfiltration.

## Installation

### Prerequisites

- Go 1.26+ (the bridge pins a patched toolchain in `go.mod`; `go` downloads it automatically)
- Python 3.11+
- Anthropic Claude Desktop app (or Cursor), or a claude.ai account for the remote connector
- UV (Python package manager), install with `curl -LsSf https://astral.sh/uv/install.sh | sh`
- Or skip Go, Python and UV entirely and use Docker Compose (see [Running with Docker](#running-with-docker))
- FFmpeg (_optional_) - Only needed for audio messages. If you want to send audio files as playable WhatsApp voice messages, they must be in `.ogg` Opus format. With FFmpeg installed, the MCP server will automatically convert non-Opus audio files. Without FFmpeg, you can still send raw audio files using the `send_file` tool.

### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/lharries/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Run the WhatsApp bridge**

   Navigate to the whatsapp-bridge directory and run the Go application:

   ```bash
   cd whatsapp-bridge
   go run main.go
   ```

   The first time you run it, you will be prompted to scan a QR code. Scan the QR code with your WhatsApp mobile app to authenticate.

   On first run the bridge also generates a random API token in `whatsapp-bridge/store/api_token` (readable only by your user). The MCP server reads this file automatically; nothing to configure. The bridge's REST API only listens on `127.0.0.1:8080`.

   After approximately 20 days, you will might need to re-authenticate.

3. **Connect to the MCP server**

   Copy the below json with the appropriate {{PATH}} values:

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "{{PATH_TO_UV}}", // Run `which uv` and place the output here
         "args": [
           "--directory",
           "{{PATH_TO_SRC}}/whatsapp-mcp/whatsapp-mcp-server", // cd into the repo, run `pwd` and enter the output here + "/whatsapp-mcp-server"
           "run",
           "main.py"
         ]
       }
     }
   }
   ```

   Optional hardening settings go in an `env` block (see [Security](#security) for what each one does):

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "{{PATH_TO_UV}}",
         "args": ["--directory", "{{PATH_TO_SRC}}/whatsapp-mcp/whatsapp-mcp-server", "run", "main.py"],
         "env": {
           "WHATSAPP_MEDIA_DIRS": "/Users/you/Pictures:/Users/you/Documents/to-send",
           "WHATSAPP_MCP_READ_ONLY": "0"
         }
       }
     }
   }
   ```

   For **Claude**, save this as `claude_desktop_config.json` in your Claude Desktop configuration directory at:

   ```
   ~/Library/Application Support/Claude/claude_desktop_config.json
   ```

   For **Cursor**, save this as `mcp.json` in your Cursor configuration directory at:

   ```
   ~/.cursor/mcp.json
   ```

4. **Restart Claude Desktop / Cursor**

   Open Claude Desktop and you should now see WhatsApp as an available integration.

   Or restart Cursor.

### Running in Docker

`docker-compose.yml` runs the bridge and the MCP server (HTTP mode, OAuth-protected) as two containers; setup steps are in the comments at the top of that file and in `.env.example`. Only the MCP server is published, on `127.0.0.1:8000`.

To run it on a host where another stack already owns ports 80/443, add the `docker-compose.vps.yml` overlay: it publishes no host ports and attaches the MCP server to the existing reverse proxy's Docker network instead. [deploy/shared-host](deploy/shared-host/README.md) has the full walkthrough.

### Windows Compatibility

If you're running this project on Windows, be aware that `go-sqlite3` requires **CGO to be enabled** in order to compile and work properly. By default, **CGO is disabled on Windows**, so you need to explicitly enable it and have a C compiler installed.

#### Steps to get it working:

1. **Install a C compiler**  
   We recommend using [MSYS2](https://www.msys2.org/) to install a C compiler for Windows. After installing MSYS2, make sure to add the `ucrt64\bin` folder to your `PATH`.  
   → A step-by-step guide is available [here](https://code.visualstudio.com/docs/cpp/config-mingw).

2. **Enable CGO and run the app**

   ```bash
   cd whatsapp-bridge
   go env -w CGO_ENABLED=1
   go run main.go
   ```

Without this setup, you'll likely run into errors like:

> `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`

## Architecture Overview

This application consists of two main components:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): A Go application that connects to WhatsApp's web API, handles authentication via QR code, and keeps a bounded in-memory cache of messages. It exposes an authenticated loopback REST API and is the only component that holds WhatsApp data.

2. **Python MCP Server** (`whatsapp-mcp-server/`): A thin Model Context Protocol (MCP) server that translates tool calls into requests to the bridge. It holds no data and never opens a database. It speaks stdio to a local client (Claude Desktop, Cursor) or, in HTTP mode, streamable HTTP behind a built-in OAuth 2.1 authorization server so claude.ai can use it as a custom connector.

### Data Storage

- **Messages live in memory only.** The bridge caches what arrives while it runs (capped by `WHATSAPP_MAX_CACHED_MESSAGES`, default 50,000, oldest evicted first) plus whatever you fetch from the phone. Restarting the bridge empties the cache.
- **Older history comes from your phone on demand.** WhatsApp has no server-side message store to query (the servers only relay end-to-end encrypted traffic), so the `fetch_older_messages` tool asks your phone for the next batch of older messages in a chat, exactly as WhatsApp Web does when you scroll up. The phone must be online.
- **On disk, inside `whatsapp-bridge/store/`:** `whatsapp.db` (the WhatsApp session: device identity and encryption keys, required to stay paired), `api_token`, `send_audit.log` (metadata only), any media you explicitly download, and `chat_index.json`.
- **`chat_index.json` is metadata only:** chat JIDs, display names, timestamps and the IDs of the oldest/newest message seen per chat. It exists so the bridge can still ask the phone for older history after a restart. It contains no message text or media keys. Disable it with `WHATSAPP_PERSIST_CHAT_INDEX=0` if you prefer nothing at all; history fetching then works only for chats that have received a message since startup.
- If you are upgrading, delete the old `whatsapp-bridge/store/messages.db`; it still holds plaintext history from earlier versions and is no longer read.

## Running with Docker

The repository ships a `docker-compose.yml` that builds both components and wires them together on a private network. Only the MCP server is published, and only on the host's loopback interface.

```bash
cp .env.example .env          # set WHATSAPP_MCP_PUBLIC_URL (see the next section)
docker compose up -d --build
docker compose logs -f bridge # scan the QR code on first start
```

What the compose file does for you:

- Builds the bridge with CGO inside the Go image, so you do not need a C toolchain on the host.
- Runs both containers as an unprivileged user with a read-only root filesystem, all capabilities dropped and `no-new-privileges`.
- Keeps the session keys, API token, OAuth state and audit logs in one named volume (`store`) that only the two containers mount. Back it up like a password database.
- Does not publish the bridge's port at all; the MCP container reaches it by service name. The bridge's loopback-only `Host` check is replaced by network isolation in this setup.
- Starts the MCP server in HTTP mode by default. The bridge has a `GET /health` probe and the MCP server a `GET /health` route; compose waits for the bridge to be healthy before starting the server.

The bridge waits `WHATSAPP_QR_TIMEOUT` seconds (default 180) for you to scan the code, then exits so a fresh code is printed on the automatic restart. Once paired, the session persists in the volume. The containers are meant for the claude.ai (HTTP) setup. Claude Desktop and Cursor launch the MCP server as a local stdio process, so for those run both components locally as described above.

## Remote access from claude.ai

Claude Desktop launches the MCP server itself over stdio, so nothing is exposed. To use the same server from claude.ai (web, mobile, or a Claude Desktop "custom connector") it must be reachable over HTTPS and protected by OAuth. This project includes a small, strict OAuth 2.1 authorization server for exactly that, so no third-party identity provider is needed.

How it stays safe:

- **Only you can approve a connection.** When claude.ai starts the OAuth flow, your browser lands on a consent page served by this server, and the server prints a one-time approval code to its console (`docker compose logs mcp`). Nothing on the public page is enough on its own. Codes are 8 characters from a 32-symbol alphabet, expire after 5 minutes, allow 5 attempts, and 20 failures in 15 minutes lock the consent endpoint. If you cannot see the console, set `WHATSAPP_MCP_OWNER_PASSWORD_HASH` and approve with a password instead.
- **Only claude.ai can be a client.** Dynamic client registration is on because claude.ai requires it, but every redirect URI must be on the allow-list (`https://claude.ai/api/mcp/auth_callback` and `https://claude.com/api/mcp/auth_callback` by default). A registration with any other callback is refused, so a stolen authorization code can never be delivered anywhere else.
- **PKCE (S256) is mandatory**, `state` is passed through, and the `resource` indicator is bound to this server's `/mcp` URL so a token cannot be replayed against another MCP server.
- **Two scopes.** `whatsapp:read` is always granted; `whatsapp:send` only if you tick the box on the consent page. The send tools check the token's scopes on every call, and `WHATSAPP_MCP_READ_ONLY=1` removes them regardless.
- **Short-lived, hashed tokens.** Access tokens last 1 hour, refresh tokens 30 days and rotate on every use. Only SHA-256 hashes are stored (`store/mcp_oauth_state.json`, mode 600). `/revoke` is supported, and `uv run python oauth.py revoke-all` (or `docker compose exec mcp python oauth.py revoke-all`) logs every client out immediately; `oauth.py status` lists live sessions.
- **DNS-rebinding protection** on the MCP endpoint: requests whose `Host` is not the public hostname (or `Origin` is not claude.ai) are rejected.
- **Audit trail.** Registrations, authorization requests, consent grants/denials/failures, token issue/refresh/revoke are appended as JSON lines to `store/mcp_auth_audit.log`.

Setup:

1. Pick a hostname and get TLS in front of port 8000. Any of these works:
   - **Cloudflare Tunnel** (no inbound ports): create a tunnel in the Zero Trust dashboard with a public hostname pointing at `http://mcp:8000`, put its token in `.env` as `CLOUDFLARE_TUNNEL_TOKEN`, and start with `docker compose --profile tunnel up -d`. Adding a Cloudflare Access policy in front is a good extra layer; the OAuth server still protects every request behind it.
   - **Tailscale Funnel / Serve**: `tailscale funnel 8000` (public) or `tailscale serve 8000` (tailnet only; claude.ai cannot reach a tailnet-only URL, so Funnel is needed for the hosted app).
   - **Caddy / nginx** with Let's Encrypt, proxying to `127.0.0.1:8000`, forwarding the original `Host` header.
2. Set `WHATSAPP_MCP_PUBLIC_URL=https://your-host` in `.env` (no `/mcp`, no trailing slash). Running without Docker: also set `WHATSAPP_MCP_TRANSPORT=http` and run `uv run python main.py` in `whatsapp-mcp-server/`. The server refuses to start with a plain-`http` public URL unless `WHATSAPP_MCP_ALLOW_INSECURE_HTTP=1` (local testing only).
3. In claude.ai go to Settings → Connectors → Add custom connector, and enter `https://your-host/mcp`. Leave client ID and secret empty; claude.ai registers itself.
4. claude.ai opens the consent page. Read the approval code from the server log (`docker compose logs mcp`), enter it, decide whether to allow sending, and approve.
5. Optional: generate a password hash with `docker compose run --rm mcp python oauth.py hash-password` and put it in `.env` as `WHATSAPP_MCP_OWNER_PASSWORD_HASH` so you can approve from your phone without console access.

When you stop using the connector, run `oauth.py revoke-all` or remove the connector in claude.ai, which revokes its tokens.

## Security

This project gives an AI agent read access to your private messages and the ability to send messages as you. The design below limits what can go wrong, but read the caution at the top of this README: an attacker who can message you can try to prompt-inject the agent.

### What is enforced

- **Loopback only.** The bridge's REST API binds to `127.0.0.1` and rejects requests whose `Host` header is not a loopback address (blocks DNS-rebinding). It also requires `Content-Type: application/json` and a bearer token, which together stop a web page in your browser from calling it.
- **Bearer token.** Generated on first run into `whatsapp-bridge/store/api_token` (mode 600). Every API call must present it. The MCP server reads the file; set `WHATSAPP_API_TOKEN` on both sides to use your own.
- **Media allow-list.** Files can only be sent from `whatsapp-bridge/store/` plus directories listed in `WHATSAPP_MEDIA_DIRS` (colon-separated on macOS/Linux, semicolon on Windows). Symlinks are resolved before checking. The bridge enforces this independently of the MCP server, and the check runs before any filesystem access so the API cannot be used to probe for files. Without this, a prompt-injected agent could send `~/.ssh/id_rsa` to an attacker.
- **Safe downloads.** Filenames chosen by remote senders are sanitized and downloads are confined to `whatsapp-bridge/store/`.
- **Recipient controls.** `WHATSAPP_ALLOWED_RECIPIENTS` (comma-separated numbers or JIDs) limits who the agent can ever message. `WHATSAPP_SEND_KNOWN_CHATS_ONLY=1` limits sending to chats that already exist in your history, so an injected instruction cannot open a conversation with a stranger's number. Both are off by default; either one is a strong defence against text exfiltration.
- **Send audit log.** Every send attempt is appended as a JSON line to `whatsapp-bridge/store/send_audit.log` (time, recipient, kind, media filename, message length, outcome). Message bodies are not written. Set `WHATSAPP_DISABLE_AUDIT_LOG=1` to turn it off.
- **Rate limit.** At most `WHATSAPP_SEND_RATE_LIMIT` sends per minute (default 20, 0 disables).
- **Size limits.** Media larger than `WHATSAPP_MAX_MEDIA_BYTES` (default 64 MiB) is refused both for sending and for downloading; messages over 64k characters are rejected; request bodies are capped at 1 MiB; the HTTP server has read/write/idle timeouts.
- **Output hardening.** Contact and group names are flattened to one printable line and multi-line message bodies are indented before being shown to the agent, so a sender cannot forge a fake "From: Me" line inside the tool output.
- **Recipient validation.** Phone numbers must be digits only; JIDs must be on a known WhatsApp server.
- **File permissions.** `store/` is created mode 700 and the databases, token and downloaded media mode 600. `whatsapp.db` holds your Signal session keys: anyone who can read it can act as your WhatsApp account.
- **No message database.** Messages are cached in the bridge's memory and never written to disk. The MCP server has no data access of its own; every read goes through the bridge's authenticated API, so there is one enforcement point for policy, limits and logging. Pagination and fetch sizes from the agent are clamped server-side.
- **Quiet logs.** The bridge logs message metadata (chat, direction, type), never message bodies.
- **Read-only mode.** Set `WHATSAPP_MCP_READ_ONLY=1` in the MCP server's environment and the send tools are not registered at all. Use this if you only want the agent to read and search.
- **Remote mode is OAuth-only.** In HTTP mode every MCP request needs a bearer token issued by the built-in authorization server after you approve the connection with a console code or password; see [Remote access from claude.ai](#remote-access-from-claudeai). Health and OAuth discovery are the only unauthenticated routes.
- **Tool annotations.** Read tools are marked `readOnlyHint`; send tools are marked `openWorldHint` so clients that support it can ask for confirmation.
- **Dependencies.** Both the Go and Python dependency sets are pinned to versions with no known vulnerabilities as of October 2026. A GitHub Actions workflow (`.github/workflows/security.yml`) runs `gofmt`, `go vet`, the Go tests, `govulncheck`, the Python end-to-end tests (stdio and OAuth/HTTP), `pip-audit` and a Docker Compose build on every push and weekly; Dependabot keeps both ecosystems current.

### Running the tests

```bash
cd whatsapp-bridge && go test ./...
cd whatsapp-mcp-server && uv run python tests/e2e_test.py .
cd whatsapp-mcp-server && uv run python tests/oauth_e2e_test.py .
docker compose build
```

The Python tests spin up the MCP server (over stdio, and over HTTP with the full OAuth flow: discovery, registration, PKCE, consent, scopes, refresh rotation, revocation) against a stub bridge, so they need no WhatsApp account.

### Configuration reference

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
| `WHATSAPP_MAX_MEDIA_BYTES` | bridge | `67108864` | Max size of a file to send |
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

### What is not covered

- **Prompt injection.** Message content returned to the agent is untrusted. The server tells the model this in its instructions, but that is advice, not enforcement. Read-only mode, the recipient controls and the media allow-list are the real controls.
- **Encryption at rest of the session.** `whatsapp.db` is not encrypted. Full-disk encryption and the 600/700 permissions are what protect it, and whoever copies it can request your history from your phone just as the bridge does. Keeping messages out of a database reduces what a casual copy of `store/` exposes; it does not change the value of the session file.
- **Process memory.** While the bridge runs, cached messages are plaintext in its memory, like any WhatsApp client.
- **Other processes running as you.** Anything running under your user account can read the token and the session database, or talk to the bridge API.
- **The hosted client.** In remote mode, Anthropic's servers hold a valid token for as long as you keep the connector; messages you ask about pass through claude.ai like any other connector data. Revoke with `oauth.py revoke-all` when you stop using it.

## Usage

Once connected, you can interact with your WhatsApp contacts through Claude, leveraging Claude's AI capabilities in your WhatsApp conversations.

### MCP Tools

Claude can access the following tools to interact with WhatsApp:

- **search_contacts**: Search for contacts by name or phone number
- **list_messages**: Retrieve messages with optional filters and context
- **list_chats**: List available chats with metadata
- **get_chat**: Get information about a specific chat
- **get_direct_chat_by_contact**: Find a direct chat with a specific contact
- **get_contact_chats**: List all chats involving a specific contact
- **get_last_interaction**: Get the most recent message with a contact
- **get_message_context**: Retrieve context around a specific message
- **fetch_older_messages**: Ask your phone for older messages in a chat and add them to the bridge's cache (phone must be online)
- **send_message**: Send a WhatsApp message to a specified phone number or group JID
- **send_file**: Send a file (image, video, raw audio, document) to a specified recipient
- **send_audio_message**: Send an audio file as a WhatsApp voice message (requires the file to be an .ogg opus file or ffmpeg must be installed)
- **download_media**: Download media from a WhatsApp message and get the local file path

### Media Handling Features

The MCP server supports both sending and receiving various media types:

#### Media Sending

You can send various media types to your WhatsApp contacts:

- **Images, Videos, Documents**: Use the `send_file` tool to share any supported media type. Files must live under `whatsapp-bridge/store/` or a directory listed in `WHATSAPP_MEDIA_DIRS` (see [Security](#security)).
- **Voice Messages**: Use the `send_audio_message` tool to send audio files as playable WhatsApp voice messages.
  - For optimal compatibility, audio files should be in `.ogg` Opus format.
  - With FFmpeg installed, the system will automatically convert other audio formats (MP3, WAV, etc.) to the required format.
  - Without FFmpeg, you can still send raw audio files using the `send_file` tool, but they won't appear as playable voice messages.

#### Media Downloading

By default, only the media's metadata and decryption info are held in the bridge's memory alongside the message. The message will indicate that media was sent. To access this media you need to use the download_media tool which takes the `message_id` and `chat_jid` (which are shown when printing messages containing the media); this downloads the file into `whatsapp-bridge/store/` and returns the path, which can then be opened or passed to another tool. Downloaded files are the only message content that ever reaches disk, and only because you asked for them.

## Technical Details

1. Claude sends requests to the Python MCP server
2. The MCP server calls the Go bridge's authenticated loopback REST API for every read and write
3. The Go bridge keeps a bounded in-memory cache fed by live WhatsApp events and by on-demand history requests to your phone
4. Data flows back through the chain to Claude
5. When sending messages, the request flows from Claude through the MCP server to the Go bridge and to WhatsApp

## Troubleshooting

- If you encounter permission issues when running uv, you may need to add it to your PATH or use the full path to the executable.
- Make sure both the Go application and the Python server are running for the integration to work properly.
- **"Bridge API token not found"** or **HTTP 401**: start the bridge once so it creates `whatsapp-bridge/store/api_token`, or set the same `WHATSAPP_API_TOKEN` for both the bridge and the MCP server. If you moved the store directory, set `WHATSAPP_BRIDGE_STORE` for the MCP server.
- **"media_path is outside the allowed media directories"**: add the directory to `WHATSAPP_MEDIA_DIRS` for both the bridge and the MCP server, then restart both.
- **"Send rate limit exceeded"**: the bridge caps sends per minute; raise `WHATSAPP_SEND_RATE_LIMIT` if you need more.
- **"recipient ... is not in WHATSAPP_ALLOWED_RECIPIENTS"** or **"is not an existing chat"**: the bridge's recipient policy refused the send. Adjust `WHATSAPP_ALLOWED_RECIPIENTS` / `WHATSAPP_SEND_KNOWN_CHATS_ONLY` and restart the bridge.

- **claude.ai says the connector failed to authorize**: check `docker compose logs mcp` for `client_rejected` (the callback is not on `WHATSAPP_MCP_ALLOWED_REDIRECTS`), make sure `WHATSAPP_MCP_PUBLIC_URL` matches the hostname in the browser exactly (scheme and port included), and that your proxy forwards the original `Host` header; a mismatch is rejected as DNS rebinding.
- **"This connection was authorized without the 'whatsapp:send' scope"**: you left the send box unticked on the consent page. Remove and re-add the connector in claude.ai, then tick it (or keep it off and use the connector read-only).
- **Lost the approval code**: it is printed once per request; start the connection again from claude.ai for a new one, or set `WHATSAPP_MCP_OWNER_PASSWORD_HASH`.

### Authentication Issues

- **QR Code Not Displaying**: If the QR code doesn't appear, try restarting the authentication script. If issues persist, check if your terminal supports displaying QR codes.
- **WhatsApp Already Logged In**: If your session is already active, the Go bridge will automatically reconnect without showing a QR code.
- **Device Limit Reached**: WhatsApp limits the number of linked devices. If you reach this limit, you'll need to remove an existing device from WhatsApp on your phone (Settings > Linked Devices).
- **No Messages Loading**: Right after pairing, the phone pushes recent history and it can take a few minutes to arrive. After a restart the cache starts empty: new messages appear as they arrive, and `fetch_older_messages` pulls older ones for a chat from the phone.
- **"no cached messages for this chat to anchor the request"**: the bridge can only ask the phone for messages older than one it has already seen. Wait for a message in that chat, or make sure `WHATSAPP_PERSIST_CHAT_INDEX` is on so anchors survive restarts.
- **"timed out waiting for the phone"**: the phone is offline, asleep with background data off, or WhatsApp was force-stopped. Open WhatsApp on the phone and retry.
- **WhatsApp Out of Sync**: If the bridge cannot connect or decrypt, delete `whatsapp-bridge/store/whatsapp.db` and restart the bridge to re-authenticate.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues). The documentation includes helpful tips for checking logs and resolving common issues.
