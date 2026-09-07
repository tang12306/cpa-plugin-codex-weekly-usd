"""Local stand-ins for the two things a probe talks to.

The probe path is no longer a host callback the harness can fake, so the tests
exercise it for real: the plugin dials a socket, speaks SOCKS5 when the
credential asks for it, sends the request and parses the quota out of the
response headers. Faking that at the callback boundary would have tested none
of it - and the last defect in this area was precisely that probes left by a
route nobody had checked.
"""
import http.server
import json
import os
import socket
import threading


class _Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("content-length") or 0)
        if length:
            self.rfile.read(length)
        token = (self.headers.get("authorization") or "").replace("Bearer ", "").strip()
        spec = self.server.specs.get(token)
        self.server.hits.append({
            "token": token,
            "model": self.headers.get("x-probe-model") or "",
            "via_socks": self.server.socks_hits[0] > 0,
        })
        if spec is None:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(spec.get("StatusCode", 200))
        for key, values in (spec.get("Headers") or {}).items():
            for value in values:
                self.send_header(key, value)
        self.send_header("content-length", "0")
        self.end_headers()

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
