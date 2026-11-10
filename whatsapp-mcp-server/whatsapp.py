"""Thin client for the WhatsApp bridge's authenticated loopback REST API.

The MCP server holds no data of its own: every read and write goes to the
bridge, which keeps messages in memory only. This module never opens a
database or reads message content from disk.
"""
import logging
import os
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

import requests
from mcp.server.mcpserver.exceptions import ToolError

import audio

# The MCP transport uses stdout for protocol messages, so all diagnostics go
# to stderr via logging. Never print() from this module.
logger = logging.getLogger("whatsapp-mcp")

_HERE = Path(__file__).resolve().parent

# Where the Go bridge keeps its session database, API token and downloaded
# media. The MCP server only needs it for the token file and as the default
# media allow-list entry.
BRIDGE_STORE_DIR = Path(
    os.environ.get("WHATSAPP_BRIDGE_STORE", str(_HERE.parent / "whatsapp-bridge" / "store"))
).expanduser().resolve()
API_TOKEN_FILE = BRIDGE_STORE_DIR / "api_token"
OUTBOX_DIR = BRIDGE_STORE_DIR / "outbox"

WHATSAPP_API_BASE_URL = os.environ.get("WHATSAPP_API_BASE_URL", "http://127.0.0.1:8080/api").rstrip("/")

# (connect timeout, read timeout) in seconds. Media uploads and requests that
# wait for the phone can take a while.
REQUEST_TIMEOUT = (5.0, float(os.environ.get("WHATSAPP_API_TIMEOUT", "180")))


class BridgeError(ToolError):
    """Raised when the bridge is unreachable or refuses a read request.

    Subclassing the SDK's ToolError makes the message visible to the agent;
    other exceptions are masked as a generic "Error executing tool".
    """


# ---------------------------------------------------------------------------
# HTTP plumbing
# ---------------------------------------------------------------------------


def _api_token() -> str:
    token = os.environ.get("WHATSAPP_API_TOKEN", "").strip()
    if token:
        return token
    try:
        return API_TOKEN_FILE.read_text(encoding="utf-8").strip()
    except OSError:
        return ""


_session: Optional[requests.Session] = None


def _http() -> requests.Session:
    """A session that ignores environment proxies and .netrc.

    HTTP_PROXY / .netrc would otherwise be able to redirect loopback traffic,
    along with the bearer token, somewhere else.
    """
    global _session
    if _session is None:
        s = requests.Session()
        s.trust_env = False
        _session = s
    return _session


def _api_post(path: str, payload: Dict[str, Any]) -> Tuple[bool, Optional[Dict[str, Any]], str]:
    """POST to the bridge. Returns (ok, parsed_json, error_message)."""
    token = _api_token()
    if not token:
        return (
            False,
            None,
            "Bridge API token not found. Start the bridge once so it creates "
            f"{API_TOKEN_FILE}, or set WHATSAPP_API_TOKEN for this server.",
        )
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
    }
    try:
        response = _http().post(
            f"{WHATSAPP_API_BASE_URL}/{path}",
            json=payload,
            headers=headers,
            timeout=REQUEST_TIMEOUT,
            allow_redirects=False,
        )
    except requests.RequestException as e:
        return False, None, f"Could not reach the WhatsApp bridge: {e}"

    try:
        data = response.json()
    except ValueError:
        data = None

    if response.status_code == 200 and isinstance(data, dict):
        return True, data, ""

    if isinstance(data, dict) and data.get("message"):
        return False, data, str(data["message"])
    return False, data, f"Bridge returned HTTP {response.status_code}: {response.text[:200]}"


def _read(path: str, payload: Dict[str, Any]) -> Dict[str, Any]:
    """POST for a read operation; raise BridgeError on failure."""
    ok, data, err = _api_post(path, payload)
    if not ok or data is None:
        raise BridgeError(err or "Unknown bridge error")
    return data


# ---------------------------------------------------------------------------
# Formatting (all inputs are untrusted remote content)
# ---------------------------------------------------------------------------


def _parse_ts(value: Any) -> Optional[datetime]:
    if value is None:
        return None
    if isinstance(value, datetime):
        return value
    try:
        return datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except ValueError:
        return None


