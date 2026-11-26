"""A stub of the central control host for the path check budget run (#426).

HTTPS on 127.0.0.1:<port>: /health answers 200, /derp answers the DERP upgrade with a server key
frame, /blob serves <size> bytes. UDP on 127.0.0.1:3478 answers STUN binding requests.
Usage: python3 central_stub.py CERT KEY PORT SIZE
"""
import http.server
import socket
import ssl
import sys
import threading

cert, key, port, size = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
BLOB = b"\0" * size
DERP_MAGIC = b"DERP\xf0\x9f\x94\x91"


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"ok")
        elif self.path == "/derp":
            self.send_response(101)
            self.send_header("Upgrade", "DERP")
            self.send_header("Connection", "Upgrade")
            self.end_headers()
            body = DERP_MAGIC + b"\0" * 32
            self.wfile.write(b"\x01" + len(body).to_bytes(4, "big") + body)
        elif self.path == "/blob":
            self.send_response(200)
            self.send_header("Content-Length", str(len(BLOB)))
            self.end_headers()
            self.wfile.write(BLOB)
        else:
            self.send_error(404)


def stun():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.bind(("127.0.0.1", 3478))
    while True:
        req, addr = s.recvfrom(1500)
        if len(req) >= 20 and req[0:2] == b"\x00\x01":
            s.sendto(b"\x01\x01\x00\x00" + req[4:20], addr)


threading.Thread(target=stun, daemon=True).start()
srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.minimum_version = ssl.TLSVersion.TLSv1_2
ctx.load_cert_chain(cert, key)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
srv.serve_forever()
