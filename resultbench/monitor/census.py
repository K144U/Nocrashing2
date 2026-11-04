"""Hosting census of result portals (field study, gap analysis option A).

One polite pass over every target in targets.yaml, outside result days:
who answers (addresses, reverse DNS, CNAME aliases), whether a CDN sits in
front, TLS issuer and expiry, HTTP version, and how the landing page answers.
It sends one GET per target (robots.txt respected) and needs no result day,
so it can run months before the season and be repeated to see changes.

    python -m monitor.census --config monitor/targets.yaml --out field/census-2026-11.json

Writes the full records as JSON and prints a Markdown table.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import socket
import ssl
import subprocess
import sys
from pathlib import Path
from urllib.parse import urlsplit

import yaml

from monitor import monitor

# Substrings in CNAMEs, reverse DNS names or headers that identify a CDN or cloud.
CDN_SIGNS = [
    ("cloudflare", ["cloudflare", "cf-ray", "cf-cache-status"]),
    ("akamai", ["akamai", "edgekey", "edgesuite", "akamaiedge", "akamai-grn", "x-akamai"]),
    ("cloudfront", ["cloudfront", "x-amz-cf-id"]),
    ("fastly", ["fastly", "x-served-by"]),
    ("azure-front-door", ["azurefd", "azureedge", "x-azure-ref"]),
    ("google-cloud", ["googleusercontent", "1e100.net", "google frontend"]),
    ("aws", ["amazonaws"]),
    ("nic", ["nic.in"]),
]


def infer_hosting(headers: dict, names: list[str]) -> list[str]:
    """CDN or hosting providers suggested by headers and DNS names."""
    hay = " ".join([*(f"{k} {v}" for k, v in headers.items()), *names]).lower()
    return [name for name, signs in CDN_SIGNS if any(s in hay for s in signs)]


def dns_info(host: str) -> dict:
    out = {"host": host, "canonical": "", "aliases": [], "ipv4": [], "ipv6": [], "ptr": [], "ttl": None}
    try:
        canonical, aliases, _ = socket.gethostbyname_ex(host)
        out["canonical"], out["aliases"] = canonical, aliases
    except OSError as e:
        out["error"] = str(e)
        return out
    for fam, key in ((socket.AF_INET, "ipv4"), (socket.AF_INET6, "ipv6")):
        try:
            out[key] = sorted({ai[4][0] for ai in socket.getaddrinfo(host, 443, fam, socket.SOCK_STREAM)})
        except OSError:
            pass
    for ip in out["ipv4"][:4]:
        try:
            out["ptr"].append(socket.gethostbyaddr(ip)[0])
        except OSError:
            pass
    try:  # TTL and CNAME chain need dig; skip quietly if it is missing
        p = subprocess.run(["dig", "+noall", "+answer", host], capture_output=True, text=True, timeout=10)
        rows = [l.split() for l in p.stdout.splitlines() if l.strip() and not l.startswith(";")]
        if rows:
            out["ttl"] = min(int(r[1]) for r in rows if len(r) > 1 and r[1].isdigit())
            out["cname_chain"] = [r[4].rstrip(".") for r in rows if len(r) > 4 and r[3] == "CNAME"]
    except (OSError, subprocess.TimeoutExpired, ValueError):
        pass
    return out


def tls_info(host: str, port: int = 443, timeout: float = 10) -> dict:
    ctx = ssl.create_default_context()
    try:
        with socket.create_connection((host, port), timeout=timeout) as s, ctx.wrap_socket(s, server_hostname=host) as t:
            cert = t.getpeercert()
            issuer = dict(x[0] for x in cert.get("issuer", []))
            expires = dt.datetime.strptime(cert["notAfter"], "%b %d %H:%M:%S %Y %Z").date().isoformat()
            return {"tls": t.version(), "issuer": issuer.get("organizationName", issuer.get("commonName", "")),
                    "expires": expires, "alpn": t.selected_alpn_protocol()}
    except (OSError, ssl.SSLError, ValueError, KeyError) as e:
        return {"tls_error": str(e)[:200]}


def census(config: dict, timeout_s: float = 30) -> list[dict]:
    contact = config.get("contact", "").strip()
    if not contact or "example" in contact:
        raise SystemExit("set `contact` in the config (it goes into the User-Agent so site owners can reach you)")
    ua = f"ResultBench-FieldMonitor/1.0 (academic hosting census; {contact})"
    records = []
    for t in config["targets"]:
        url = t["url"]
        host = urlsplit(url).hostname or ""
        rec = {"target": t["id"], "group": t.get("group", ""), "url": url,
               "checked_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")}
        rec["dns"] = dns_info(host)
        if urlsplit(url).scheme == "https":
            rec.update(tls_info(host))
        if monitor.robots_allows(url, ua):
            p = monitor.probe(url, ua, timeout_s)
            rec["http"] = {k: p.get(k) for k in ("http_code", "time_total", "time_starttransfer", "size_download",
                                                   "remote_ip", "num_redirects", "url_effective", "title",
                                                   "soft_error", "ok", "headers", "errormsg")}
        else:
            rec["http"] = {"skipped": "robots.txt"}
        # The portal's own name says nothing about its hosting (every board
        # domain ends in .nic.in or .gov.in), so only aliases, CNAMEs and
        # reverse DNS count.
        canonical = rec["dns"].get("canonical", "")
        names = [*([canonical] if canonical != host else []), *rec["dns"].get("aliases", []),
                 *rec["dns"].get("ptr", []), *rec["dns"].get("cname_chain", [])]
        rec["hosting"] = infer_hosting(rec["http"].get("headers") or {}, names)
        records.append(rec)
        print(f"{t['id']}: {rec['http'].get('http_code', '-')} {','.join(rec['hosting']) or 'unknown'}", file=sys.stderr)
    return records


def table(records: list[dict]) -> str:
    rows = ["| Target | Status | Hosting hints | IPv6 | TLS issuer | Expires | DNS TTL | Server |",
            "|---|---|---|---|---|---|---|---|"]
    for r in records:
        h = r.get("http", {})
        rows.append("| {} | {} | {} | {} | {} | {} | {} | {} |".format(
            r["target"], h.get("http_code", h.get("skipped", "-")), ", ".join(r["hosting"]) or "-",
            "yes" if r["dns"].get("ipv6") else "no", r.get("issuer", r.get("tls_error", "-"))[:40],
            r.get("expires", "-"), r["dns"].get("ttl") if r["dns"].get("ttl") is not None else "-",
            (h.get("headers") or {}).get("server", "-")))
    return "\n".join(rows)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--config", type=Path, default=Path("monitor/targets.yaml"))
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--timeout", type=float, default=30)
    a = ap.parse_args()
    recs = census(yaml.safe_load(a.config.read_text()), a.timeout)
    a.out.parent.mkdir(parents=True, exist_ok=True)
    a.out.write_text(json.dumps(recs, indent=2) + "\n")
    print(table(recs))


if __name__ == "__main__":
    main()
