# Security Policy

## Threat model in one paragraph

This project links an AI agent to your personal WhatsApp account. The agent can read every cached message and, unless disabled, send messages and files as you. The main risks are (1) another process or machine reaching the bridge API, (2) a prompt-injected agent sending private data to an attacker, (3) someone copying `whatsapp-bridge/store/`, which holds your session keys, and, in remote (HTTP) mode, (4) someone on the internet obtaining an OAuth token. Remote mode is guarded by a built-in OAuth 2.1 server: owner approval with a console code or password, redirect allow-list, mandatory PKCE, scoped short-lived hashed tokens with rotation and revocation. Messages themselves are kept only in the bridge's memory; the on-disk chat index holds metadata, never content. The controls for each risk are described in the [Security section of the README](README.md#security).

## Reporting a vulnerability

Please do not open a public issue for security problems. Email the maintainer listed in the repository profile, or use GitHub's private vulnerability reporting on this repository if it is enabled. Include steps to reproduce and the commit you tested against. You should hear back within a week.

## Supported versions

Only the `main` branch is supported. Dependencies are scanned weekly in CI with `govulncheck` and `pip-audit`; update to the latest `main` to pick up fixes.

## Hardening checklist for operators

- Run the bridge and MCP server as your own user on a machine with full-disk encryption.
- Leave `WHATSAPP_BRIDGE_ADDR` at its loopback default.
- Set `WHATSAPP_MCP_READ_ONLY=1` unless you need the agent to send.
- If you do need sending, set `WHATSAPP_ALLOWED_RECIPIENTS` or `WHATSAPP_SEND_KNOWN_CHATS_ONLY=1`, and only list directories you intend to share in `WHATSAPP_MEDIA_DIRS`.
- Review `whatsapp-bridge/store/send_audit.log` periodically.
- Set `WHATSAPP_PERSIST_CHAT_INDEX=0` if you do not even want chat names and JIDs on disk; accept that history fetching then only works for chats active since the bridge started.
- Delete any leftover `whatsapp-bridge/store/messages.db` from versions before the memory-only cache.
- Keep `whatsapp-bridge/store/` out of backups that leave the machine unencrypted, and never commit it.

Remote mode (claude.ai) additionally:

- Terminate TLS in front of the server (tunnel or reverse proxy) and never set `WHATSAPP_MCP_ALLOW_INSECURE_HTTP` outside local tests.
- Keep `WHATSAPP_MCP_ALLOWED_REDIRECTS` at the claude.ai defaults unless you run another trusted client.
- Approve connections only when you started them yourself; click Deny on any consent page you did not expect and check `store/mcp_auth_audit.log`.
- Grant `whatsapp:send` only if you need it; `WHATSAPP_MCP_READ_ONLY=1` removes it entirely.
- Run `python oauth.py status` occasionally and `python oauth.py revoke-all` when a device or the connector is no longer in use.
- If you use the Docker setup, keep the `store` volume on an encrypted disk and the published port on `127.0.0.1`.
