import logging
import os
import sqlite3
import tempfile
from dataclasses import dataclass, asdict
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

import requests

import audio

# The MCP transport uses stdout for protocol messages, so all diagnostics go
# to stderr via logging. Never print() from this module.
logger = logging.getLogger("whatsapp-mcp")

_HERE = Path(__file__).resolve().parent

# Where the Go bridge keeps its databases, downloaded media and API token.
BRIDGE_STORE_DIR = Path(
    os.environ.get("WHATSAPP_BRIDGE_STORE", str(_HERE.parent / "whatsapp-bridge" / "store"))
).expanduser().resolve()
MESSAGES_DB_PATH = BRIDGE_STORE_DIR / "messages.db"
API_TOKEN_FILE = BRIDGE_STORE_DIR / "api_token"
OUTBOX_DIR = BRIDGE_STORE_DIR / "outbox"

WHATSAPP_API_BASE_URL = os.environ.get("WHATSAPP_API_BASE_URL", "http://127.0.0.1:8080/api").rstrip("/")

# (connect timeout, read timeout) in seconds. Media uploads can take a while.
REQUEST_TIMEOUT = (5.0, float(os.environ.get("WHATSAPP_API_TIMEOUT", "180")))

# Hard caps on agent-supplied pagination values. Without these a negative
# LIMIT returns the entire table and a huge OFFSET is a free full-table scan.
MAX_LIMIT = 100
MAX_PAGE = 10_000
MAX_CONTEXT = 20
MAX_QUERY_LENGTH = 500

# ---------------------------------------------------------------------------
# Data classes
# ---------------------------------------------------------------------------


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None


@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")


@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str


@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]


def chat_to_dict(chat: Chat) -> Dict[str, Any]:
    d = asdict(chat)
    d["is_group"] = chat.is_group
    return d


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _clamp_int(value: Any, lo: int, hi: int, default: int) -> int:
    try:
        n = int(value)
    except (TypeError, ValueError):
        return default
    return max(lo, min(hi, n))


def _clean_query(query: Optional[str]) -> Optional[str]:
    if not query:
        return None
    query = str(query)[:MAX_QUERY_LENGTH]
    return query or None


def _connect() -> sqlite3.Connection:
    """Open the message database read-only.

    The MCP server only ever reads; opening with mode=ro means a bug or a
    prompt-injected agent cannot alter or delete message history through it.
    """
    uri = f"{MESSAGES_DB_PATH.as_uri()}?mode=ro"
    return sqlite3.connect(uri, uri=True)


def _parse_ts(value: Any) -> Optional[datetime]:
    if value is None:
        return None
    if isinstance(value, datetime):
        return value
    try:
        return datetime.fromisoformat(str(value))
    except ValueError:
        return None


def _row_to_message(row: Tuple[Any, ...]) -> Message:
    return Message(
        timestamp=_parse_ts(row[0]) or datetime.min,
        sender=row[1],
        chat_name=row[2],
        content=row[3] or "",
        is_from_me=bool(row[4]),
        chat_jid=row[5],
        id=row[6],
        media_type=row[7],
    )


def _row_to_chat(row: Tuple[Any, ...]) -> Chat:
    return Chat(
        jid=row[0],
        name=row[1],
        last_message_time=_parse_ts(row[2]),
        last_message=row[3],
        last_sender=row[4],
        last_is_from_me=None if row[5] is None else bool(row[5]),
    )


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

    Returns (resolved_path, "") on success or (None, error) on failure. The
    allow-list check happens before touching the filesystem so this cannot
    be used to probe for the existence of arbitrary files. The bridge
    performs the same check independently.
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


def get_sender_name(sender_jid: str) -> str:
    try:
        with _connect() as conn:
            cursor = conn.cursor()
            cursor.execute("SELECT name FROM chats WHERE jid = ? LIMIT 1", (sender_jid,))
            result = cursor.fetchone()

            if not result:
                phone_part = sender_jid.split("@")[0] if "@" in sender_jid else sender_jid
                cursor.execute("SELECT name FROM chats WHERE jid LIKE ? LIMIT 1", (f"%{phone_part}%",))
                result = cursor.fetchone()

            if result and result[0]:
                return result[0]
            return sender_jid
    except sqlite3.Error as e:
        logger.error("Database error while getting sender name: %s", e)
        return sender_jid


def format_message(message: Message, show_chat_info: bool = True) -> str:
    """Format a single message. Message bodies are untrusted remote input."""
    output = ""

    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "

    content_prefix = ""
    if message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "

    sender_name = "Me" if message.is_from_me else get_sender_name(message.sender)
    output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    return output


