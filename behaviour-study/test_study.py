"""Tests for the study server and analysis: python3 -m pytest behaviour-study"""
import json
import sys
import threading
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import analyze  # noqa: E402
import server  # noqa: E402


class FakeClock:
    def __init__(self):
        self.t = 1000.0

    def __call__(self):
        return self.t


def test_conditions_and_release(tmp_path):
    clock = FakeClock()
    st = server.Study(tmp_path / "e.jsonl", release_after_s=100, slow_hold_s=25, clock=clock)
    code, body, delay = st.answer("p1", "slow")
    assert code == 504 and delay == 25
    code, body, _ = st.answer("p1", "queue-eta")
    assert code == 200 and body["status"] == "queued" and body["eta_s"] == 100
    p0 = body["position"]
    clock.t += 50
    _, body, _ = st.answer("p1", "queue")
    assert "eta_s" not in body and body["position"] < p0
    clock.t += 40
    code, body, delay = st.answer("p1", "slow")  # result arrives during the held request
    assert code == 200 and body["status"] == "result" and abs(delay - 10) < 1e-9
    clock.t += 20
    code, body, _ = st.answer("p1", "error")
    assert code == 200 and body["status"] == "result"


def test_condition_assignment_is_balanced():
    counts = {c: 0 for c in server.CONDITIONS}
    for i in range(400):
        counts[server.Study.condition_for(f"P{i:03d}")] += 1
    assert min(counts.values()) > 70


def test_http_end_to_end(tmp_path):
    log = tmp_path / "events.jsonl"
    st = server.Study(log, release_after_s=0.6, slow_hold_s=0.2)
    srv = ThreadingHTTPServer(("127.0.0.1", 0), server.make_handler(st))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{srv.server_address[1]}"
    try:
        page = urllib.request.urlopen(base + "/").read().decode()
        assert "Mock Examination Board" in page
        with _expect_http(504):
            urllib.request.urlopen(base + "/api/result?p=A1&c=slow")
        req = urllib.request.Request(base + "/api/event?p=A1&c=slow", data=json.dumps(
            {"type": "load", "n_load": 2, "secret": "dropped"}).encode(), method="POST")
        urllib.request.urlopen(req).read()
        import time
        time.sleep(0.6)
        body = json.loads(urllib.request.urlopen(base + "/api/result?p=A1&c=slow").read())
        assert body["status"] == "result"
    finally:
        srv.shutdown()
    events = [json.loads(l) for l in log.read_text().splitlines()]
    assert [e["type"] for e in events] == ["request", "load", "request"]
    assert "secret" not in events[1]


class _expect_http:
    def __init__(self, code):
        self.code = code

    def __enter__(self):
        return self

    def __exit__(self, typ, val, tb):
        assert typ is urllib.error.HTTPError and val.code == self.code
        return True


def _ev(p, c, t, typ, **kw):
    return {"p": p, "c": c, "server_t": t, "type": typ, **kw}


def test_analysis(tmp_path):
    evs = []
    # slow: reloads 8 s into a held request, then 12 s into the next, then leaves.
    evs += [_ev("s1", "slow", 0, "request", status=504, delay_s=25), _ev("s1", "slow", 8, "load", n_load=2),
            _ev("s1", "slow", 9, "request", status=504, delay_s=25), _ev("s1", "slow", 21, "leave")]
    # error: retries 4 s after each error, gets the result.
    evs += [_ev("e1", "error", 0, "request", status=503, delay_s=2), _ev("e1", "error", 6, "request", status=503, delay_s=2),
            _ev("e1", "error", 12, "request", status=200, delay_s=0), _ev("e1", "error", 12.1, "result_shown")]
    # queue: two participants, one reloads once and gives up at 60 s, one waits for the result.
    evs += [_ev("q1", "queue", 0, "request", status=200, delay_s=0), _ev("q1", "queue", 30, "load", n_load=2),
            _ev("q1", "queue", 31, "request", status=200, delay_s=0), _ev("q1", "queue", 60, "leave")]
    evs += [_ev("q2", "queue", 0, "request", status=200, delay_s=0), _ev("q2", "queue", 240, "result_shown")]
    log = tmp_path / "e.jsonl"
    log.write_text("\n".join(json.dumps(e) for e in evs))
    parts = [analyze.participant(e) for e in analyze.load(log).values()]
    by = {p["condition"]: p for p in parts if p["condition"] != "queue"}
    assert by["slow"]["patience"] == [8, 12] and by["slow"]["outcome"] == "leave"
    assert by["error"]["retry_delays"] == [4, 4] and by["error"]["errors_retried"] == 2
    q = analyze.summarise([p for p in parts if p["condition"] == "queue"])
    assert q["gave_up_share"] == 0.5 and q["give_up_p50_s"] == 60
    sug = analyze.suggest({"slow": analyze.summarise([by["slow"]]), "error": analyze.summarise([by["error"]]), "queue": q})
    assert sug["users"]["retry_prob"] == 1.0 and sug["users"]["retry_delay_s"] == [4, 4]
    assert sug["users"]["queue_patience_s"] == [60, 60]
    assert "users:" in analyze.to_yaml(sug)


def test_kaplan_meier_censoring():
    curve = analyze.kaplan_meier([10, 20, 30, 40], [True, False, True, False])
    assert curve == [(0.0, 1.0), (10, 0.75), (30, 0.375)]
