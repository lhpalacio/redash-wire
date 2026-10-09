"""A stand-in Redash for the README screenshots: just enough of the API for
the proxy to start, report healthy and list made-up data sources.

    python3 dev/fake-redash.py [port]
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

DATA_SOURCES = [
    {"id": 1, "name": "Analytics", "type": "pg"},
    {"id": 2, "name": "Warehouse", "type": "redshift"},
    {"id": 3, "name": "Orders", "type": "mysql"},
    {"id": 4, "name": "Billing", "type": "mysql"},
    {"id": 5, "name": "Events", "type": "bigquery"},
]

SESSION = {
    "user": {"name": "Demo User", "email": "demo@example.com"},
    "client_config": {"version": "10.1.0"},
}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        routes = {"/api/data_sources": DATA_SOURCES, "/api/session": SESSION}
        body = routes.get(self.path.split("?")[0])
        if body is None:
            self.send_error(404)
            return
        payload = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


port = int(sys.argv[1]) if len(sys.argv) > 1 else 18080
HTTPServer(("127.0.0.1", port), Handler).serve_forever()
