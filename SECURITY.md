# Security Policy

## Threat model in one paragraph

This project links an AI agent to your personal WhatsApp account. The agent can read every stored message and, unless disabled, send messages and files as you. The main risks are (1) another process or machine reaching the bridge API, (2) a prompt-injected agent sending private data to an attacker, and (3) someone copying `whatsapp-bridge/store/`, which holds your session keys and message history. The controls for each are described in the [Security section of the README](README.md#security).

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
- Keep `whatsapp-bridge/store/` out of backups that leave the machine unencrypted, and never commit it.
