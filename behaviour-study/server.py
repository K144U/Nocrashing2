"""Mock result portal for the student-behaviour study (gap G3 in
reports/next-paper-gap-analysis.md).

Each participant opens the page with their anonymous code and tries to get a
mock result. The server withholds the result for RELEASE_AFTER_S seconds and,
depending on the participant's condition, answers slowly, with errors, or with
a waiting-room screen. The page reports every load, refresh, poll, tab switch
and "give up" click; the server appends them to a JSON-lines log with its own
timestamp. No personal data is collected: only the code on the consent sheet.

    python3 server.py --port 8000 --log events.jsonl
    # participants open http://<laptop>:8000/?p=<code>

Conditions (one per participant, assigned from the code unless the URL says
otherwise with &c=<condition>):

    slow       the result request hangs, then fails with 504 after SLOW_HOLD_S
    error      the result request fails with 503 after 1-3 s
    queue      waiting-room screen with a queue position, polled every 5 s
    queue-eta  as queue, plus an estimate of the remaining wait

Standard library only.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import random
import threading
import time
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

CONDITIONS = ["slow", "error", "queue", "queue-eta"]
HERE = Path(__file__).resolve().parent


class Study:
    """Participant state and the event log. Thread-safe."""

    def __init__(self, log_path: Path, release_after_s: float, slow_hold_s: float, clock=time.time):
        self.log_path = log_path
        self.release_after_s = release_after_s
        self.slow_hold_s = slow_hold_s
        self.clock = clock
        self.lock = threading.Lock()
        self.first_seen: dict[str, float] = {}
        self.queue_start: dict[str, int] = {}

    @staticmethod
    def condition_for(code: str) -> str:
        h = int(hashlib.sha256(code.encode()).hexdigest(), 16)
        return CONDITIONS[h % len(CONDITIONS)]

    def log(self, event: dict) -> None:
        event = {"server_t": round(self.clock(), 3), **event}
        with self.lock, self.log_path.open("a") as f:
            f.write(json.dumps(event, sort_keys=True) + "\n")

    def start(self, code: str) -> float:
        with self.lock:
            if code not in self.first_seen:
                self.first_seen[code] = self.clock()
                self.queue_start[code] = random.Random(code).randint(4000, 12000)
            return self.first_seen[code]

    def answer(self, code: str, condition: str) -> tuple[int, dict, float]:
        """Return (status, body, delay_s) for one result request."""
        start = self.start(code)
        elapsed = self.clock() - start
        remaining = self.release_after_s - elapsed
        if remaining <= 0:
            return 200, {"status": "result", "roll": code, "result": mock_result(code)}, 0.0
        if condition == "slow":
            hold = min(self.slow_hold_s, remaining)
            if hold >= remaining:  # the result arrives during this request
                return 200, {"status": "result", "roll": code, "result": mock_result(code)}, remaining
            return 504, {"status": "error", "message": "Gateway Timeout"}, hold
        if condition == "error":
            delay = random.uniform(1, 3)
            return 503, {"status": "error", "message": "Service Unavailable"}, delay
        p0 = self.queue_start[code]
        position = max(1, round(p0 * remaining / self.release_after_s))
        body = {"status": "queued", "position": position, "retry_after_ms": 5000}
        if condition == "queue-eta":
            body["eta_s"] = round(remaining)
        return 200, body, 0.0


def mock_result(code: str) -> dict:
    r = random.Random("result:" + code)
    subjects = ["Mathematics", "Physics", "Chemistry", "English", "Computer Science"]
    marks = {s: r.randint(55, 98) for s in subjects}
    return {"marks": marks, "total": sum(marks.values()), "max": 100 * len(subjects), "note": "Mock result for a study"}


def make_handler(study: Study):
    page = (HERE / "static" / "index.html").read_bytes()

    class Handler(BaseHTTPRequestHandler):
        server_version = "BehaviourStudy/1"

        def log_message(self, *args):  # keep the console quiet
            pass

        def _json(self, status: int, body: dict) -> None:
            data = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _params(self) -> tuple[str, str]:
            q = parse_qs(urlparse(self.path).query)
            code = (q.get("p") or [""])[0][:32]
            cond = (q.get("c") or [""])[0]
            if cond not in CONDITIONS:
                cond = Study.condition_for(code) if code else ""
            return code, cond

        def do_GET(self):
            path = urlparse(self.path).path
            if path == "/":
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Content-Length", str(len(page)))
                self.end_headers()
                self.wfile.write(page)
                return
            if path == "/api/result":
                code, cond = self._params()
                if not code:
                    self._json(400, {"status": "error", "message": "missing participant code"})
                    return
                status, body, delay = study.answer(code, cond)
                study.log({"p": code, "c": cond, "type": "request", "status": status, "delay_s": round(delay, 3),
                           "answer": body["status"]})
                if delay > 0:
                    time.sleep(delay)
                self._json(status, body)
                return
            self._json(404, {"status": "error", "message": "not found"})

        def do_POST(self):
            if urlparse(self.path).path != "/api/event":
                self._json(404, {"status": "error"})
                return
            n = min(int(self.headers.get("Content-Length") or 0), 64 << 10)
            try:
                ev = json.loads(self.rfile.read(n) or b"{}")
            except json.JSONDecodeError:
                self._json(400, {"status": "error"})
                return
            code, cond = self._params()
            allowed = {"type", "n_load", "client_t", "visible", "answers", "detail"}
            study.log({"p": code, "c": cond, **{k: v for k, v in ev.items() if k in allowed}})
            self._json(200, {"status": "ok"})

    return Handler


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--port", type=int, default=8000)
    ap.add_argument("--host", default="0.0.0.0")
    ap.add_argument("--log", type=Path, default=Path("events.jsonl"))
    ap.add_argument("--release-after", type=float, default=240, help="seconds from first visit to the result")
    ap.add_argument("--slow-hold", type=float, default=25, help="seconds a request hangs in the slow condition")
    a = ap.parse_args()
    study = Study(a.log, a.release_after, a.slow_hold)
    srv = ThreadingHTTPServer((a.host, a.port), make_handler(study))
    print(f"study server on http://{a.host}:{a.port}/?p=<code>  log: {a.log}")
    srv.serve_forever()


if __name__ == "__main__":
    main()
