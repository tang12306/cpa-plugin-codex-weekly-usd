"""Local stand-ins for the things a probe talks to.

The probe path is no longer a host callback the harness can fake, so the tests
exercise it for real: the plugin dials a socket, speaks SOCKS5 when the
credential asks for it, sends the request and parses the quota out of the
answer. Faking that at the callback boundary would have tested none of it - and
the last defect in this area was precisely that probes left by a route nobody
had checked.

Two endpoints answer from one fixture per token. POST .../codex/responses is
the model probe, with the quota in its response headers. GET .../wham/usage is
the usage endpoint, with the same quota as a JSON document - built from the
fixture's headers, so a test states a credential's quota once and either read
sees it. A fixture with "NoUsage" set gets a 404 from the usage endpoint, which
is how a test says it has gone away.

A fixture with "Unstarted" set describes a window that has been reset and not
started, the way upstream does: the whole period still to run, however long
ago the reset was, until a model request arrives - after which the window runs
from that moment. "KickIgnored" keeps it unstarted regardless, which is how a
test says a kick-start did not take.
"""
import http.server
import json
import os
import socket
import threading
import time


def usage_document(headers, unstarted=False, started_at=None):
    """The usage endpoint's answer for a credential whose quota headers are these.

    unstarted describes a reset window still waiting for its first request;
    started_at is when one arrived, if it has.
    """
    h = {k.lower(): v[0] for k, v in (headers or {}).items() if v}
    now = int(time.time())
    slots = {}
    for slot in ("primary", "secondary"):
        pct = h.get("x-codex-%s-used-percent" % slot)
        if pct is None:
            slots[slot] = None
            continue
        full = int(h["x-codex-%s-window-minutes" % slot]) * 60
        reset_at = int(h["x-codex-%s-reset-at" % slot])
        if unstarted:
            reset_at = (started_at + full) if started_at else now + full
        slots[slot] = {
            "used_percent": float(pct),
            "limit_window_seconds": full,
            "reset_after_seconds": max(0, reset_at - now),
            "reset_at": reset_at,
        }
    reached = any(w and w["used_percent"] >= 100 for w in slots.values())
    return {
        "plan_type": h.get("x-codex-plan-type", "team"),
        "rate_limit": {
            "allowed": not reached,
            "limit_reached": reached,
            "primary_window": slots["primary"],
            "secondary_window": slots["secondary"],
        },
        "credits": {"has_credits": False, "unlimited": False, "balance": None},
        # The shape upstream uses: an object, or null when no limit is hit.
        "rate_limit_reached_type": {"type": "rate_limit_reached", "details": "default"} if reached else None,
        "rate_limit_reset_credits": {"available_count": 0, "applicable_available_count": 0},
    }


