#!/usr/bin/env python3
"""The deploy smoke test's WebSocket check (deploy.sh), standard library only.

    ws_smoke.py check   log in, open /ws, join a fresh room, expect an answer
                        for it, close, log out
    ws_smoke.py login   log in and log out only (deploy.sh's precondition)

Configuration comes from the environment, with no defaults here: deploy.sh
sets the production ones, and a local run must name its own stack, so this
script never reaches production by accident.

    SMOKE_API       http(s) base of the API, e.g. http://localhost:8080
    SMOKE_WS        ws(s) base of the WebSocket service, e.g. ws://localhost:8084
    SMOKE_USER      the account to log in as (it is never registered here)
    SMOKE_PASSWORD  its password

Exit status: 0 passed; 3 the login was refused with 401 (no such account, or
a wrong password: the API does not tell them apart); 2 the configuration is
missing; 1 any other failure. It writes exactly one line, on stderr, and
never prints the password or the token.

The WebSocket client is written out here (RFC 6455, client side) rather than
installed: the runner has python3 but nothing may be downloaded. It sends
masked frames, reads 7-, 16- and 64-bit lengths, reassembles fragmented
messages, skips ping, pong and binary frames, verifies TLS (certificate and
host name) for wss://, and has a timeout on every read.
"""

import base64
import hashlib
import http.client
import json
import os
import socket
import ssl
import struct
import sys
import time
import urllib.parse
import uuid

HTTP_TIMEOUT = 10  # seconds, per request (connect and every read)
HANDSHAKE_TIMEOUT = 10  # seconds, connect, TLS and the 101 answer
ANSWER_TIMEOUT = 5  # seconds from sending the join to its answer
MAX_MESSAGE = 1 << 20  # larger messages from the server are a failure
WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

# A join is answered with one of these for the joined room. Both on purpose;
# see the comment in check().
JOIN_ANSWERS = ("JoinRejected", "PlayerJoined")


class Fail(Exception):
    """A failed check; the message is the one line printed."""

    def __init__(self, message, status=1):
        super().__init__(message)
        self.status = status


def config():
    names = ("SMOKE_API", "SMOKE_WS", "SMOKE_USER", "SMOKE_PASSWORD")
    missing = [n for n in names if not os.environ.get(n)]
    if missing:
        raise Fail("not set: " + ", ".join(missing), status=2)
    api = urllib.parse.urlsplit(os.environ["SMOKE_API"])
    ws = urllib.parse.urlsplit(os.environ["SMOKE_WS"])
    if api.scheme not in ("http", "https") or not api.hostname:
        raise Fail("SMOKE_API must be an http:// or https:// URL", status=2)
    if ws.scheme not in ("ws", "wss") or not ws.hostname:
        raise Fail("SMOKE_WS must be a ws:// or wss:// URL", status=2)
    return api, ws, os.environ["SMOKE_USER"], os.environ["SMOKE_PASSWORD"]


def where(u):
    return "%s://%s" % (u.scheme, u.netloc)


# --- HTTP: login and logout -------------------------------------------------


def request(api, method, path, body=None, token=None):
    """One request, no proxy and no redirect. Returns (status, body bytes)."""
    if api.scheme == "https":
        conn = http.client.HTTPSConnection(
            api.hostname, api.port, timeout=HTTP_TIMEOUT, context=ssl.create_default_context()
        )
    else:
        conn = http.client.HTTPConnection(api.hostname, api.port, timeout=HTTP_TIMEOUT)
    headers = {"User-Agent": "gamemesh-deploy-smoke", "Accept": "application/json"}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    try:
        conn.request(method, api.path.rstrip("/") + path, body=data, headers=headers)
        resp = conn.getresponse()
        return resp.status, resp.read(64 * 1024)
    except (OSError, http.client.HTTPException) as e:
        raise Fail("%s %s%s: %s" % (method, where(api), path, describe(e)))
    finally:
        conn.close()


def login(api, user, password):
    status, body = request(
        api, "POST", "/api/v1/auth/login", {"identifier": user, "password": password}
    )
    if status == 401:
        raise Fail(
            "login as %s refused (401): no such account or a wrong password" % user, status=3
        )
    if status != 200:
        raise Fail("login as %s: HTTP %d from %s" % (user, status, where(api)))
    try:
        token = json.loads(body)["token"]
    except (ValueError, KeyError, TypeError):
        token = None
    if not isinstance(token, str) or not token:
        raise Fail("login as %s: HTTP 200 without a token" % user)
    return token


