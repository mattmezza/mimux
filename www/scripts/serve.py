#!/usr/bin/env python3
"""Static file server with gzip/brotli-less compression for local Lighthouse
runs — python's http.server sends everything identity-encoded, which makes
transfer-size audits unrealistically harsh compared to Cloudflare Pages."""
import gzip
import http.server
import os
import sys

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8732
COMPRESSIBLE = ("text/", "application/javascript", "application/json",
                "application/xml", "image/svg")


class Handler(http.server.SimpleHTTPRequestHandler):
    def end_headers(self):
        self.send_header("Cache-Control", "no-cache")
        super().end_headers()

    def send_head(self):
        path = self.translate_path(self.path)
        if os.path.isdir(path):
            path = os.path.join(path, "index.html")
        ctype = self.guess_type(path)
        if (ctype and ctype.startswith(COMPRESSIBLE)
                and "gzip" in self.headers.get("Accept-Encoding", "")):
            try:
                with open(path, "rb") as f:
                    body = gzip.compress(f.read(), compresslevel=6)
            except OSError:
                return super().send_head()
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Encoding", "gzip")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            import io
            return io.BytesIO(body)
        return super().send_head()

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