def _display_name(name: Optional[str], limit: int = 80) -> str:
    """Flatten a remote-controlled name onto one printable line.

    Contact and group names are chosen by other people. Without this, a name
    containing a newline could forge a whole fake message line in the output.
    """
    if not name:
        return ""
    cleaned = "".join(ch if ch.isprintable() else " " for ch in str(name))
    cleaned = " ".join(cleaned.split())
    return cleaned[:limit]


def _display_content(content: Optional[str]) -> str:
    """Indent continuation lines so message bodies cannot start a fake header line."""
    if not content:
        return ""
    return str(content).replace("\r", "").replace("\n", "\n    ")


def format_message(message: Dict[str, Any], show_chat_info: bool = True) -> str:
    """Format one message dict as returned by the bridge."""
    ts = _parse_ts(message.get("timestamp"))
    stamp = f"{ts:%Y-%m-%d %H:%M:%S}" if ts else "unknown time"
    output = f"[{stamp}] "

    chat_name = _display_name(message.get("chat_name"))
    if show_chat_info and chat_name:
        output += f"Chat: {chat_name} "

    content_prefix = ""
    if message.get("media_type"):
        media_type = _display_name(message.get("media_type"), 20)
        content_prefix = (
            f"[{media_type} - Message ID: {_display_name(message.get('id'))} "
            f"- Chat JID: {_display_name(message.get('chat_jid'))}] "
        )

    if message.get("is_from_me"):
        sender_name = "Me"
    else:
        sender_name = _display_name(message.get("sender_name") or message.get("sender"))
    output += f"From: {sender_name}: {content_prefix}{_display_content(message.get('content'))}\n"
    return output


def format_messages_list(messages: List[Dict[str, Any]], show_chat_info: bool = True) -> str:
    if not messages:
        return "No messages to display."
    return "".join(format_message(m, show_chat_info) for m in messages)


# ---------------------------------------------------------------------------
# Read operations
# ---------------------------------------------------------------------------


def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1,
) -> str:
    """Get cached messages matching the criteria, formatted for the agent."""
    payload: Dict[str, Any] = {
        "limit": int(limit),
        "page": int(page),
        "include_context": bool(include_context),
        "context_before": int(context_before),
        "context_after": int(context_after),
    }
    if after:
        payload["after"] = str(after)
    if before:
        payload["before"] = str(before)
    if sender_phone_number:
        payload["sender"] = str(sender_phone_number)
    if chat_jid:
        payload["chat_jid"] = str(chat_jid)
    if query:
        payload["query"] = str(query)

    try:
        data = _read("messages", payload)
    except BridgeError as e:
        return f"Error: {e}"

    messages = data.get("messages") or []
    text = format_messages_list(messages, show_chat_info=True)
    if not messages:
        text += (
            " The bridge only caches messages received while it is running, plus what has been "
            "fetched from the phone. Use fetch_older_messages for a chat to pull more history."
        )
    return text


def get_message_context(message_id: str, before: int = 5, after: int = 5, chat_jid: Optional[str] = None) -> Dict[str, Any]:
    """Get a message with the messages around it in the same chat."""
    payload: Dict[str, Any] = {"message_id": str(message_id), "before": int(before), "after": int(after)}
    if chat_jid:
        payload["chat_jid"] = str(chat_jid)
    data = _read("message_context", payload)
    return {
        "message": data.get("message"),
        "before": data.get("before") or [],
        "after": data.get("after") or [],
    }


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
) -> List[Dict[str, Any]]:
    data = _read(
        "chats",
        {
            "query": str(query or ""),
            "limit": int(limit),
            "page": int(page),
            "include_last_message": bool(include_last_message),
            "sort_by": "name" if sort_by == "name" else "last_active",
        },
    )
    return data.get("chats") or []


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Dict[str, Any]]:
    data = _read("chat", {"chat_jid": str(chat_jid), "include_last_message": bool(include_last_message)})
    return data.get("chat")


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Dict[str, Any]]:
    needle = str(sender_phone_number or "").strip()
    if not needle:
        return None
    data = _read("chats", {"query": needle, "limit": 1, "page": 0, "direct_only": True, "include_last_message": True})
    chats = data.get("chats") or []
    return chats[0] if chats else None


def search_contacts(query: str) -> List[Dict[str, Any]]:
    data = _read("contacts", {"query": str(query or "")})
    return data.get("contacts") or []


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    data = _read("contact_chats", {"jid": str(jid), "limit": int(limit), "page": int(page)})
    return data.get("chats") or []


