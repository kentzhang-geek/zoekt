#!/usr/bin/env python3
"""Read-only Unreal baseline via Zoekt's existing /api/search HTTP API.

Python 3.10+, standard library only. Does not build, index, restart, or drop caches.
This measures native search + JSON HTTP, not the proposed MCP search interface.
"""

import argparse
import hashlib
import http.client
import json
import math
import platform
import re
import statistics
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit


HEADER = r"(^|/)RenderCore/Public/RenderGraphBuilder[.]h$"
MODULE = r"(^|/)RenderCore/.*[.](cpp|h)$"
CALL = r"\bRegisterExternal(Texture|Buffer)\s*\("
CASES = [
    dict(id="files", query=r"file:(^|/)RenderCore/.*(RenderGraphBuilder|RenderingThread)[.]h$ type:filename",
         path=r"/RenderCore/.*(RenderGraphBuilder|RenderingThread)[.]h$", min_files=2),
    dict(id="literal_scoped", query=f"file:{HEADER} content:RegisterExternalTexture", path=HEADER),
    dict(id="alternation_scoped", query=f"file:{HEADER} content:(Register|Queue)External(Texture|Buffer)", path=HEADER),
    dict(id="calls_scoped", query=f"file:{MODULE} content:{CALL}", path=MODULE),
    dict(id="calls_unscoped", query=f"content:{CALL}", allow_display_limit=True),
    dict(id="multiline", query=f"file:{HEADER} content:(?s:RegisterExternalTexture\\(.{{0,250}}ERDGTextureFlags)", path=HEADER),
    dict(id="file_and", query=f"file:{HEADER} content:RegisterExternalTexture content:ERDGTextureFlags", path=HEADER),
    dict(id="case_insensitive", query=f"file:{HEADER.lower()} content:registerexternal(texture|buffer)", path=HEADER),
    dict(id="wrong_path_zero", query=r"file:(^|/)RenderCore/Public/RenderingThread[.]h$ content:RegisterExternal(Texture|Buffer)",
         max_files=0, min_files=0),
    dict(id="weak_branch", query=r"content:(RegisterExternalTexture|xx.+)", stress=True),
]
COUNTERS = (
    "FileCount", "MatchCount", "FilesConsidered", "FilesLoaded", "FilesSkipped",
    "ShardsScanned", "ShardsSkipped", "Crashes", "ContentBytesLoaded",
    "IndexBytesLoaded", "RegexpsConsidered", "NgramLookups", "NgramMatches",
)


class Client:
    def __init__(self, url, timeout):
        endpoint = urlsplit(url)
        if endpoint.scheme not in ("http", "https") or not endpoint.hostname:
            raise ValueError("--url must be an http(s) server URL")
        if endpoint.username or endpoint.password or endpoint.query or endpoint.fragment:
            raise ValueError("--url must not contain credentials, query, or fragment")
        cls = http.client.HTTPSConnection if endpoint.scheme == "https" else http.client.HTTPConnection
        self.connection = cls(endpoint.hostname, endpoint.port, timeout=timeout)
        self.base_path = endpoint.path.rstrip("/")

    def post(self, path, body):
        payload = json.dumps(body).encode("utf-8")
        start = time.perf_counter_ns()
        self.connection.request("POST", self.base_path + path, payload, {"Content-Type": "application/json"})
        response = self.connection.getresponse()
        raw = response.read()
        elapsed_ms = (time.perf_counter_ns() - start) / 1e6
        if response.status != 200:
            raise RuntimeError(f"HTTP {response.status}: {raw[:300]!r}")
        document = json.loads(raw)
        if document.get("Error"):
            raise RuntimeError(document["Error"])
        return document, elapsed_ms, len(raw)

    def close(self):
        self.connection.close()


def distribution(values):
    ordered = sorted(values)
    return {
        "min": min(values), "p50": statistics.median(values),
        "p95": ordered[math.ceil(len(ordered) * 0.95) - 1],
        "max": max(values), "mean": statistics.mean(values),
    }


