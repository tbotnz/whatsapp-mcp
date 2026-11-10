"""Built-in OAuth 2.1 authorization server for remote (HTTP) mode.

Used only when WHATSAPP_MCP_TRANSPORT=http. Design goals, in order:

1. Only the owner can authorize a client. Approving a connection requires a
   one-time code printed on the server's console (or, optionally, an owner
   password configured as a PBKDF2 hash). Nothing on the public page alone
   is enough.
2. Only known clients can take part. Dynamic client registration is open, as
   claude.ai requires, but every redirect URI must be on the allow-list
   (claude.ai's callbacks by default), so a registered client can never
   receive a code anywhere else.
3. Least privilege. Two scopes: whatsapp:read and whatsapp:send. The owner
   can drop the send scope on the consent page even if the client asked.
4. Tokens are opaque, random, short-lived and stored only as SHA-256 hashes.
   Refresh tokens rotate on use. Revocation removes the whole pair.
5. Everything auth-related is audited to store/mcp_auth_audit.log.
"""
import base64
import getpass
import hashlib
import hmac
import html
import json
import logging
import os
import secrets
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional
from urllib.parse import urlparse

from pydantic import AnyHttpUrl, AnyUrl
from starlette.requests import Request
from starlette.responses import HTMLResponse, JSONResponse, RedirectResponse, Response

from mcp.server.auth.middleware.auth_context import get_access_token
from mcp.server.auth.provider import (
    AccessToken,
    AuthorizationCode,
    AuthorizationParams,
    AuthorizeError,
    OAuthAuthorizationServerProvider,
    RefreshToken,
    RegistrationError,
    TokenError,
    construct_redirect_uri,
)
from mcp.server.auth.settings import AuthSettings, ClientRegistrationOptions, RevocationOptions
from mcp.server.mcpserver.exceptions import ToolError
from mcp.server.transport_security import TransportSecuritySettings
from mcp.shared.auth import OAuthClientInformationFull, OAuthToken

logger = logging.getLogger("whatsapp-mcp.oauth")

SCOPE_READ = "whatsapp:read"
SCOPE_SEND = "whatsapp:send"
ALL_SCOPES = [SCOPE_READ, SCOPE_SEND]

DEFAULT_ALLOWED_REDIRECTS = [
    "https://claude.ai/api/mcp/auth_callback",
    "https://claude.com/api/mcp/auth_callback",
]

PAIRING_ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"  # no 0/O/1/I
PENDING_TTL = 300  # seconds an approval code stays valid
CODE_TTL = 300  # seconds an authorization code stays valid
MAX_ATTEMPTS_PER_REQUEST = 5
LOCKOUT_FAILURES = 20
LOCKOUT_WINDOW = 900
MAX_PENDING = 20
MAX_CLIENTS = 50


def _now() -> int:
    return int(time.time())


