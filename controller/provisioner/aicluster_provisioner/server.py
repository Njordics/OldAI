"""HTTP/JSON API used by the Go core.

    GET  /healthz
    GET  /catalog            full catalog
    POST /fit                {"nodes": [...], "models"?: [...], "quant"?: str, "exclude"?: [...]}

Listens on a unix socket (production) or loopback TCP (development).
"""

from __future__ import annotations

import json
import logging
import os
import socket
import socketserver
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from . import fit as fitmod
from .catalog import CatalogStore

log = logging.getLogger(__name__)
MAX_BODY = 4 << 20


class Handler(BaseHTTPRequestHandler):
    store: CatalogStore  # set by make_server
    server_version = "aicluster-provisioner"

    def log_message(self, fmt, *args):  # route through logging, not stderr
        log.debug("%s " + fmt, self.address_string(), *args)

    def address_string(self):
        return self.client_address[0] if isinstance(self.client_address, tuple) and self.client_address else "unix"

    def _send(self, status: int, body: dict):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _error(self, status: int, code: str, msg: str):
        self._send(status, {"v": 1, "error": {"code": code, "message": msg, "retryable": status >= 500}})

    def do_GET(self):
        if self.path == "/healthz":
            return self._send(200, {"ok": True, "catalog_error": self.store.error})
        if self.path == "/catalog":
            try:
                cat = self.store.get()
            except Exception as e:  # noqa: BLE001 - report any load failure
                return self._error(503, "catalog_unavailable", str(e))
            body = cat.to_json()
            body["error"] = self.store.error
            return self._send(200, body)
        self._error(404, "not_found", self.path)

    def do_POST(self):
        handler = {"/fit": fitmod.fit, "/pool": fitmod.pool}.get(self.path)
        if handler is None:
            return self._error(404, "not_found", self.path)
        n = int(self.headers.get("Content-Length") or 0)
        if n > MAX_BODY:
            return self._error(413, "bad_request", "body too large")
        try:
            req = json.loads(self.rfile.read(n) or b"{}")
        except json.JSONDecodeError as e:
            return self._error(400, "bad_request", str(e))
        try:
            return self._send(200, handler(self.store.get(), req))
        except KeyError as e:
            return self._error(404, "unknown_model", str(e.args[0]))
        except Exception as e:  # noqa: BLE001
            log.exception("fit failed")
            return self._error(500, "internal", str(e))


if hasattr(socketserver, "UnixStreamServer"):  # not on Windows dev machines
    class UnixHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
        daemon_threads = True

        def get_request(self):
            req, _ = super().get_request()
            return req, ("unix", 0)


def make_server(listen: str, store: CatalogStore):
    handler = type("BoundHandler", (Handler,), {"store": store})
    if listen.startswith("unix:"):
        path = listen[len("unix:"):]
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass
        srv = UnixHTTPServer(path, handler)
        os.chmod(path, 0o660)
        return srv
    host, _, port = listen.removeprefix("tcp:").rpartition(":")
    # On Windows SO_REUSEADDR lets a second process bind the same port and
    # silently split traffic; refuse instead.
    ThreadingHTTPServer.allow_reuse_address = os.name != "nt"
    srv = ThreadingHTTPServer((host or "127.0.0.1", int(port)), handler)
    srv.daemon_threads = True
    return srv


def bound_address(srv) -> str:
    if srv.socket.family == getattr(socket, "AF_UNIX", None):
        return "unix:" + srv.server_address
    host, port = srv.server_address[:2]
    return f"tcp:{host}:{port}"