def measure(client, repo, case, opts):
    # Quoting escapes the native query string's quote/backslash layer as well.
    query = "r:" + json.dumps("^" + re.escape(repo) + "$") + " case:no " + case["query"]
    document, http_ms, size = client.post("/api/search", {"Q": query, "Opts": opts})
    result = document["Result"]
    files = result.get("Files") or []
    errors = []
    if len(files) < case.get("min_files", 1):
        errors.append("expected positive file matches are missing")
    if len(files) > case.get("max_files", opts["MaxDocDisplayCount"]):
        errors.append("unexpected file matches")
    signatures = []
    for file in files:
        if file["Repository"] != repo:
            errors.append("repository filter leaked")
        # Legacy JSON returns native absolute paths; normalize only for assertions.
        if "path" in case and not re.search(case["path"], file["FileName"].replace("\\", "/"), re.IGNORECASE):
            errors.append("path filter leaked")
        matches = file.get("LineMatches") or []
        signatures.append((file["Repository"], file["FileName"], sorted(
            (m["LineNumber"], m["Line"]) for m in matches)))
    signature = hashlib.sha256(json.dumps(sorted(signatures)).encode()).hexdigest()
    signals = [key for key in ("FilesSkipped", "ShardsSkipped", "Crashes") if result.get(key, 0)]
    if result.get("FileCount", 0) > len(files):
        signals.append("files_omitted_from_response")
    if len(files) >= opts["MaxDocDisplayCount"]:
        signals.append("at_file_display_limit")
    if result["Duration"] >= opts["MaxWallTime"] * 0.95:
        signals.append("near_deadline")
    if result.get("Crashes", 0):
        errors.append("shard crash or index not ready")
    return {
        "engine_ms": result["Duration"] / 1e6,
        "queue_ms": result.get("Wait", 0) / 1e6,
        "http_ms": http_ms, "response_bytes": size, "returned_files": len(files),
        "counters": {key: result.get(key, 0) for key in COUNTERS},
        "partial_or_limit_signals": signals, "signature": signature,
        "errors": sorted(set(errors)),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:6070")
    parser.add_argument("--repo", default="ue")
    parser.add_argument("--runs", type=int, default=30)
    parser.add_argument("--warmup", type=int, default=3)
    parser.add_argument("--timeout-ms", type=int, default=3000)
    parser.add_argument("--case", action="append", help="Case ID; repeat to select several")
    parser.add_argument("--include-stress", action="store_true")
    parser.add_argument("--label", default="unlabelled-server")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.runs < 1 or args.warmup < 0 or not 100 <= args.timeout_ms <= 10000:
        parser.error("runs >= 1, warmup >= 0, timeout-ms in 100..10000 required")
    cases = [case for case in CASES if (args.include_stress or not case.get("stress"))
             and (not args.case or case["id"] in args.case)]
    if not cases or (args.case and set(args.case) - {case["id"] for case in cases}):
        parser.error("unknown case or stress case without --include-stress")
    opts = dict(MaxWallTime=args.timeout_ms * 1000000, MaxDocDisplayCount=200,
                MaxMatchDisplayCount=4000, NumContextLines=5,
                ShardMaxMatchCount=100000, TotalMaxMatchCount=1000000)
    # Explicit engine limits bypass /api/search's result-count heuristic and
    # match current MCP engine defaults. 4000 is GLOBAL ranges, not 20 snippets/file.
    report = {
        "timestamp_utc": datetime.now(timezone.utc).isoformat(), "label": args.label,
        "url": args.url, "repo": args.repo, "platform": platform.platform(),
        "processor": platform.processor(), "python": platform.python_version(),
        "transport": "serial HTTP JSON with connection reuse; excludes client JSON decode",
        "cache": "uncontrolled existing server; first sample is NOT a cold-cache claim",
        "completion": "legacy API has no definitive completion flag; zero signals do not prove completeness",
        "warmup": args.warmup, "runs": args.runs, "options": opts, "cases": [],
    }
    failed = False
    client = Client(args.url, args.timeout_ms / 1000 + 5)
    try:
        query = "r:" + json.dumps("^" + re.escape(args.repo) + "$")
        listing, _, _ = client.post("/api/list", {"Q": query})
        listing = listing["List"]
        entries = listing.get("Repos") or []
        if listing.get("Crashes") or not entries:
            raise RuntimeError("repository missing or index not ready")
        report["corpus"] = {
            "stats": listing["Stats"],
            "indexes": [{"repo": entry["Repository"]["Name"], "metadata": entry["IndexMetadata"]}
                        for entry in entries],
        }
        for case in cases:
            entry = {"case": case, "samples": []}
            report["cases"].append(entry)
            case_opts = dict(opts)
            if case.get("stress"):
                case_opts.update(MaxDocDisplayCount=20, MaxMatchDisplayCount=200, NumContextLines=0)
            entry["options"] = case_opts
            try:
                entry["first_observation"] = measure(client, args.repo, case, case_opts)
                if entry["first_observation"]["errors"]:
                    raise RuntimeError(str(entry["first_observation"]["errors"]))
                for _ in range(args.warmup):
                    warmup = measure(client, args.repo, case, case_opts)
                    if warmup["errors"]:
                        raise RuntimeError(str(warmup["errors"]))
                for _ in range(args.runs):
                    sample = measure(client, args.repo, case, case_opts)
                    entry["samples"].append(sample)
                    if sample["errors"]:
                        raise RuntimeError(str(sample["errors"]))
                samples = entry["samples"]
                entry["summary"] = {key: distribution([s[key] for s in samples])
                                    for key in ("engine_ms", "queue_ms", "http_ms", "response_bytes", "returned_files")}
                entry["partial_or_limit_signals"] = sorted({flag for s in samples for flag in s["partial_or_limit_signals"]})
                entry["stable_results"] = len({s["signature"] for s in samples}) == 1
                unexpected_signals = set(entry["partial_or_limit_signals"])
                if case.get("allow_display_limit"):
                    unexpected_signals -= {"files_omitted_from_response", "at_file_display_limit"}
                if not case.get("stress") and (unexpected_signals or not entry["stable_results"]):
                    raise RuntimeError("partial/limited or unstable result; not a passing normal baseline")
                engine = entry["summary"]["engine_ms"]
                http_summary = entry["summary"]["http_ms"]
                print(f"{case['id']:22} engine p50/p95={engine['p50']:.2f}/{engine['p95']:.2f} ms"
                      f"  HTTP p95={http_summary['p95']:.2f} ms  files={samples[-1]['returned_files']}"
                      f"  signals={entry['partial_or_limit_signals']}", flush=True)
            except (OSError, ValueError, KeyError, RuntimeError, http.client.HTTPException) as error:
                entry["error"] = str(error)
                print(f"{case['id']}: FAILED: {error}", file=sys.stderr, flush=True)
                failed = True
                client.close()
                client = Client(args.url, args.timeout_ms / 1000 + 5)
    except (OSError, ValueError, KeyError, RuntimeError, http.client.HTTPException) as error:
        report["error"] = str(error)
        print(f"FAILED: {error}", file=sys.stderr, flush=True)
        failed = True
    finally:
        client.close()
        report["passed"] = not failed and len(report["cases"]) == len(cases) and all(
            "summary" in entry for entry in report["cases"])
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        print(f"Report: {args.output.resolve()}", flush=True)
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