def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> str:
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
    """Get messages matching the specified criteria with optional context."""
    limit = _clamp_int(limit, 1, MAX_LIMIT, 20)
    page = _clamp_int(page, 0, MAX_PAGE, 0)
    context_before = _clamp_int(context_before, 0, MAX_CONTEXT, 1)
    context_after = _clamp_int(context_after, 0, MAX_CONTEXT, 1)
    query = _clean_query(query)

    try:
        with _connect() as conn:
            cursor = conn.cursor()

            query_parts = [
                "SELECT messages.timestamp, messages.sender, chats.name, messages.content, "
                "messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages",
                "JOIN chats ON messages.chat_jid = chats.jid",
            ]
            where_clauses = []
            params: List[Any] = []

            if after:
                after_dt = _parse_ts(after)
                if after_dt is None:
                    raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
                where_clauses.append("messages.timestamp > ?")
                params.append(after_dt)

            if before:
                before_dt = _parse_ts(before)
                if before_dt is None:
                    raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
                where_clauses.append("messages.timestamp < ?")
                params.append(before_dt)

            if sender_phone_number:
                where_clauses.append("messages.sender = ?")
                params.append(str(sender_phone_number))

            if chat_jid:
                where_clauses.append("messages.chat_jid = ?")
                params.append(str(chat_jid))

            if query:
                where_clauses.append("LOWER(messages.content) LIKE LOWER(?)")
                params.append(f"%{query}%")

            if where_clauses:
                query_parts.append("WHERE " + " AND ".join(where_clauses))

            query_parts.append("ORDER BY messages.timestamp DESC")
            query_parts.append("LIMIT ? OFFSET ?")
            params.extend([limit, page * limit])

            cursor.execute(" ".join(query_parts), tuple(params))
            result = [_row_to_message(row) for row in cursor.fetchall()]

        if include_context and result:
            messages_with_context: List[Message] = []
            for msg in result:
                context = get_message_context(msg.id, context_before, context_after)
                messages_with_context.extend(context.before)
                messages_with_context.append(context.message)
                messages_with_context.extend(context.after)
            return format_messages_list(messages_with_context, show_chat_info=True)

        return format_messages_list(result, show_chat_info=True)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return "Error: could not read the message database. Is the bridge running?"


def get_message_context(message_id: str, before: int = 5, after: int = 5) -> MessageContext:
    """Get context around a specific message."""
    before = _clamp_int(before, 0, MAX_CONTEXT, 5)
    after = _clamp_int(after, 0, MAX_CONTEXT, 5)

    with _connect() as conn:
        cursor = conn.cursor()

        cursor.execute(
            """
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me,
                   chats.jid, messages.id, messages.media_type, messages.chat_jid
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.id = ?
            """,
            (str(message_id),),
        )
        msg_data = cursor.fetchone()

        if not msg_data:
            raise ValueError(f"Message with ID {message_id} not found")

        target_message = _row_to_message(msg_data)
        target_chat_jid = msg_data[8]
        target_ts = msg_data[0]

        base = (
            "SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, "
            "chats.jid, messages.id, messages.media_type FROM messages "
            "JOIN chats ON messages.chat_jid = chats.jid "
        )

        cursor.execute(
            base + "WHERE messages.chat_jid = ? AND messages.timestamp < ? ORDER BY messages.timestamp DESC LIMIT ?",
            (target_chat_jid, target_ts, before),
        )
        before_messages = [_row_to_message(row) for row in cursor.fetchall()]

        cursor.execute(
            base + "WHERE messages.chat_jid = ? AND messages.timestamp > ? ORDER BY messages.timestamp ASC LIMIT ?",
            (target_chat_jid, target_ts, after),
        )
        after_messages = [_row_to_message(row) for row in cursor.fetchall()]

    return MessageContext(message=target_message, before=before_messages, after=after_messages)


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    limit = _clamp_int(limit, 1, MAX_LIMIT, 20)
    page = _clamp_int(page, 0, MAX_PAGE, 0)
    query = _clean_query(query)

    try:
        with _connect() as conn:
            cursor = conn.cursor()

            query_parts = [
                """
                SELECT
                    chats.jid,
                    chats.name,
                    chats.last_message_time,
                    messages.content as last_message,
                    messages.sender as last_sender,
                    messages.is_from_me as last_is_from_me
                FROM chats
                """
            ]

            if include_last_message:
                query_parts.append(
                    "LEFT JOIN messages ON chats.jid = messages.chat_jid "
                    "AND chats.last_message_time = messages.timestamp"
                )
            else:
                # Keep the column list stable without joining.
                query_parts[0] = """
                SELECT chats.jid, chats.name, chats.last_message_time, NULL, NULL, NULL
                FROM chats
                """

            params: List[Any] = []
            if query:
                query_parts.append("WHERE (LOWER(chats.name) LIKE LOWER(?) OR chats.jid LIKE ?)")
                params.extend([f"%{query}%", f"%{query}%"])

            # sort_by is mapped to a fixed expression, never interpolated.
            order_by = "chats.name" if sort_by == "name" else "chats.last_message_time DESC"
            query_parts.append(f"ORDER BY {order_by}")
            query_parts.append("LIMIT ? OFFSET ?")
            params.extend([limit, page * limit])

            cursor.execute(" ".join(query_parts), tuple(params))
            return [_row_to_chat(row) for row in cursor.fetchall()]

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []


