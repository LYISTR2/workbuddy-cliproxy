#!/usr/bin/env python3
"""Refresh expired WorkBuddy catalogs and CPA registrations without restarting CPA."""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def catalogs(row):
    if isinstance(row.get("catalog"), dict) and row["catalog"].get("availability") != "metadata_unavailable":
        return [row["catalog"]]
    return [v["catalog"] for v in row.get("catalog_variants", [])
            if isinstance(v, dict) and isinstance(v.get("catalog"), dict)]


def summarize(rows):
    entries = [c for row in rows for c in catalogs(row)]
    return {"model_count": len(rows), "catalog_count": len(entries),
            "unavailable_count": sum(not catalogs(row) for row in rows),
            "stale_count": sum(bool(c.get("stale")) for c in entries),
            "channels": sorted({c["channel"] for c in entries if c.get("channel")}),
            "model_ids": [row["id"] for row in rows]}


def refresh(call, clock=time.monotonic, pause=time.sleep):
    plugins = call("/v0/management/plugins")
    plugin = next((p for p in plugins.get("plugins", []) if p.get("id") == "workbuddy"), None)
    if not plugin or not plugin.get("effective_enabled"):
        return {"status": "skipped", "reason": "plugin_disabled"}
    before = [r for r in call("/v1/models", "api_key").get("data", [])
              if r.get("owned_by") == "workbuddy"]
    if not before:
        return {"status": "skipped", "reason": "no_registered_workbuddy_models"}
    if not any("catalog" in r or "catalog_variants" in r for r in before):
        return {"status": "skipped", "reason": "dynamic_catalog_not_advertised"}

    stats = summarize(before)
    fresh = not stats["unavailable_count"] and not stats["stale_count"]
    rows = before
    if not fresh:
        previous = sorted(c.get("fetched_at", "") for r in before for c in catalogs(r))
        call("/v0/management/plugins/workbuddy/config", data={"_catalog_refresh": time.time_ns()})
        deadline = clock() + 45
        while clock() < deadline:
            rows = [r for r in call("/v1/models", "api_key").get("data", [])
                    if r.get("owned_by") == "workbuddy"]
            stats = summarize(rows)
            stamps = sorted(c.get("fetched_at", "") for r in rows for c in catalogs(r))
            # Unavailable rows are reported separately, including a total outage.
            if stats["unavailable_count"] or (stamps and stamps != previous and not stats["stale_count"]):
                break
            pause(1)
        else:
            return {"status": "partial", "reason": "catalog_refresh_not_confirmed", **stats}
    files = call("/v0/management/auth-files").get("files", [])
    accounts = []
    for item in files:
        if item.get("type", item.get("provider")) != "workbuddy":
            continue
        name = item.get("name", "")
        models = call("/v0/management/auth-files/models?name=" + urllib.parse.quote(name, safe=""))
        accounts.append({"name": name, "model_count": len(models.get("models", []))})
    return {"status": "partial" if stats["unavailable_count"] or stats["stale_count"] else
            "fresh" if fresh else "refreshed", **stats, "accounts": accounts,
            "time_utc": dt.datetime.now(dt.timezone.utc).isoformat()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8317")
    parser.add_argument("--credentials-file")
    args = parser.parse_args()
    path = Path(args.credentials_file) if args.credentials_file else (
        Path(os.environ.get("CREDENTIALS_DIRECTORY", "/run/credentials/codebuddy-model-refresh.service")) / "management.json"
    )
    credentials = json.loads(path.read_text(encoding="utf-8"))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    base = args.base_url.rstrip("/")

    def call(route, key="management_key", data=None):
        headers = {"Authorization": "Bearer " + credentials[key]}
        payload = None
        if data is not None:
            headers["Content-Type"] = "application/json"
            payload = json.dumps(data).encode()
        request = urllib.request.Request(base + route, data=payload, headers=headers,
                                         method="PATCH" if data is not None else "GET")
        with opener.open(request, timeout=15) as response:
            return json.load(response)

    print(json.dumps(refresh(call), ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except urllib.error.HTTPError as error:
        print(json.dumps({"status": "failed", "reason": "management_http_error", "http_status": error.code}))
        raise SystemExit(1)
    except Exception as error:
        print(json.dumps({"status": "failed", "reason": type(error).__name__}))
        raise SystemExit(1)