class _Handler(http.server.BaseHTTPRequestHandler):
    def _hit(self, method, said=""):
        token = (self.headers.get("authorization") or "").replace("Bearer ", "").strip()
        self.server.hits.append({
            "token": token,
            "method": method,
            "path": self.path,
            "said": said,
            "via_socks": self.server.socks_hits[0] > 0,
        })
        return token, self.server.specs.get(token)

    def do_POST(self):
        length = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            said = json.loads(raw)["input"][0]["content"][0]["text"]
        except Exception:
            said = ""
        token, spec = self._hit("POST", said)
        if spec is None:
            self.send_response(404)
            self.end_headers()
            return
        # Any accepted request starts a waiting window - a probe as much as a
        # greeting - unless the test says this one does not take.
        if spec.get("Unstarted") and not spec.get("KickIgnored") and spec.get("StatusCode", 200) < 300:
            self.server.started.setdefault(token, int(time.time()))
        self.send_response(spec.get("StatusCode", 200))
        for key, values in (spec.get("Headers") or {}).items():
            for value in values:
                self.send_header(key, value)
        self.send_header("content-length", "0")
        self.end_headers()

    def do_GET(self):
        token, spec = self._hit("GET")
        if spec is None or spec.get("NoUsage") or not self.path.endswith("/wham/usage"):
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()
            return
        status = spec.get("StatusCode", 200)
        # A 429 on a model request is a credential with nothing left, and the
        # usage endpoint reports that as an ordinary answer. A refusal or a
        # server failure is the same on both.
        if status in (401, 403) or status >= 500:
            body = json.dumps({"detail": "fixture status %d" % status}).encode()
        else:
            status = 200
            body = json.dumps(usage_document(spec.get("Headers"), spec.get("Unstarted"),
                                             self.server.started.get(token))).encode()
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def start_probe_server(probe_dir):
    """Serve the quota fixtures in probe_dir over real HTTP.

    Returns (url, hits, socks_hits, shutdown). hits records one entry per probe
    received, which is what the "how much does this design probe?" assertions
    are actually measuring.
    """
    specs = {}
    for name in os.listdir(probe_dir):
        if name.endswith(".json"):
            with open(os.path.join(probe_dir, name)) as fh:
                specs[name[:-5]] = json.load(fh)

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    server.specs = specs
    server.started = {}
    server.hits = []
    server.socks_hits = [0]
    threading.Thread(target=server.serve_forever, daemon=True).start()
    url = "http://127.0.0.1:%d/backend-api/codex/responses" % server.server_address[1]
    return url, server.hits, server.socks_hits, server.shutdown


def start_socks5(counter, require_auth=None):
    """A minimal SOCKS5 proxy that relays to whatever the client asks for.

    It exists to prove the plugin's hand-rolled dialer speaks the protocol
    correctly, including the username/password exchange, and that a credential
    with a proxy_url actually leaves through it.
    """
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("127.0.0.1", 0))
    listener.listen(8)
    port = listener.getsockname()[1]
    stop = threading.Event()

    def pipe(a, b):
        try:
            while True:
                data = a.recv(65536)
                if not data:
                    break
                b.sendall(data)
        except OSError:
            pass
        finally:
            for s in (a, b):
                try:
                    s.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

    def handle(conn):
        try:
            head = conn.recv(2)
            if len(head) < 2 or head[0] != 5:
                return
            methods = conn.recv(head[1])
            if require_auth:
                if 2 not in methods:
                    conn.sendall(bytes([5, 0xFF]))
                    return
                conn.sendall(bytes([5, 2]))
                conn.recv(1)                              # sub-negotiation version
                ulen = conn.recv(1)[0]
                user = conn.recv(ulen).decode()
                plen = conn.recv(1)[0]
                password = conn.recv(plen).decode()
                if (user, password) != require_auth:
                    conn.sendall(bytes([1, 1]))
                    return
                conn.sendall(bytes([1, 0]))
            else:
                conn.sendall(bytes([5, 0]))

            request = conn.recv(4)
            if len(request) < 4 or request[1] != 1:
                return
            atyp = request[3]
            if atyp == 1:
                host = socket.inet_ntoa(conn.recv(4))
            elif atyp == 3:
                host = conn.recv(conn.recv(1)[0]).decode()
            else:
                conn.recv(16)
                return
            port_bytes = conn.recv(2)
            target_port = int.from_bytes(port_bytes, "big")

            upstream = socket.create_connection((host, target_port), timeout=10)
            counter[0] += 1
            conn.sendall(bytes([5, 0, 0, 1]) + socket.inet_aton("0.0.0.0") + (0).to_bytes(2, "big"))
            threading.Thread(target=pipe, args=(conn, upstream), daemon=True).start()
            pipe(upstream, conn)
        except OSError:
            pass
        finally:
            try:
                conn.close()
            except OSError:
                pass

    def serve():
        while not stop.is_set():
            try:
                conn, _ = listener.accept()
            except OSError:
                return
            threading.Thread(target=handle, args=(conn,), daemon=True).start()

    threading.Thread(target=serve, daemon=True).start()

    def shutdown():
        stop.set()
        listener.close()

    return "socks5://127.0.0.1:%d" % port, shutdown