def logout(api, token):
    """Best effort: the session expires with the token anyway. Returns a note."""
    try:
        status, _ = request(api, "POST", "/api/v1/auth/logout", token=token)
    except Fail as e:
        return "; logout failed (%s)" % e
    return "" if status == 200 else "; logout answered HTTP %d" % status


# --- WebSocket ----------------------------------------------------------------


def describe(e):
    if isinstance(e, socket.timeout):
        return "timed out"
    if isinstance(e, ssl.SSLCertVerificationError):
        return "TLS certificate not accepted: %s" % e.verify_message
    return str(e) or type(e).__name__


class Deadline:
    """A point in time, and how long it was set for (for the message)."""

    def __init__(self, seconds):
        self.seconds = seconds
        self.end = time.monotonic() + seconds

    def left(self):
        return self.end - time.monotonic()


class WebSocket:
    def __init__(self, ws, path_and_query):
        port = ws.port or (443 if ws.scheme == "wss" else 80)
        self.buf = b""
        self.where = "%s://%s" % (ws.scheme, ws.netloc)
        try:
            sock = socket.create_connection((ws.hostname, port), timeout=HANDSHAKE_TIMEOUT)
            if ws.scheme == "wss":
                sock = ssl.create_default_context().wrap_socket(sock, server_hostname=ws.hostname)
        except OSError as e:
            raise Fail("connect to %s: %s" % (self.where, describe(e)))
        self.sock = sock
        self.handshake(ws, path_and_query)

    def handshake(self, ws, path_and_query):
        key = base64.b64encode(os.urandom(16)).decode()
        req = (
            "GET %s HTTP/1.1\r\n"
            "Host: %s\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            "Sec-WebSocket-Key: %s\r\n"
            "Sec-WebSocket-Version: 13\r\n"
            "User-Agent: gamemesh-deploy-smoke\r\n"
            "\r\n" % (path_and_query, ws.netloc, key)
        )
        deadline = Deadline(HANDSHAKE_TIMEOUT)
        try:
            self.sock.sendall(req.encode())
            while b"\r\n\r\n" not in self.buf:
                if len(self.buf) > 16 * 1024:
                    raise Fail("upgrade at %s: response headers too long" % self.where)
                self.recv_some(deadline, "upgrade answer")
        except OSError as e:
            raise Fail("upgrade at %s: %s" % (self.where, describe(e)))
        head, self.buf = self.buf.split(b"\r\n\r\n", 1)
        lines = head.decode("latin-1").split("\r\n")
        parts = lines[0].split(" ", 2)
        if len(parts) < 2 or parts[1] != "101":
            # The body (an error envelope) is not needed: the status says it.
            raise Fail("upgrade at %s: %s instead of 101" % (self.where, lines[0].strip()))
        headers = {}
        for line in lines[1:]:
            name, _, value = line.partition(":")
            headers[name.strip().lower()] = value.strip()
        want = base64.b64encode(hashlib.sha1((key + WS_GUID).encode()).digest()).decode()
        if headers.get("sec-websocket-accept") != want:
            raise Fail("upgrade at %s: wrong Sec-WebSocket-Accept" % self.where)

    def recv_some(self, deadline, what):
        late = Fail("no %s from %s within %g s" % (what, self.where, deadline.seconds))
        left = deadline.left()
        if left <= 0:
            raise late
        self.sock.settimeout(left)
        try:
            chunk = self.sock.recv(65536)
        except socket.timeout:
            raise late
        except OSError as e:
            raise Fail("waiting for the %s from %s: %s" % (what, self.where, describe(e)))
        if not chunk:
            raise Fail("%s closed the connection while waiting for the %s" % (self.where, what))
        self.buf += chunk

    def read_exact(self, n, deadline, what):
        while len(self.buf) < n:
            self.recv_some(deadline, what)
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def read_frame(self, deadline, what):
        b0, b1 = self.read_exact(2, deadline, what)
        fin, opcode = b0 & 0x80, b0 & 0x0F
        masked, n = b1 & 0x80, b1 & 0x7F
        if n == 126:
            (n,) = struct.unpack("!H", self.read_exact(2, deadline, what))
        elif n == 127:
            (n,) = struct.unpack("!Q", self.read_exact(8, deadline, what))
        if n > MAX_MESSAGE:
            raise Fail("%s sent a frame of %d bytes" % (self.where, n))
        mask = self.read_exact(4, deadline, what) if masked else None
        payload = self.read_exact(n, deadline, what)
        if mask:  # servers do not mask, but a masked frame is still readable
            payload = bytes(c ^ mask[i % 4] for i, c in enumerate(payload))
        return bool(fin), opcode, payload

    def read_text(self, deadline, what):
        """The next complete text message; other frame types are skipped."""
        parts, kind = [], None
        while True:
            fin, opcode, payload = self.read_frame(deadline, what)
            if opcode == 0x8:
                code = struct.unpack("!H", payload[:2])[0] if len(payload) >= 2 else None
                raise Fail(
                    "%s closed the WebSocket (code %s) while waiting for the %s"
                    % (self.where, code, what)
                )
            if opcode in (0x9, 0xA):  # ping, pong: nothing to answer within seconds
                continue
            if opcode in (0x1, 0x2):  # text, binary: a new message
                kind, parts = opcode, [payload]
            elif opcode == 0x0 and kind is not None:  # continuation
                parts.append(payload)
            else:
                continue
            if sum(len(p) for p in parts) > MAX_MESSAGE:
                raise Fail("%s sent a message over %d bytes" % (self.where, MAX_MESSAGE))
            if fin:
                if kind == 0x1:
                    return b"".join(parts).decode("utf-8", "replace")
                parts, kind = [], None  # a binary message: not ours

    def send(self, opcode, payload):
        n = len(payload)
        if n < 126:
            head = struct.pack("!BB", 0x80 | opcode, 0x80 | n)
        elif n < 1 << 16:
            head = struct.pack("!BBH", 0x80 | opcode, 0x80 | 126, n)
        else:
            head = struct.pack("!BBQ", 0x80 | opcode, 0x80 | 127, n)
        mask = os.urandom(4)
        body = bytes(c ^ mask[i % 4] for i, c in enumerate(payload))
        self.sock.settimeout(HTTP_TIMEOUT)
        try:
            self.sock.sendall(head + mask + body)
        except OSError as e:
            raise Fail("send to %s: %s" % (self.where, describe(e)))

    def close(self):
        """A close frame (1000), then up to a second for the server's own."""
        try:
            self.send(0x8, struct.pack("!H", 1000))
            deadline = Deadline(1)
            while True:
                _, opcode, _ = self.read_frame(deadline, "close answer")
                if opcode == 0x8:
                    break
        except Fail:
            pass  # the check has passed; how the server closes is not part of it
        finally:
            self.sock.close()


