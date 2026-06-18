#!/usr/bin/env python3
"""Minimal stand-in for the E2 broker, serving the frozen contract from the golden
fixtures so the menu-bar app can be exercised without a live daemon (E3/E4 absent).

Serves:
  GET  /healthz                      -> testdata/api/healthz.json (auth-exempt)
  GET  /builds                       -> testdata/api/builds.json   (bearer)
  GET  /builds/{id}                  -> testdata/api/build.json    (bearer)
  POST /builds/{id}/kill             -> {"killed":true,...}        (bearer)
  WS   /events                       -> snapshot frame, then idle  (bearer on upgrade)

Writes ~/.config/bazel-broker/config.json (override dir via $XDG_CONFIG_HOME or the
config path via $BAZEL_BROKER_CONFIG) so TokenLoader finds the token. Loopback only.

Run:  python3 stub-broker.py [port]
"""
import base64
import hashlib
import json
import os
import socket
import struct
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8765
TOKEN = "stubtoken00000000000000000000000000000000000000000000000000000000"
REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
FIX = os.path.join(REPO, "testdata", "api")

WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def fixture(name):
    with open(os.path.join(FIX, name), "rb") as f:
        return f.read()


def write_config():
    if os.environ.get("BAZEL_BROKER_CONFIG"):
        path = os.environ["BAZEL_BROKER_CONFIG"]
    else:
        base = os.environ.get("XDG_CONFIG_HOME") or os.path.join(
            os.path.expanduser("~"), ".config"
        )
        path = os.path.join(base, "bazel-broker", "config.json")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump({"host": "127.0.0.1", "port": PORT, "token": TOKEN}, f)
    os.chmod(path, 0o600)
    print(f"wrote config: {path}")


def ws_frame(payload: bytes) -> bytes:
    header = bytearray([0x81])  # FIN + text
    n = len(payload)
    if n < 126:
        header.append(n)
    elif n < 65536:
        header.append(126)
        header += struct.pack(">H", n)
    else:
        header.append(127)
        header += struct.pack(">Q", n)
    return bytes(header) + payload


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def authed(self):
        return self.headers.get("Authorization") == f"Bearer {TOKEN}"

    def send_json(self, code, body: bytes):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/healthz":
            return self.send_json(200, fixture("healthz.json"))
        if path == "/events":
            return self.handle_ws()
        if not self.authed():
            return self.send_json(401, b'{"error":"unauthorized"}')
        if path == "/builds":
            return self.send_json(200, fixture("builds.json"))
        if path.startswith("/builds/"):
            return self.send_json(200, fixture("build.json"))
        self.send_json(404, b'{"error":"not_found"}')

    def do_POST(self):
        if not self.authed():
            return self.send_json(401, b'{"error":"unauthorized"}')
        path = self.path.split("?")[0]
        if path.startswith("/builds/") and path.endswith("/kill"):
            inv = path[len("/builds/"):-len("/kill")]
            body = json.dumps(
                {"killed": True, "invocation_id": inv, "pid": 4242,
                 "outcome": "sigint", "elapsed_ms": 3120}
            ).encode()
            print(f"KILL {inv}")
            return self.send_json(200, body)
        self.send_json(404, b'{"error":"not_found"}')

    def handle_ws(self):
        key = self.headers.get("Sec-WebSocket-Key")
        if not self.authed() or not key:
            return self.send_json(401, b'{"error":"unauthorized"}')
        accept = base64.b64encode(
            hashlib.sha1((key + WS_GUID).encode()).digest()
        ).decode()
        # Write the 101 handshake as raw bytes so no extra headers (Server/Date) or
        # auto-added Content-Length interfere with URLSession's WS client.
        handshake = (
            "HTTP/1.1 101 Switching Protocols\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Accept: {accept}\r\n\r\n"
        )
        # Send the snapshot event built from the fixture, then idle.
        snap = fixture("event_snapshot.json")
        try:
            self.connection.sendall(handshake.encode())
            self.connection.sendall(ws_frame(snap))
            # Keep the connection open; block until the client closes it.
            while True:
                data = self.connection.recv(1024)
                if not data:
                    break
        except OSError:
            pass


def main():
    write_config()
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"stub broker on http://127.0.0.1:{PORT} (token={TOKEN[:8]}…)")
    srv.serve_forever()


if __name__ == "__main__":
    main()
