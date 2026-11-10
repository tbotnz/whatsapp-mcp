"""End-to-end check of the MCP server in HTTP mode with the built-in OAuth server.

Starts a stub bridge, then main.py with WHATSAPP_MCP_TRANSPORT=http on a free
port, and drives the whole OAuth 2.1 flow the way claude.ai does: discovery,
dynamic client registration, PKCE authorize, owner consent (pairing code read
from the server's stderr), token exchange, tool calls over streamable HTTP,
scope enforcement, refresh rotation and revocation. No WhatsApp account or
network access is needed.

Usage (from whatsapp-mcp-server/):
    uv run python tests/oauth_e2e_test.py .
"""
import asyncio
import base64
import hashlib
import json
import os
import re
import secrets
import socket
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

import httpx
import httpx2

from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

SERVER_DIR = Path(sys.argv[1] if len(sys.argv) > 1 else Path(__file__).resolve().parent.parent).resolve()
TOKEN = "t" * 64
failures = []


def check(cond, msg):
    print(("PASS " if cond else "FAIL ") + msg)
    if not cond:
        failures.append(msg)


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


# --- stub bridge -------------------------------------------------------------

ALICE = "111@s.whatsapp.net"
CHATS = [{"jid": ALICE, "name": "Alice", "last_message_time": "2026-10-20T10:00:02Z", "last_message": "ok",
          "last_sender": "999", "last_is_from_me": True, "is_group": False}]


class StubBridge(BaseHTTPRequestHandler):
    sends = []

    def log_message(self, *a):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        if self.headers.get("Authorization") != f"Bearer {TOKEN}":
            self.send_response(401)
            self.end_headers()
            return
        if self.path == "/api/chats":
            out = {"chats": CHATS}
        elif self.path == "/api/send":
            StubBridge.sends.append(body)
            out = {"success": True, "message": "Message sent to " + body.get("recipient", "")}
        else:
            out = {"error": "not stubbed: " + self.path}
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(out).encode())


# --- helpers ------------------------------------------------------------------


class StderrWatcher(threading.Thread):
    """Collects the server's stderr and extracts approval codes as they are printed."""

    def __init__(self, stream):
        super().__init__(daemon=True)
        self.stream = stream
        self.lines = []
        self.codes = []
        self.cond = threading.Condition()

    def run(self):
        for raw in self.stream:
            line = raw.decode(errors="replace").rstrip()
            with self.cond:
                self.lines.append(line)
                m = re.search(r"Approval code:\s+([A-Z2-9]{4})-([A-Z2-9]{4})", line)
                if m:
                    self.codes.append(m.group(1) + m.group(2))
                self.cond.notify_all()

    def next_code(self, seen, timeout=10):
        deadline = time.time() + timeout
        with self.cond:
            while len(self.codes) <= seen:
                remaining = deadline - time.time()
                if remaining <= 0:
                    raise TimeoutError("no approval code printed; stderr:\n" + "\n".join(self.lines[-30:]))
                self.cond.wait(remaining)
            return self.codes[seen]


def pkce():
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    return verifier, challenge


