#!/usr/bin/env python3
"""A tiny local stand-in for Paystack's /refund endpoint, for Phase 17a
manual QA. It logs every request and answers a canned success envelope so
orders.Service.ProcessRefund's real HTTP client (not the fake.Provider used
by automated tests) can be exercised end to end."""
import http.server
import json
import sys

counter = {"n": 0}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        counter["n"] += 1
        print(f"stub: POST {self.path} body={body.decode()}", file=sys.stderr, flush=True)
        resp = {
            "status": True,
            "message": "Refund has been queued for processing",
            "data": {"id": counter["n"], "status": "pending", "amount": json.loads(body).get("amount", 0)},
        }
        payload = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, format, *args):
        pass


if __name__ == "__main__":
    server = http.server.HTTPServer(("127.0.0.1", 9911), Handler)
    server.serve_forever()
