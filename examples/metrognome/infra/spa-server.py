#!/usr/bin/env python3
"""SPA-aware static file server. Serves files if they exist, otherwise index.html."""
import http.server
import os
import sys

DIRECTORY = sys.argv[1] if len(sys.argv) > 1 else "."
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 9091
BIND = sys.argv[3] if len(sys.argv) > 3 else "127.0.0.1"


class SPAHandler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=DIRECTORY, **kwargs)

    def do_GET(self):
        # If the path maps to a real file, serve it normally.
        path = self.translate_path(self.path)
        if os.path.isfile(path):
            return super().do_GET()
        # Otherwise serve index.html (SPA client-side routing).
        self.path = "/index.html"
        return super().do_GET()

    def log_message(self, format, *args):
        pass  # silence logs


if __name__ == "__main__":
    server = http.server.HTTPServer((BIND, PORT), SPAHandler)
    print(f"SPA server on {BIND}:{PORT} serving {DIRECTORY}", flush=True)
    server.serve_forever()
