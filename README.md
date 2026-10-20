# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal Whatsapp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to your **personal WhatsApp account** directly via the Whatsapp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

Here's an example of what you can do when it's connected to Claude.

![WhatsApp MCP](./example-use.png)

> To get updates on this and other projects I work on [enter your email here](https://docs.google.com/forms/d/1rTF9wMBTN0vPfzWuQa2BjfGKdKIpTbyeKxhPMcEzgyI/preview)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that project injection could lead to private data exfiltration.

## Installation

### Prerequisites

- Go 1.26+ (the bridge pins a patched toolchain in `go.mod`; `go` downloads it automatically)
- Python 3.11+
- Anthropic Claude Desktop app (or Cursor)
- UV (Python package manager), install with `curl -LsSf https://astral.sh/uv/install.sh | sh`
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

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): A Go application that connects to WhatsApp's web API, handles authentication via QR code, and stores message history in SQLite. It serves as the bridge between WhatsApp and the MCP server.

2. **Python MCP Server** (`whatsapp-mcp-server/`): A Python server implementing the Model Context Protocol (MCP), which provides standardized tools for Claude to interact with WhatsApp data and send/receive messages.

### Data Storage

- All message history is stored in a SQLite database within the `whatsapp-bridge/store/` directory
- The database maintains tables for chats and messages
- Messages are indexed for efficient searching and retrieval

## Security

This project gives an AI agent read access to your private messages and the ability to send messages as you. The design below limits what can go wrong, but read the caution at the top of this README: an attacker who can message you can try to prompt-inject the agent.

### What is enforced

- **Loopback only.** The bridge's REST API binds to `127.0.0.1` and rejects requests whose `Host` header is not a loopback address (blocks DNS-rebinding). It also requires `Content-Type: application/json` and a bearer token, which together stop a web page in your browser from calling it.
- **Bearer token.** Generated on first run into `whatsapp-bridge/store/api_token` (mode 600). Every API call must present it. The MCP server reads the file; set `WHATSAPP_API_TOKEN` on both sides to use your own.
- **Media allow-list.** Files can only be sent from `whatsapp-bridge/store/` plus directories listed in `WHATSAPP_MEDIA_DIRS` (colon-separated on macOS/Linux, semicolon on Windows). Symlinks are resolved before checking. The bridge enforces this independently of the MCP server, and the check runs before any filesystem access so the API cannot be used to probe for files. Without this, a prompt-injected agent could send `~/.ssh/id_rsa` to an attacker.
- **Safe downloads.** Filenames chosen by remote senders are sanitized and downloads are confined to `whatsapp-bridge/store/`.
- **Rate limit.** At most `WHATSAPP_SEND_RATE_LIMIT` sends per minute (default 20, 0 disables).
- **Size limits.** Media larger than `WHATSAPP_MAX_MEDIA_BYTES` (default 64 MiB) and messages over 64k characters are rejected; request bodies are capped at 1 MiB; the HTTP server has read/write/idle timeouts.
- **Recipient validation.** Phone numbers must be digits only; JIDs must be on a known WhatsApp server.
- **File permissions.** `store/` is created mode 700 and the databases, token and downloaded media mode 600. `whatsapp.db` holds your Signal session keys: anyone who can read it can act as your WhatsApp account.
- **Read-only database access.** The MCP server opens `messages.db` read-only; pagination arguments from the agent are clamped.
- **Quiet logs.** The bridge logs message metadata (chat, direction, type), never message bodies.
- **Read-only mode.** Set `WHATSAPP_MCP_READ_ONLY=1` in the MCP server's environment and the send tools are not registered at all. Use this if you only want the agent to read and search.
- **Tool annotations.** Read tools are marked `readOnlyHint`; send tools are marked `openWorldHint` so clients that support it can ask for confirmation.
- **Dependencies.** Both the Go and Python dependency sets are pinned to versions with no known vulnerabilities as of October 2026 (`govulncheck` and `pip-audit` clean).