def check(api, ws, user, password):
    token = login(api, user, password)
    try:
        room = str(uuid.uuid4())
        path = ws.path.rstrip("/") + "/ws?" + urllib.parse.urlencode({"token": token})
        start = time.monotonic()
        conn = WebSocket(ws, path)
        try:
            conn.send(0x1, json.dumps({"action": "join", "room": room}).encode())
            deadline = Deadline(ANSWER_TIMEOUT)
            what = "answer to the join"
            while True:
                text = conn.read_text(deadline, what)
                try:
                    msg = json.loads(text)
                except ValueError:
                    continue
                if not isinstance(msg, dict) or msg.get("room") != room:
                    continue  # a broadcast for everyone, or another room
                # Either answer passes, and this must stay so. Releases with
                # room membership checks (from the PR that added this check
                # on) answer a stranger's join with JoinRejected; releases
                # before it accept any join and broadcast PlayerJoined. A
                # rollback runs this same check against the previous commit,
                # so requiring JoinRejected would report a working rollback
                # to an older release as failed. Either frame proves what the
                # check is for: the upgrade, the token and session check, the
                # read loop and the write loop. Not that the room lookup
                # works: a lookup that fails or takes over 2 s is answered
                # with the same JoinRejected, on purpose without a reason.
                if msg.get("type") in JOIN_ANSWERS:
                    answer = msg["type"]
                    break
        finally:
            conn.close()
        took = time.monotonic() - start
    finally:
        note = logout(api, token)
    return "ws ok: %s joined a new room at %s, answered %s in %.2f s%s" % (
        user,
        conn.where,
        answer,
        took,
        note,
    )


def login_only(api, user, password):
    token = login(api, user, password)
    note = logout(api, token)
    return "login ok: %s at %s%s" % (user, where(api), note)


def main(argv):
    if len(argv) != 2 or argv[1] not in ("check", "login"):
        print("usage: ws_smoke.py check|login (configured by SMOKE_* variables)", file=sys.stderr)
        return 2
    try:
        api, ws, user, password = config()
        line = check(api, ws, user, password) if argv[1] == "check" else login_only(api, user, password)
    except Fail as e:
        print("ws smoke: %s" % e, file=sys.stderr)
        return e.status
    print(line, file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