def _hash(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _normalize_code(value: str) -> str:
    return "".join(ch for ch in value.upper() if ch.isalnum())


# ---------------------------------------------------------------------------
# Owner password hashing (optional alternative to the console code)
# ---------------------------------------------------------------------------


def hash_password(password: str, iterations: int = 600_000) -> str:
    salt = secrets.token_bytes(16)
    dk = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"), salt, iterations)
    return f"pbkdf2_sha256${iterations}${base64.b64encode(salt).decode()}${base64.b64encode(dk).decode()}"


def verify_password(password: str, encoded: str) -> bool:
    try:
        algo, iterations, salt_b64, hash_b64 = encoded.split("$")
        if algo != "pbkdf2_sha256":
            return False
        dk = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"), base64.b64decode(salt_b64), int(iterations))
        return hmac.compare_digest(dk, base64.b64decode(hash_b64))
    except (ValueError, TypeError):
        return False


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------


@dataclass
class HttpConfig:
    public_url: str
    host: str
    port: int
    allowed_redirects: List[str]
    owner_password_hash: Optional[str]
    access_ttl: int
    refresh_ttl: int
    state_path: Path
    audit_path: Path
    allow_send: bool = True  # False in read-only mode: the consent page never offers whatsapp:send

    @property
    def resource_url(self) -> str:
        return self.public_url + "/mcp"

    @property
    def public_host(self) -> str:
        return urlparse(self.public_url).netloc

    @classmethod
    def from_env(cls, store_dir: Path) -> "HttpConfig":
        public_url = os.environ.get("WHATSAPP_MCP_PUBLIC_URL", "").strip().rstrip("/")
        if not public_url:
            raise ValueError(
                "WHATSAPP_MCP_TRANSPORT=http requires WHATSAPP_MCP_PUBLIC_URL, the https:// URL "
                "claude.ai will reach this server at (without the /mcp suffix)."
            )
        parsed = urlparse(public_url)
        if parsed.scheme != "https" and not _env_bool("WHATSAPP_MCP_ALLOW_INSECURE_HTTP"):
            raise ValueError(
                "WHATSAPP_MCP_PUBLIC_URL must be https://. OAuth tokens and your messages would otherwise "
                "travel in clear text. Set WHATSAPP_MCP_ALLOW_INSECURE_HTTP=1 only for local testing."
            )
        if not parsed.netloc or parsed.query or parsed.fragment:
            raise ValueError("WHATSAPP_MCP_PUBLIC_URL must look like https://host[:port][/path]")

        redirects = [r.strip().rstrip("/") for r in os.environ.get("WHATSAPP_MCP_ALLOWED_REDIRECTS", "").split(",") if r.strip()]
        if not redirects:
            redirects = list(DEFAULT_ALLOWED_REDIRECTS)
        for r in redirects:
            rp = urlparse(r)
            if rp.scheme != "https" and not _env_bool("WHATSAPP_MCP_ALLOW_INSECURE_HTTP"):
                raise ValueError(f"WHATSAPP_MCP_ALLOWED_REDIRECTS entry {r!r} must be https://")

        pw_hash = os.environ.get("WHATSAPP_MCP_OWNER_PASSWORD_HASH", "").strip() or None
        if pw_hash and not pw_hash.startswith("pbkdf2_sha256$"):
            raise ValueError("WHATSAPP_MCP_OWNER_PASSWORD_HASH must be produced by: uv run python oauth.py hash-password")

        return cls(
            public_url=public_url,
            host=os.environ.get("WHATSAPP_MCP_HOST", "127.0.0.1").strip() or "127.0.0.1",
            port=int(os.environ.get("WHATSAPP_MCP_PORT", "8000")),
            allowed_redirects=redirects,
            owner_password_hash=pw_hash,
            access_ttl=max(60, int(os.environ.get("WHATSAPP_MCP_ACCESS_TOKEN_TTL", "3600"))),
            refresh_ttl=max(3600, int(os.environ.get("WHATSAPP_MCP_REFRESH_TOKEN_TTL", str(30 * 86400)))),
            state_path=store_dir / "mcp_oauth_state.json",
            audit_path=store_dir / "mcp_auth_audit.log",
            allow_send=not _env_bool("WHATSAPP_MCP_READ_ONLY"),
        )


def _env_bool(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in ("1", "true", "yes", "on")


def build_auth_settings(cfg: HttpConfig) -> AuthSettings:
    return AuthSettings(
        issuer_url=AnyHttpUrl(cfg.public_url),
        resource_server_url=AnyHttpUrl(cfg.resource_url),
        required_scopes=[SCOPE_READ],
        validate_token_resource=True,
        client_registration_options=ClientRegistrationOptions(
            enabled=True,
            valid_scopes=ALL_SCOPES,
            default_scopes=[SCOPE_READ],
        ),
        revocation_options=RevocationOptions(enabled=True),
    )


def build_transport_security(cfg: HttpConfig) -> TransportSecuritySettings:
    """Reject requests whose Host/Origin is not us or claude.ai (DNS rebinding)."""
    hosts = {cfg.public_host, f"{cfg.host}:{cfg.port}", f"127.0.0.1:{cfg.port}", f"localhost:{cfg.port}"}
    if ":" not in cfg.public_host:
        hosts.add(cfg.public_host + (":443" if cfg.public_url.startswith("https") else ":80"))
    # Extra Host values a reverse proxy or container network may present, e.g. "mcp:8000".
    for h in os.environ.get("WHATSAPP_MCP_EXTRA_ALLOWED_HOSTS", "").split(","):
        if h.strip():
            hosts.add(h.strip())
    origins = {cfg.public_url, "https://claude.ai", "https://claude.com"}
    for r in cfg.allowed_redirects:
        p = urlparse(r)
        origins.add(f"{p.scheme}://{p.netloc}")
    return TransportSecuritySettings(enable_dns_rebinding_protection=True, allowed_hosts=sorted(hosts), allowed_origins=sorted(origins))


def require_scope(scope: str) -> None:
    """Tool-level scope check. Without an access token (stdio mode) local trust applies."""
    token = get_access_token()
    if token is None:
        return
    if scope not in token.scopes:
        raise ToolError(
            f"This connection was authorized without the '{scope}' scope. "
            "Reconnect the WhatsApp connector and allow it on the consent page."
        )


# ---------------------------------------------------------------------------
# Provider
# ---------------------------------------------------------------------------


class LocalOAuthProvider(OAuthAuthorizationServerProvider[AuthorizationCode, RefreshToken, AccessToken]):
    def __init__(self, cfg: HttpConfig):
        self.cfg = cfg
        self.clients: Dict[str, Dict[str, Any]] = {}
        self.codes: Dict[str, Dict[str, Any]] = {}  # sha256(code) -> record
        self.access: Dict[str, Dict[str, Any]] = {}  # sha256(token) -> record
        self.refresh: Dict[str, Dict[str, Any]] = {}  # sha256(token) -> record
        self.pending: Dict[str, Dict[str, Any]] = {}  # memory only
        self._failures: List[int] = []
        self._state_sig: Any = None
        self._load()

    # --- persistence (hashes only) -----------------------------------------

    def _file_sig(self) -> Any:
        try:
            st = self.cfg.state_path.stat()
            return (st.st_mtime_ns, st.st_size, st.st_ino)
        except OSError:
            return None

    def _maybe_reload(self) -> None:
        """Pick up out-of-band changes (``oauth.py revoke-all``) without a restart."""
        if self._file_sig() != self._state_sig:
            self._load()

    def _load(self) -> None:
        self._state_sig = self._file_sig()
        try:
            data = json.loads(self.cfg.state_path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            self.clients, self.codes, self.access, self.refresh = {}, {}, {}, {}
            return
        except (OSError, ValueError) as e:
            logger.error("Could not read OAuth state %s: %s", self.cfg.state_path, e)
            return
        self.clients = data.get("clients", {})
        self.codes = data.get("codes", {})
        self.access = data.get("access", {})
        self.refresh = data.get("refresh", {})
        self._purge()

    def _save(self) -> None:
        self._purge()
        data = {"version": 1, "clients": self.clients, "codes": self.codes, "access": self.access, "refresh": self.refresh}
        tmp = self.cfg.state_path.with_suffix(".tmp")
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(data, f)
        os.replace(tmp, self.cfg.state_path)
        try:
            os.chmod(self.cfg.state_path, 0o600)
        except OSError:
            pass
        self._state_sig = self._file_sig()

    def _purge(self) -> None:
        now = _now()
        self.codes = {k: v for k, v in self.codes.items() if v.get("expires_at", 0) > now}
        self.access = {k: v for k, v in self.access.items() if v.get("expires_at", 0) > now}
        self.refresh = {k: v for k, v in self.refresh.items() if v.get("expires_at", 0) > now}
        self.pending = {k: v for k, v in self.pending.items() if v.get("expires_at", 0) > now}
        self._failures = [t for t in self._failures if t > now - LOCKOUT_WINDOW]

    def audit(self, event: str, **fields: Any) -> None:
        entry = {"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "event": event, **fields}
        try:
            fd = os.open(self.cfg.audit_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            with os.fdopen(fd, "a", encoding="utf-8") as f:
                f.write(json.dumps(entry) + "\n")
        except OSError as e:
            logger.error("Could not write auth audit log: %s", e)
        logger.info("auth %s %s", event, {k: v for k, v in fields.items() if k != "scopes"})

    # --- clients -------------------------------------------------------------

    async def get_client(self, client_id: str) -> Optional[OAuthClientInformationFull]:
        self._maybe_reload()
        raw = self.clients.get(client_id)
        if raw is None:
            return None
        return OAuthClientInformationFull.model_validate(raw)

    async def register_client(self, client_info: OAuthClientInformationFull) -> None:
        uris = [str(u).rstrip("/") for u in (client_info.redirect_uris or [])]
        if not uris:
            raise RegistrationError("invalid_redirect_uri", "At least one redirect_uri is required")
        for uri in uris:
            if uri not in self.cfg.allowed_redirects:
                self.audit("client_rejected", client_name=client_info.client_name, redirect_uri=uri)
                raise RegistrationError(
                    "invalid_redirect_uri",
                    "redirect_uri is not on this server's allow-list (WHATSAPP_MCP_ALLOWED_REDIRECTS)",
                )
        if any(gt not in ("authorization_code", "refresh_token") for gt in client_info.grant_types):
            raise RegistrationError("invalid_client_metadata", "Only authorization_code and refresh_token grants are supported")

        if len(self.clients) >= MAX_CLIENTS:
            oldest = min(self.clients.items(), key=lambda kv: kv[1].get("client_id_issued_at") or 0)[0]
            del self.clients[oldest]
        self.clients[client_info.client_id] = client_info.model_dump(mode="json", exclude_none=True)
        self._save()
        self.audit("client_registered", client_id=client_info.client_id, client_name=client_info.client_name, redirect_uris=uris)

    # --- authorization ---------------------------------------------------------

    def _lockout_active(self) -> bool:
        self._purge()
        return len(self._failures) >= LOCKOUT_FAILURES

    async def authorize(self, client: OAuthClientInformationFull, params: AuthorizationParams) -> str:
        if params.resource and str(params.resource).rstrip("/") != self.cfg.resource_url:
            raise AuthorizeError("invalid_target", "Unknown resource indicator")
        scopes = params.scopes or [SCOPE_READ]
        if any(s not in ALL_SCOPES for s in scopes):
            raise AuthorizeError("invalid_scope", f"Supported scopes: {' '.join(ALL_SCOPES)}")
        if SCOPE_READ not in scopes:
            scopes = [SCOPE_READ] + scopes
        if self._lockout_active():
            raise AuthorizeError("temporarily_unavailable", "Too many failed approval attempts; try again later")

        self._purge()
        while len(self.pending) >= MAX_PENDING:
            oldest = min(self.pending.items(), key=lambda kv: kv[1]["created_at"])[0]
            del self.pending[oldest]

        req_id = secrets.token_urlsafe(32)
        pairing = "".join(secrets.choice(PAIRING_ALPHABET) for _ in range(8))
        self.pending[req_id] = {
            "client_id": client.client_id,
            "client_name": client.client_name or client.client_id,
            "scopes": scopes,
            "state": params.state,
            "code_challenge": params.code_challenge,
            "redirect_uri": str(params.redirect_uri),
            "redirect_uri_provided_explicitly": params.redirect_uri_provided_explicitly,
            "resource": self.cfg.resource_url,
            "csrf": secrets.token_urlsafe(32),
            "pairing": pairing,
            "attempts": 0,
            "created_at": _now(),
            "expires_at": _now() + PENDING_TTL,
        }
        self.audit("authorize_requested", client_id=client.client_id, client_name=client.client_name, scopes=scopes)
        banner = (
            "\n==================== WhatsApp MCP authorization request ====================\n"
            f" Client:        {client.client_name or client.client_id}\n"
            f" Scopes:        {' '.join(scopes)}\n"
            f" Approval code: {pairing[:4]}-{pairing[4:]}   (valid for {PENDING_TTL // 60} minutes)\n"
            " Enter this code on the consent page that opened in your browser.\n"
            " If you did not start this from claude.ai, click Deny.\n"
            "============================================================================\n"
        )
        print(banner, file=sys.stderr, flush=True)
        return f"{self.cfg.public_url}/consent?req={req_id}"

    # --- codes and tokens --------------------------------------------------

    async def load_authorization_code(self, client: OAuthClientInformationFull, authorization_code: str) -> Optional[AuthorizationCode]:
        rec = self.codes.get(_hash(authorization_code))
        if rec is None or rec["client_id"] != client.client_id:
            return None
        return AuthorizationCode(
            code=authorization_code,
            scopes=rec["scopes"],
            expires_at=rec["expires_at"],
            client_id=rec["client_id"],
            code_challenge=rec["code_challenge"],
            redirect_uri=AnyUrl(rec["redirect_uri"]),
            redirect_uri_provided_explicitly=rec["redirect_uri_provided_explicitly"],
            resource=rec.get("resource"),
            subject="owner",
        )

    async def exchange_authorization_code(self, client: OAuthClientInformationFull, authorization_code: AuthorizationCode) -> OAuthToken:
        rec = self.codes.pop(_hash(authorization_code.code), None)
        if rec is None:
            raise TokenError("invalid_grant", "Authorization code already used or expired")
        token = self._issue(client.client_id, rec["scopes"], rec.get("resource") or self.cfg.resource_url)
        self.audit("token_issued", client_id=client.client_id, scopes=rec["scopes"], grant="authorization_code")
        return token

    def _issue(self, client_id: str, scopes: List[str], resource: str) -> OAuthToken:
        pair = secrets.token_hex(8)
        access = secrets.token_urlsafe(32)
        refresh = secrets.token_urlsafe(32)
        now = _now()
        self.access[_hash(access)] = {"client_id": client_id, "scopes": scopes, "expires_at": now + self.cfg.access_ttl, "resource": resource, "pair": pair}
        self.refresh[_hash(refresh)] = {"client_id": client_id, "scopes": scopes, "expires_at": now + self.cfg.refresh_ttl, "resource": resource, "pair": pair}
        self._save()
        return OAuthToken(access_token=access, token_type="Bearer", expires_in=self.cfg.access_ttl, scope=" ".join(scopes), refresh_token=refresh)

    async def load_refresh_token(self, client: OAuthClientInformationFull, refresh_token: str) -> Optional[RefreshToken]:
        self._maybe_reload()
        rec = self.refresh.get(_hash(refresh_token))
        if rec is None or rec["client_id"] != client.client_id or rec["expires_at"] <= _now():
            return None
        return RefreshToken(token=refresh_token, client_id=rec["client_id"], scopes=rec["scopes"], expires_at=rec["expires_at"], resource=rec.get("resource"), subject="owner")

    async def exchange_refresh_token(self, client: OAuthClientInformationFull, refresh_token: RefreshToken, scopes: List[str]) -> OAuthToken:
        rec = self.refresh.get(_hash(refresh_token.token))
        if rec is None:
            raise TokenError("invalid_grant", "Refresh token already used or revoked")
        granted = scopes or rec["scopes"]
        if any(s not in rec["scopes"] for s in granted):
            raise TokenError("invalid_scope", "Cannot escalate scopes on refresh")
        self._revoke_pair(rec["pair"])  # rotation: the old pair dies with this use
        token = self._issue(client.client_id, granted, rec.get("resource") or self.cfg.resource_url)
        self.audit("token_refreshed", client_id=client.client_id, scopes=granted)
        return token

    async def load_access_token(self, token: str) -> Optional[AccessToken]:
        self._maybe_reload()
        rec = self.access.get(_hash(token))
        if rec is None or rec["expires_at"] <= _now():
            return None
        return AccessToken(token=token, client_id=rec["client_id"], scopes=rec["scopes"], expires_at=rec["expires_at"], resource=rec.get("resource"), subject="owner")

    def _revoke_pair(self, pair: str) -> None:
        self.access = {k: v for k, v in self.access.items() if v.get("pair") != pair}
        self.refresh = {k: v for k, v in self.refresh.items() if v.get("pair") != pair}
        self._save()

    async def revoke_token(self, token: Any) -> None:
        raw = getattr(token, "token", None)
        if not raw:
            return
        rec = self.access.get(_hash(raw)) or self.refresh.get(_hash(raw))
        if rec is None:
            return
        self._revoke_pair(rec["pair"])
        self.audit("token_revoked", client_id=rec["client_id"])

    # --- consent pages -----------------------------------------------------

    def _page(self, body: str, status: int = 200) -> Response:
        doc = (
            "<!doctype html><html lang='en'><head><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'>"
            "<title>WhatsApp MCP authorization</title><style>"
            "body{font:16px/1.5 system-ui,sans-serif;background:#f4f5f7;color:#1b1f23;margin:0;padding:24px}"
            "main{max-width:460px;margin:40px auto;background:#fff;border-radius:12px;padding:28px;box-shadow:0 2px 12px rgba(0,0,0,.08)}"
            "h1{font-size:20px;margin:0 0 12px}code,input{font:inherit}input[type=text],input[type=password]{width:100%;box-sizing:border-box;padding:10px;border:1px solid #c9ced6;border-radius:8px;margin:6px 0 14px}"
            "button{padding:10px 18px;border-radius:8px;border:0;font:inherit;cursor:pointer}button.ok{background:#128c7e;color:#fff}button.no{background:#e5e7eb;margin-left:8px}"
            "label.chk{display:block;margin:10px 0}.muted{color:#5b6470;font-size:14px}.warn{color:#9a3412}"
            "</style></head><body><main>" + body + "</main></body></html>"
        )
        resp = HTMLResponse(doc, status_code=status)
        resp.headers["Cache-Control"] = "no-store"
        resp.headers["X-Frame-Options"] = "DENY"
        resp.headers["X-Content-Type-Options"] = "nosniff"
        resp.headers["Referrer-Policy"] = "no-referrer"
        resp.headers["Content-Security-Policy"] = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
        return resp

    def _expired_page(self) -> Response:
        return self._page("<h1>Authorization request expired</h1><p>Start the connection again from claude.ai.</p>", 400)

    async def consent_get(self, request: Request) -> Response:
        self._purge()
        req_id = request.query_params.get("req", "")
        pend = self.pending.get(req_id)
        if pend is None:
            return self._expired_page()
        if self._lockout_active():
            return self._page("<h1>Temporarily locked</h1><p>Too many failed attempts. Try again in a few minutes.</p>", 429)
        # Clients often request only the scopes advertised as required, so the
        # owner can always opt in to sending here (unless the server is read-only).
        wants_send = SCOPE_SEND in pend["scopes"]
        offer_send = self.cfg.allow_send
        hint = "the approval code shown in the bridge's terminal"
        if self.cfg.owner_password_hash:
            hint += " or your owner password"
        body = (
            f"<h1>Allow <strong>{html.escape(pend['client_name'])}</strong> to use your WhatsApp?</h1>"
            "<p class='muted'>This will let the connected assistant read your cached WhatsApp messages"
            + (" and, if you tick the box below, send messages as you" if offer_send else "")
            + ".</p>"
            f"<form method='post' action='/consent'>"
            f"<input type='hidden' name='req' value='{html.escape(req_id)}'>"
            f"<input type='hidden' name='csrf' value='{html.escape(pend['csrf'])}'>"
            f"<label>Enter {hint}<input type='password' name='code' autocomplete='one-time-code' autofocus required></label>"
            + (
                "<label class='chk'><input type='checkbox' name='allow_send' value='1'"
                + (" checked" if wants_send else "")
                + "> Allow sending messages and files</label>"
                if offer_send
                else "<p class='muted'>This server is read-only; sending is disabled.</p>"
            )
            + "<p class='muted'>Read access is always included. "
            f"Attempts left: {MAX_ATTEMPTS_PER_REQUEST - pend['attempts']}.</p>"
            "<button class='ok' type='submit' name='action' value='approve'>Approve</button>"
            "<button class='no' type='submit' name='action' value='deny' formnovalidate>Deny</button>"
            "</form>"
        )
        return self._page(body)

    async def consent_post(self, request: Request) -> Response:
        self._purge()
        form = await request.form()
        req_id = str(form.get("req", ""))
        pend = self.pending.get(req_id)
        if pend is None:
            return self._expired_page()
        if not hmac.compare_digest(str(form.get("csrf", "")), pend["csrf"]):
            del self.pending[req_id]
            return self._page("<h1>Request rejected</h1><p>Invalid form token. Start again from claude.ai.</p>", 400)

        redirect_uri = pend["redirect_uri"]
        state = pend["state"]

        if form.get("action") == "deny":
            del self.pending[req_id]
            self.audit("consent_denied", client_id=pend["client_id"])
            return RedirectResponse(construct_redirect_uri(redirect_uri, error="access_denied", state=state), status_code=302)

        if self._lockout_active():
            return self._page("<h1>Temporarily locked</h1><p>Too many failed attempts. Try again in a few minutes.</p>", 429)

        supplied = str(form.get("code", ""))
        ok = hmac.compare_digest(_normalize_code(supplied), pend["pairing"])
        if not ok and self.cfg.owner_password_hash:
            ok = verify_password(supplied, self.cfg.owner_password_hash)
        if not ok:
            pend["attempts"] += 1
            self._failures.append(_now())
            self.audit("consent_failed", client_id=pend["client_id"], attempts=pend["attempts"])
            if pend["attempts"] >= MAX_ATTEMPTS_PER_REQUEST:
                del self.pending[req_id]
                return RedirectResponse(construct_redirect_uri(redirect_uri, error="access_denied", error_description="Too many failed approval attempts", state=state), status_code=302)
            return self._page(
                f"<h1 class='warn'>Incorrect code</h1><p>Attempts left: {MAX_ATTEMPTS_PER_REQUEST - pend['attempts']}.</p>"
                f"<p><a href='/consent?req={html.escape(req_id)}'>Try again</a></p>",
                401,
            )

        granted = [SCOPE_READ]
        if self.cfg.allow_send and form.get("allow_send") == "1":
            granted.append(SCOPE_SEND)

        code = secrets.token_urlsafe(32)
        self.codes[_hash(code)] = {
            "client_id": pend["client_id"],
            "scopes": granted,
            "expires_at": _now() + CODE_TTL,
            "code_challenge": pend["code_challenge"],
            "redirect_uri": redirect_uri,
            "redirect_uri_provided_explicitly": pend["redirect_uri_provided_explicitly"],
            "resource": pend["resource"],
        }
        del self.pending[req_id]
        self._save()
        self.audit("consent_granted", client_id=pend["client_id"], scopes=granted)
        return RedirectResponse(construct_redirect_uri(redirect_uri, code=code, state=state), status_code=302)


async def _health(_: Request) -> Response:
    """Liveness probe for containers and tunnels. Deliberately reveals nothing else."""
    resp = JSONResponse({"ok": True})
    resp.headers["Cache-Control"] = "no-store"
    return resp


def register_consent_routes(mcp: Any, provider: LocalOAuthProvider) -> None:
    mcp.custom_route("/consent", methods=["GET"], include_in_schema=False)(provider.consent_get)
    mcp.custom_route("/consent", methods=["POST"], include_in_schema=False)(provider.consent_post)
    mcp.custom_route("/health", methods=["GET"], include_in_schema=False)(_health)


def _cli_state_path() -> Path:
    from whatsapp import BRIDGE_STORE_DIR  # noqa: WPS433 (CLI only)

    return BRIDGE_STORE_DIR / "mcp_oauth_state.json"


def _cli_revoke_all() -> None:
    """Remove every issued token (and optionally every client) from the state file.

    A running server notices the change on its next request, so this works
    without a restart. claude.ai will ask you to authorize again.
    """
    path = _cli_state_path()
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        print("No OAuth state found; nothing to revoke.")
        return
    n = len(data.get("access", {})) + len(data.get("refresh", {}))
    data["access"], data["refresh"], data["codes"] = {}, {}, {}
    if "--clients" in sys.argv:
        n += len(data.get("clients", {}))
        data["clients"] = {}
    fd = os.open(path, os.O_WRONLY | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump(data, f)
    print(f"Revoked {n} tokens/clients in {path}")


def _cli_status() -> None:
    path = _cli_state_path()
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        print("No OAuth state found.")
        return
    now = _now()
    print(f"State file: {path}")
    print(f"Clients: {len(data.get('clients', {}))}")
    for cid, c in data.get("clients", {}).items():
        print(f"  {cid}  {c.get('client_name', '')}  redirects={c.get('redirect_uris')}")
    live_access = [a for a in data.get("access", {}).values() if a.get("expires_at", 0) > now]
    live_refresh = [r for r in data.get("refresh", {}).values() if r.get("expires_at", 0) > now]
    print(f"Live access tokens: {len(live_access)}; live refresh tokens (sessions): {len(live_refresh)}")
    for r in live_refresh:
        print(f"  session scopes={' '.join(r.get('scopes', []))} expires={time.strftime('%Y-%m-%d %H:%M', time.gmtime(r['expires_at']))}Z")


if __name__ == "__main__":
    if len(sys.argv) >= 2 and sys.argv[1] == "revoke-all":
        _cli_revoke_all()
    elif len(sys.argv) >= 2 and sys.argv[1] == "status":
        _cli_status()
    elif len(sys.argv) >= 2 and sys.argv[1] == "hash-password":
        pw = getpass.getpass("Owner password: ")
        if len(pw) < 12:
            print("Use at least 12 characters.", file=sys.stderr)
            sys.exit(1)
        if pw != getpass.getpass("Repeat: "):
            print("Passwords differ.", file=sys.stderr)
            sys.exit(1)
        print(hash_password(pw))
    else:
        print("Usage: uv run python oauth.py hash-password | status | revoke-all [--clients]", file=sys.stderr)
        sys.exit(1)