def wait_for(url, timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            r = httpx.get(url, timeout=2)
            if r.status_code < 500:
                return
        except httpx.HTTPError:
            pass
        time.sleep(0.2)
    raise TimeoutError("server did not come up at " + url)


async def mcp_call(url, access_token, tool, args):
    headers = {"Authorization": f"Bearer {access_token}"} if access_token else {}
    async with httpx2.AsyncClient(headers=headers, timeout=30) as hc:
        async with streamable_http_client(url, http_client=hc) as (read, write, *_):
            async with ClientSession(read, write) as session:
                await session.initialize()
                return await session.call_tool(tool, args)


def run_mcp(url, access_token, tool, args):
    return asyncio.run(mcp_call(url, access_token, tool, args))


# --- main --------------------------------------------------------------------


def main():
    tmp = Path(tempfile.mkdtemp(prefix="wa-oauth-e2e-"))
    store = tmp / "store"
    store.mkdir(mode=0o700)
    (store / "api_token").write_text(TOKEN)

    bridge = HTTPServer(("127.0.0.1", 0), StubBridge)
    threading.Thread(target=bridge.serve_forever, daemon=True).start()
    bridge_port = bridge.server_address[1]

    port = free_port()
    public = f"http://127.0.0.1:{port}"
    mcp_url = public + "/mcp"
    redirect = "http://127.0.0.1:9/callback"

    env = {
        **os.environ,
        "WHATSAPP_BRIDGE_STORE": str(store),
        "WHATSAPP_API_BASE_URL": f"http://127.0.0.1:{bridge_port}/api",
        "WHATSAPP_MCP_TRANSPORT": "http",
        "WHATSAPP_MCP_PUBLIC_URL": public,
        "WHATSAPP_MCP_ALLOW_INSECURE_HTTP": "1",
        "WHATSAPP_MCP_HOST": "127.0.0.1",
        "WHATSAPP_MCP_PORT": str(port),
        "WHATSAPP_MCP_ALLOWED_REDIRECTS": redirect,
        "WHATSAPP_MCP_ACCESS_TOKEN_TTL": "60",
    }
    env.pop("WHATSAPP_MCP_READ_ONLY", None)
    env.pop("WHATSAPP_MCP_OWNER_PASSWORD_HASH", None)

    # Misconfiguration must refuse to start rather than serve insecurely.
    bad = subprocess.run([sys.executable, "main.py"], cwd=SERVER_DIR, capture_output=True, text=True, timeout=60,
                         env={**env, "WHATSAPP_MCP_PUBLIC_URL": "http://example.com", "WHATSAPP_MCP_ALLOW_INSECURE_HTTP": ""})
    check(bad.returncode != 0 and "https" in bad.stderr, "refuses a plain-http public URL unless explicitly allowed")
    bad = subprocess.run([sys.executable, "main.py"], cwd=SERVER_DIR, capture_output=True, text=True, timeout=60,
                         env={k: v for k, v in env.items() if k != "WHATSAPP_MCP_PUBLIC_URL"})
    check(bad.returncode != 0 and "WHATSAPP_MCP_PUBLIC_URL" in bad.stderr, "refuses http mode without a public URL")

    proc = subprocess.Popen([sys.executable, "main.py"], cwd=SERVER_DIR, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    watcher = StderrWatcher(proc.stderr)
    watcher.start()
    try:
        wait_for(public + "/.well-known/oauth-authorization-server")
        c = httpx.Client(base_url=public, timeout=10, follow_redirects=False)

        # Health and discovery
        r = c.get("/health")
        check(r.status_code == 200 and r.json().get("ok") is True, "GET /health answers without auth")
        r = c.get("/.well-known/oauth-protected-resource/mcp")
        prm = r.json()
        check(r.status_code == 200 and prm.get("resource") == mcp_url and public in "".join(prm.get("authorization_servers", [])),
              "protected resource metadata points at this server")
        meta = c.get("/.well-known/oauth-authorization-server").json()
        check("S256" in meta.get("code_challenge_methods_supported", []) and meta.get("registration_endpoint"), "AS metadata advertises PKCE S256 and DCR")
        check(set(meta.get("scopes_supported", [])) == {"whatsapp:read", "whatsapp:send"}, "AS metadata lists the two scopes")

        # Unauthenticated MCP access
        r = c.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}},
                   headers={"Accept": "application/json, text/event-stream", "Content-Type": "application/json"})
        check(r.status_code == 401 and "resource_metadata" in r.headers.get("www-authenticate", ""), "POST /mcp without a token is 401 with WWW-Authenticate")
        r = c.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}},
                   headers={"Authorization": "Bearer nope", "Accept": "application/json, text/event-stream", "Content-Type": "application/json"})
        check(r.status_code == 401, "POST /mcp with a bogus token is 401")

        # Dynamic client registration
        r = c.post("/register", json={"redirect_uris": ["https://attacker.example/cb"], "client_name": "Evil", "token_endpoint_auth_method": "none"})
        check(r.status_code == 400 and r.json().get("error") == "invalid_redirect_uri", "registration with a non-allow-listed redirect is rejected")
        r = c.post("/register", json={"redirect_uris": [redirect], "client_name": "Claude", "token_endpoint_auth_method": "none",
                                       "grant_types": ["authorization_code", "refresh_token"], "scope": "whatsapp:read whatsapp:send"})
        check(r.status_code in (200, 201), f"registration with the allow-listed redirect succeeds ({r.status_code})")
        client = r.json()
        client_id = client["client_id"]

        def authorize(scope="whatsapp:read whatsapp:send"):
            verifier, challenge = pkce()
            state = secrets.token_urlsafe(8)
            r = c.get("/authorize", params={"client_id": client_id, "redirect_uri": redirect, "response_type": "code",
                                            "code_challenge": challenge, "code_challenge_method": "S256", "state": state,
                                            "scope": scope, "resource": mcp_url})
            return r, verifier, state

        # Plain PKCE: no challenge must fail
        r = c.get("/authorize", params={"client_id": client_id, "redirect_uri": redirect, "response_type": "code", "state": "x"})
        check(r.status_code in (302, 400), "authorize without PKCE does not open a consent page")
        loc = r.headers.get("location", "")
        check("consent" not in loc, "authorize without PKCE is refused")

        r, verifier, state = authorize()
        check(r.status_code == 302 and "/consent?req=" in r.headers.get("location", ""), "authorize redirects to the consent page")
        consent_url = r.headers["location"]
        req_id = parse_qs(urlparse(consent_url).query)["req"][0]
        code1 = watcher.next_code(0)
        check(len(code1) == 8, "an approval code was printed on the server console")

        page = c.get(f"/consent?req={req_id}")
        check(page.status_code == 200 and "Claude" in page.text and "allow_send" in page.text, "consent page names the client and offers the send scope")
        check(page.headers.get("content-security-policy", "").startswith("default-src 'none'") and page.headers.get("x-frame-options") == "DENY",
              "consent page has CSP and anti-framing headers")
        csrf = re.search(r"name='csrf' value='([^']+)'", page.text).group(1)

        r = c.post("/consent", data={"req": req_id, "csrf": "wrong", "code": code1, "action": "approve"})
        check(r.status_code == 400, "consent with a bad CSRF token is rejected")

        # The CSRF failure invalidates the request; start again.
        r, verifier, state = authorize()
        req_id = parse_qs(urlparse(r.headers["location"]).query)["req"][0]
        code2 = watcher.next_code(1)
        page = c.get(f"/consent?req={req_id}")
        csrf = re.search(r"name='csrf' value='([^']+)'", page.text).group(1)

        r = c.post("/consent", data={"req": req_id, "csrf": csrf, "code": "AAAA-AAAA", "action": "approve"})
        check(r.status_code == 401 and "Incorrect" in r.text, "wrong approval code is refused")
        r = c.post("/consent", data={"req": req_id, "csrf": csrf, "code": code1, "action": "approve"})
        check(r.status_code == 401, "an approval code from a different request is refused")

        # Approve read-only (uncheck send)
        r = c.post("/consent", data={"req": req_id, "csrf": csrf, "code": code2.lower()[:4] + "-" + code2[4:], "action": "approve"})
        loc = r.headers.get("location", "")
        q = parse_qs(urlparse(loc).query)
        check(r.status_code == 302 and loc.startswith(redirect) and q.get("state") == [state] and "code" in q, "correct code redirects back with an authorization code")
        auth_code = q["code"][0]

        r = c.post("/consent", data={"req": req_id, "csrf": csrf, "code": code2, "action": "approve"})
        check(r.status_code == 400, "a consent request cannot be replayed")

        # Token exchange
        r = c.post("/token", data={"grant_type": "authorization_code", "code": auth_code, "redirect_uri": redirect,
                                   "client_id": client_id, "code_verifier": "wrong-verifier"})
        check(r.status_code == 400, "token exchange with the wrong PKCE verifier fails")
        r = c.post("/token", data={"grant_type": "authorization_code", "code": auth_code, "redirect_uri": redirect,
                                   "client_id": client_id, "code_verifier": verifier})
        check(r.status_code == 200, f"token exchange with the right verifier succeeds ({r.status_code}: {r.text[:120]})")
        tok = r.json()
        check(tok.get("scope") == "whatsapp:read" and tok.get("refresh_token") and tok.get("expires_in") == 60, "read-only token issued with the configured TTL")
        r = c.post("/token", data={"grant_type": "authorization_code", "code": auth_code, "redirect_uri": redirect,
                                   "client_id": client_id, "code_verifier": verifier})
        check(r.status_code == 400, "an authorization code cannot be exchanged twice")

        # Tool calls
        res = run_mcp(mcp_url, tok["access_token"], "list_chats", {"limit": 5})
        text = "".join(getattr(cb, "text", "") for cb in res.content)
        check(not res.is_error and "Alice" in text, "list_chats works over HTTP with a read token")
        res = run_mcp(mcp_url, tok["access_token"], "send_message", {"recipient": "111", "message": "hi"})
        text = "".join(getattr(cb, "text", "") for cb in res.content)
        check(res.is_error and "whatsapp:send" in text, "send_message is refused without the send scope")
        check(StubBridge.sends == [], "nothing reached the bridge without the send scope")

        # DNS rebinding protection (checked after auth, so use a real token)
        r = c.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}},
                   headers={"Host": "evil.example", "Authorization": f"Bearer {tok['access_token']}", "Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
        check(r.status_code in (400, 421), f"foreign Host header is rejected even with a valid token ({r.status_code})")

        # Refresh rotation
        r = c.post("/token", data={"grant_type": "refresh_token", "refresh_token": tok["refresh_token"], "client_id": client_id})
        check(r.status_code == 200, "refresh token exchange succeeds")
        tok2 = r.json()
        r = c.post("/token", data={"grant_type": "refresh_token", "refresh_token": tok["refresh_token"], "client_id": client_id})
        check(r.status_code == 400, "a used refresh token is rejected (rotation)")
        r = c.post("/token", data={"grant_type": "refresh_token", "refresh_token": tok2["refresh_token"], "client_id": client_id, "scope": "whatsapp:read whatsapp:send"})
        check(r.status_code == 400, "refresh cannot escalate scopes")
        try:
            run_mcp(mcp_url, tok["access_token"], "list_chats", {"limit": 1})
            check(False, "old access token is dead after refresh")
        except Exception:
            check(True, "old access token is dead after refresh")
        res = run_mcp(mcp_url, tok2["access_token"], "list_chats", {"limit": 1})
        check(not res.is_error, "new access token works")

        # Revocation
        # mcp 2.3 requires the client_secret field to be present even for public clients.
        r = c.post("/revoke", data={"token": tok2["access_token"], "client_id": client_id, "client_secret": ""})
        check(r.status_code == 200, f"revocation endpoint accepts the token ({r.status_code}: {r.text[:200]})")
        try:
            run_mcp(mcp_url, tok2["access_token"], "list_chats", {"limit": 1})
            check(False, "revoked access token is rejected")
        except Exception:
            check(True, "revoked access token is rejected")
        r = c.post("/token", data={"grant_type": "refresh_token", "refresh_token": tok2["refresh_token"], "client_id": client_id})
        check(r.status_code == 400, "revocation also kills the paired refresh token")

        # Full approval including send
        r, verifier, state = authorize()
        req_id = parse_qs(urlparse(r.headers["location"]).query)["req"][0]
        code3 = watcher.next_code(2)
        csrf = re.search(r"name='csrf' value='([^']+)'", c.get(f"/consent?req={req_id}").text).group(1)
        r = c.post("/consent", data={"req": req_id, "csrf": csrf, "code": code3, "allow_send": "1", "action": "approve"})
        auth_code = parse_qs(urlparse(r.headers["location"]).query)["code"][0]
        tok3 = c.post("/token", data={"grant_type": "authorization_code", "code": auth_code, "redirect_uri": redirect,
                                      "client_id": client_id, "code_verifier": verifier}).json()
        check(tok3.get("scope") == "whatsapp:read whatsapp:send", "owner can grant the send scope")
        res = run_mcp(mcp_url, tok3["access_token"], "send_message", {"recipient": "111", "message": "hi"})
        check(not res.is_error and len(StubBridge.sends) == 1, "send_message works with the send scope")

        # Deny path
        r, verifier, state = authorize()
        req_id = parse_qs(urlparse(r.headers["location"]).query)["req"][0]
        watcher.next_code(3)
        csrf = re.search(r"name='csrf' value='([^']+)'", c.get(f"/consent?req={req_id}").text).group(1)
        r = c.post("/consent", data={"req": req_id, "csrf": csrf, "action": "deny"})
        q = parse_qs(urlparse(r.headers.get("location", "")).query)
        check(r.status_code == 302 and q.get("error") == ["access_denied"] and q.get("state") == [state], "deny redirects back with access_denied")

        # Brute force: 5 wrong codes kills the request
        r, verifier, state = authorize()
        req_id = parse_qs(urlparse(r.headers["location"]).query)["req"][0]
        watcher.next_code(4)
        csrf = re.search(r"name='csrf' value='([^']+)'", c.get(f"/consent?req={req_id}").text).group(1)
        last = None
        for _ in range(5):
            last = c.post("/consent", data={"req": req_id, "csrf": csrf, "code": "ZZZZZZZZ", "action": "approve"})
        check(last is not None and last.status_code == 302 and "access_denied" in last.headers.get("location", ""), "five wrong codes abort the request")
        check(c.get(f"/consent?req={req_id}").status_code == 400, "aborted request is gone")

        # Persistence hygiene
        state_file = store / "mcp_oauth_state.json"
        raw = state_file.read_text()
        check(oct(state_file.stat().st_mode & 0o777) == "0o600", "OAuth state file is 0600")
        check(tok3["access_token"] not in raw and tok3["refresh_token"] not in raw and auth_code not in raw, "state file holds only hashes, never raw tokens")
        audit = (store / "mcp_auth_audit.log").read_text().splitlines()
        events = {json.loads(line)["event"] for line in audit}
        check({"client_registered", "client_rejected", "authorize_requested", "consent_granted", "consent_denied", "consent_failed",
               "token_issued", "token_refreshed", "token_revoked"} <= events, "auth audit log records the whole lifecycle")
        check(all(tok3["access_token"] not in line for line in audit), "audit log never contains tokens")

        # Restart: persisted tokens survive, pending requests do not
        proc.terminate()
        proc.wait(timeout=20)
        proc2 = subprocess.Popen([sys.executable, "main.py"], cwd=SERVER_DIR, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        threading.Thread(target=proc2.stderr.read, daemon=True).start()
        try:
            wait_for(public + "/health")
            res = run_mcp(mcp_url, tok3["access_token"], "list_chats", {"limit": 1})
            check(not res.is_error, "access token survives a server restart")
            check(c.get(f"/consent?req={req_id}").status_code == 400, "pending consent requests do not survive a restart")
            cli = subprocess.run([sys.executable, "oauth.py", "revoke-all"], cwd=SERVER_DIR, env=env, capture_output=True, text=True, timeout=60)
            check(cli.returncode == 0 and "Revoked" in cli.stdout, "oauth.py revoke-all runs")
            try:
                run_mcp(mcp_url, tok3["access_token"], "list_chats", {"limit": 1})
                check(False, "revoke-all takes effect on the running server")
            except Exception:
                check(True, "revoke-all takes effect on the running server")
            cli = subprocess.run([sys.executable, "oauth.py", "status"], cwd=SERVER_DIR, env=env, capture_output=True, text=True, timeout=60)
            check(cli.returncode == 0 and "Live access tokens: 0" in cli.stdout, "oauth.py status reports no live tokens")
        finally:
            proc2.terminate()
            proc2.wait(timeout=20)
    finally:
        if proc.poll() is None:
            proc.terminate()
            proc.wait(timeout=20)
        bridge.shutdown()

    print()
    if failures:
        print(f"{len(failures)} FAILED")
        for line in watcher.lines[-40:]:
            print("  server:", line)
        sys.exit(1)
    print("ALL OAUTH E2E CHECKS PASSED")


if __name__ == "__main__":
    main()