def search_contacts(query: str) -> List[Contact]:
    """Search contacts by name or phone number."""
    query = _clean_query(query)
    if not query:
        return []

    try:
        with _connect() as conn:
            cursor = conn.cursor()
            search_pattern = f"%{query}%"
            cursor.execute(
                """
                SELECT DISTINCT jid, name
                FROM chats
                WHERE (LOWER(name) LIKE LOWER(?) OR LOWER(jid) LIKE LOWER(?))
                  AND jid NOT LIKE '%@g.us'
                ORDER BY name, jid
                LIMIT 50
                """,
                (search_pattern, search_pattern),
            )
            return [
                Contact(phone_number=row[0].split("@")[0], name=row[1], jid=row[0])
                for row in cursor.fetchall()
            ]
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact."""
    limit = _clamp_int(limit, 1, MAX_LIMIT, 20)
    page = _clamp_int(page, 0, MAX_PAGE, 0)

    try:
        with _connect() as conn:
            cursor = conn.cursor()
            cursor.execute(
                """
                SELECT DISTINCT
                    c.jid,
                    c.name,
                    c.last_message_time,
                    m.content as last_message,
                    m.sender as last_sender,
                    m.is_from_me as last_is_from_me
                FROM chats c
                JOIN messages m ON c.jid = m.chat_jid
                WHERE m.sender = ? OR c.jid = ?
                ORDER BY c.last_message_time DESC
                LIMIT ? OFFSET ?
                """,
                (str(jid), str(jid), limit, page * limit),
            )
            return [_row_to_chat(row) for row in cursor.fetchall()]
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []


def get_last_interaction(jid: str) -> Optional[str]:
    """Get most recent message involving the contact."""
    try:
        with _connect() as conn:
            cursor = conn.cursor()
            cursor.execute(
                """
                SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me, c.jid, m.id, m.media_type
                FROM messages m
                JOIN chats c ON m.chat_jid = c.jid
                WHERE m.sender = ? OR c.jid = ?
                ORDER BY m.timestamp DESC
                LIMIT 1
                """,
                (str(jid), str(jid)),
            )
            msg_data = cursor.fetchone()

        if not msg_data:
            return None
        return format_message(_row_to_message(msg_data))

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            if include_last_message:
                query = """
                    SELECT c.jid, c.name, c.last_message_time,
                           m.content as last_message, m.sender as last_sender, m.is_from_me as last_is_from_me
                    FROM chats c
                    LEFT JOIN messages m ON c.jid = m.chat_jid AND c.last_message_time = m.timestamp
                    WHERE c.jid = ?
                """
            else:
                query = """
                    SELECT c.jid, c.name, c.last_message_time, NULL, NULL, NULL
                    FROM chats c
                    WHERE c.jid = ?
                """

            cursor.execute(query, (str(chat_jid),))
            chat_data = cursor.fetchone()

        if not chat_data:
            return None
        return _row_to_chat(chat_data)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number."""
    needle = _clean_query(sender_phone_number)
    if not needle:
        return None

    try:
        with _connect() as conn:
            cursor = conn.cursor()
            cursor.execute(
                """
                SELECT c.jid, c.name, c.last_message_time,
                       m.content as last_message, m.sender as last_sender, m.is_from_me as last_is_from_me
                FROM chats c
                LEFT JOIN messages m ON c.jid = m.chat_jid AND c.last_message_time = m.timestamp
                WHERE c.jid LIKE ? AND c.jid NOT LIKE '%@g.us'
                LIMIT 1
                """,
                (f"%{needle}%",),
            )
            chat_data = cursor.fetchone()

        if not chat_data:
            return None
        return _row_to_chat(chat_data)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None


# ---------------------------------------------------------------------------
# Write operations (go through the bridge's authenticated REST API)
# ---------------------------------------------------------------------------


def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    if not message:
        return False, "Message must be provided"

    ok, data, err = _api_post("send", {"recipient": recipient, "message": message})
    if not ok:
        return False, err
    return bool(data.get("success", False)), str(data.get("message", "Unknown response"))


def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"

    resolved, err = validate_media_path(media_path)
    if resolved is None:
        return False, err

    ok, data, err = _api_post("send", {"recipient": recipient, "media_path": resolved})
    if not ok:
        return False, err
    return bool(data.get("success", False)), str(data.get("message", "Unknown response"))


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

        ok, data, err = _api_post("send", {"recipient": recipient, "media_path": resolved})
        if not ok:
            return False, err
        return bool(data.get("success", False)), str(data.get("message", "Unknown response"))
    finally:
        if temp_path:
            try:
                os.unlink(temp_path)
            except OSError:
                pass


def download_media(message_id: str, chat_jid: str) -> Optional[str]:
    """Download media from a message and return the local file path."""
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
