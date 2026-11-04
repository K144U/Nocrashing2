import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pandas as pd

from monitor import monitor
from monitor.summarize import episodes, summarize


def test_classify_body():
    ok = monitor.classify_body(b"<html><title> Board Results 2027 </title>Enter roll number</html>")
    assert ok["title"] == "Board Results 2027" and ok["soft_error"] == ""
    busy = monitor.classify_body(b"<html><title>Results</title>Server is busy due to heavy traffic. Try again later</html>")
    assert busy["soft_error"]


def test_parse_headers_keeps_last_response():
    raw = "HTTP/1.1 301 Moved\r\nServer: a\r\n\r\nHTTP/1.1 200 OK\r\nServer: nginx\r\nCF-Cache-Status: HIT\r\nX-Other: 1\r\n\r\n"
    h = monitor.parse_headers(raw)
    assert h == {"server": "nginx", "cf-cache-status": "HIT"}


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"<title>Results</title>Service Temporarily Unavailable"
        self.send_response(200)
        self.send_header("Server", "test")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


def test_probe_detects_soft_failure():
    srv = HTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    rec = monitor.probe(f"http://127.0.0.1:{srv.server_port}/", "test-agent", 5)
    srv.shutdown()
    assert rec["http_code"] == 200
    assert rec["soft_error"] and not rec["ok"]
    assert rec["headers"]["server"] == "test"
    assert rec["time_total"] > 0


def test_episodes():
    per_min = pd.DataFrame({"minute": range(-5, 30), "availability": [1.0] * 7 + [0.1] * 10 + [1.0] * 18})
    e = episodes(per_min)
    assert e["onset_min"] == 2 and e["minutes_degraded"] == 10 and e["recovered_min"] == 12


def test_summarize(tmp_path):
    ev = tmp_path / "ev"
    ev.mkdir()
    lines = []
    for t in range(-600, 1800, 20):
        down = 0 <= t < 600
        lines.append(json.dumps({"ts": 1e9 + t, "t_rel_s": t, "target": "board", "vantage": "home",
                                 "http_code": 503 if down else 200, "time_starttransfer": 2.0 if down else 0.2,
                                 "ok": not down, "soft_error": "", "headers": {"server": "nginx"}}))
    (ev / "home.jsonl").write_text("\n".join(lines))
    out = summarize(ev)
    s = pd.read_csv(out / "summary.csv")
    assert s.loc[0, "onset_min"] == 0 and s.loc[0, "minutes_degraded"] == 10
    assert (out / "board.png").exists()


def test_infer_hosting():
    from monitor.census import infer_hosting
    assert infer_hosting({"cf-ray": "abc", "server": "cloudflare"}, []) == ["cloudflare"]
    assert infer_hosting({"server": "nginx"}, ["e1234.a.akamaiedge.net"]) == ["akamai"]
    assert infer_hosting({"server": "Apache"}, ["webhost.nic.in"]) == ["nic"]
    assert infer_hosting({"server": "Apache"}, []) == []


def test_census_against_local_server():
    from monitor.census import census, table

    class Ok(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.send_header("Server", "test-server")
            self.send_header("CF-Cache-Status", "MISS")
            self.end_headers()
            self.wfile.write(b"<title>Board Results</title>Enter roll number")

        def log_message(self, *a):
            pass

    srv = HTTPServer(("127.0.0.1", 0), Ok)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        cfg = {"contact": "Tester <t@uni.ac.in>",
               "targets": [{"id": "local", "group": "test", "url": f"http://127.0.0.1:{srv.server_address[1]}/"}]}
        recs = census(cfg, timeout_s=5)
    finally:
        srv.shutdown()
    r = recs[0]
    assert r["http"]["http_code"] == 200 and r["http"]["title"] == "Board Results"
    assert r["hosting"] == ["cloudflare"]
    assert "| local | 200 | cloudflare |" in table(recs)