### Configuration reference

| Variable | Where | Default | Purpose |
|---|---|---|---|
| `WHATSAPP_API_TOKEN` | both | file `store/api_token` | Shared secret for the bridge API (min 32 chars) |
| `WHATSAPP_MEDIA_DIRS` | both | *(none)* | Extra directories files may be sent from |
| `WHATSAPP_MCP_READ_ONLY` | MCP server | `0` | `1` disables all send tools |
| `WHATSAPP_BRIDGE_ADDR` | bridge | `127.0.0.1:8080` | Listen address. Non-loopback values disable the Host check and expose the API to the network; not recommended |
| `WHATSAPP_API_BASE_URL` | MCP server | `http://127.0.0.1:8080/api` | Where the MCP server finds the bridge |
| `WHATSAPP_BRIDGE_STORE` | MCP server | `../whatsapp-bridge/store` | Path to the bridge store directory |
| `WHATSAPP_SEND_RATE_LIMIT` | bridge | `20` | Max sends per minute, `0` to disable |
| `WHATSAPP_MAX_MEDIA_BYTES` | bridge | `67108864` | Max size of a file to send |
| `WHATSAPP_API_TIMEOUT` | MCP server | `180` | Seconds to wait for the bridge |

### What is not covered

- **Prompt injection.** Message content returned to the agent is untrusted. The server tells the model this in its instructions, but that is advice, not enforcement. Read-only mode and the media allow-list are the real controls.
- **Encryption at rest.** The SQLite databases are not encrypted. Full-disk encryption and the 600/700 permissions are what protect them.
- **Other processes running as you.** Anything running under your user account can read the token and the databases.

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

By default, just the metadata of the media is stored in the local database. The message will indicate that media was sent. To access this media you need to use the download_media tool which takes the `message_id` and `chat_jid` (which are shown when printing messages containing the meda), this downloads the media and then returns the file path which can be then opened or passed to another tool.

## Technical Details

1. Claude sends requests to the Python MCP server
2. The MCP server queries the Go bridge for WhatsApp data or directly to the SQLite database
3. The Go accesses the WhatsApp API and keeps the SQLite database up to date
4. Data flows back through the chain to Claude
5. When sending messages, the request flows from Claude through the MCP server to the Go bridge and to WhatsApp

## Troubleshooting

- If you encounter permission issues when running uv, you may need to add it to your PATH or use the full path to the executable.
- Make sure both the Go application and the Python server are running for the integration to work properly.
- **"Bridge API token not found"** or **HTTP 401**: start the bridge once so it creates `whatsapp-bridge/store/api_token`, or set the same `WHATSAPP_API_TOKEN` for both the bridge and the MCP server. If you moved the store directory, set `WHATSAPP_BRIDGE_STORE` for the MCP server.
- **"media_path is outside the allowed media directories"**: add the directory to `WHATSAPP_MEDIA_DIRS` for both the bridge and the MCP server, then restart both.
- **"Send rate limit exceeded"**: the bridge caps sends per minute; raise `WHATSAPP_SEND_RATE_LIMIT` if you need more.

### Authentication Issues

- **QR Code Not Displaying**: If the QR code doesn't appear, try restarting the authentication script. If issues persist, check if your terminal supports displaying QR codes.
- **WhatsApp Already Logged In**: If your session is already active, the Go bridge will automatically reconnect without showing a QR code.
- **Device Limit Reached**: WhatsApp limits the number of linked devices. If you reach this limit, you'll need to remove an existing device from WhatsApp on your phone (Settings > Linked Devices).
- **No Messages Loading**: After initial authentication, it can take several minutes for your message history to load, especially if you have many chats.
- **WhatsApp Out of Sync**: If your WhatsApp messages get out of sync with the bridge, delete both database files (`whatsapp-bridge/store/messages.db` and `whatsapp-bridge/store/whatsapp.db`) and restart the bridge to re-authenticate.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues). The documentation includes helpful tips for checking logs and resolving common issues.