def get_last_interaction(jid: str) -> Optional[str]:
    data = _read("last_interaction", {"jid": str(jid)})
    message = data.get("message")
    if not message:
        return None
    return format_message(message)


def fetch_older_messages(chat_jid: str, count: int = 50) -> Tuple[bool, str, int]:
    """Ask the phone for older messages in a chat. Returns (ok, message, added)."""
    if not chat_jid:
        return False, "chat_jid must be provided", 0
    ok, data, err = _api_post("history", {"chat_jid": str(chat_jid), "count": int(count)})
    if not ok:
        return False, err, 0
    return bool(data.get("success", False)), str(data.get("message", "")), int(data.get("added", 0) or 0)


# ---------------------------------------------------------------------------
# Media path policy (the bridge enforces the same rule independently)
# ---------------------------------------------------------------------------


def _allowed_media_dirs() -> List[Path]:
    dirs = [BRIDGE_STORE_DIR]
    for entry in os.environ.get("WHATSAPP_MEDIA_DIRS", "").split(os.pathsep):
        entry = entry.strip()
        if not entry:
            continue
        try:
            dirs.append(Path(entry).expanduser().resolve(strict=True))
        except OSError:
            logger.warning("Ignoring WHATSAPP_MEDIA_DIRS entry %r: not found", entry)
    return dirs


def _within_allowed(path: Path, dirs: List[Path]) -> bool:
    return any(path == d or d in path.parents for d in dirs)


def validate_media_path(media_path: str) -> Tuple[Optional[str], str]:
    """Check that a media path is allowed to be sent.

    The allow-list check happens before touching the filesystem so this
    cannot be used to probe for the existence of arbitrary files.
    """
    if not media_path:
        return None, "Media path must be provided"
    p = Path(str(media_path))
    if not p.is_absolute():
        return None, "Media path must be an absolute path"

    dirs = _allowed_media_dirs()
    clean = Path(os.path.normpath(p))
    if not _within_allowed(clean, dirs):
        return None, (
            "Media path is outside the allowed media directories. Allowed: "
            + ", ".join(str(d) for d in dirs)
            + ". Set WHATSAPP_MEDIA_DIRS (for both the bridge and this server) to allow more."
        )
    try:
        real = clean.resolve(strict=True)
    except OSError:
        return None, "Media file not found"
    if not _within_allowed(real, dirs):
        return None, "Media path resolves outside the allowed media directories"
    if not real.is_file():
        return None, "Media path is not a regular file"
    return str(real), ""


# ---------------------------------------------------------------------------
# Write operations
# ---------------------------------------------------------------------------


def _send(payload: Dict[str, Any]) -> Tuple[bool, str]:
    ok, data, err = _api_post("send", payload)
    if not ok:
        return False, err
    return bool(data.get("success", False)), str(data.get("message", "Unknown response"))


def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    if not message:
        return False, "Message must be provided"
    return _send({"recipient": recipient, "message": message})


def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    resolved, err = validate_media_path(media_path)
    if resolved is None:
        return False, err
    return _send({"recipient": recipient, "media_path": resolved})


def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    resolved, err = validate_media_path(media_path)
    if resolved is None:
        return False, err

    temp_path: Optional[str] = None
    try:
        if not resolved.lower().endswith(".ogg"):
            try:
                OUTBOX_DIR.mkdir(mode=0o700, exist_ok=True)
                temp_path = audio.convert_to_opus_ogg_temp(resolved, output_dir=str(OUTBOX_DIR))
                resolved = temp_path
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {e}"
        return _send({"recipient": recipient, "media_path": resolved})
    finally:
        if temp_path:
            try:
                os.unlink(temp_path)
            except OSError:
                pass


def download_media(message_id: str, chat_jid: str) -> Optional[str]:
    """Download media from a cached message and return the local file path."""
    if not message_id or not chat_jid:
        return None
    ok, data, err = _api_post("download", {"message_id": str(message_id), "chat_jid": str(chat_jid)})
    if not ok:
        logger.error("Download failed: %s", err)
        return None
    if not data.get("success", False):
        logger.error("Download failed: %s", data.get("message", "Unknown error"))
        return None
    return data.get("path")
