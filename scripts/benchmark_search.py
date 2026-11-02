#!/usr/bin/env python3
"""Read-only Unreal interface benchmark; MCP is the default primary suite.

Python 3.10+, standard library. Never builds, indexes, launches or restarts a
server. Run against your server after you deploy and finish indexing Unreal.
"""
import argparse
import json
import math
import platform
import statistics
import sys
from datetime import datetime, timezone
from pathlib import Path
from http.client import HTTPException
from concurrent.futures import ThreadPoolExecutor

from search_benchmark_backends import Client, measure, request_for

HEADER = r"(^|/)RenderCore/Public/RenderGraphBuilder[.]h$"
MODULE = r"(^|/)RenderCore/.*[.](cpp|h)$"
CALL = r"\bRegisterExternal(Texture|Buffer)\s*\("
FILES = r"(^|/)RenderCore/.*(RenderGraphBuilder|RenderingThread)[.]h$"
CASES = [
    dict(id="files", query=f"file:{FILES} type:filename", pattern=FILES, mode="files",
         path=FILES, min_files=2),
    dict(id="literal_scoped", query=f"file:{HEADER} content:RegisterExternalTexture",
         pattern="RegisterExternalTexture", filter=HEADER, path=HEADER, evidence="RegisterExternalTexture"),
    dict(id="alternation_scoped", query=f"file:{HEADER} content:(Register|Queue)External(Texture|Buffer)",
         pattern="(Register|Queue)External(Texture|Buffer)", filter=HEADER, path=HEADER, evidence="RegisterExternal"),
    dict(id="calls_scoped", query=f"file:{MODULE} content:{CALL}",
         pattern=CALL, filter=MODULE, path=MODULE, evidence="RegisterExternal"),
    dict(id="calls_unscoped", query=f"content:{CALL}", pattern=CALL, allow_display_limit=True, evidence="RegisterExternal"),
    dict(id="multiline", query=f"file:{HEADER} content:(?s:RegisterExternalTexture\\(.{{0,250}}ERDGTextureFlags)",
         pattern=r"(?s:RegisterExternalTexture\(.{0,250}ERDGTextureFlags)", filter=HEADER, path=HEADER, evidence="RegisterExternalTexture|ERDGTextureFlags"),
    # File-level AND is intentionally a native query, including in the MCP suite.
    dict(id="file_and", query=f"file:{HEADER} content:RegisterExternalTexture content:ERDGTextureFlags", path=HEADER),
    dict(id="case_insensitive", query=f"file:{HEADER.lower()} content:registerexternal(texture|buffer)",
         pattern="registerexternal(texture|buffer)", filter=HEADER.lower(), path=HEADER, evidence="RegisterExternal"),
    dict(id="wrong_path_zero", query=r"file:(^|/)RenderCore/Public/RenderingThread[.]h$ content:RegisterExternal(Texture|Buffer)",
         pattern="RegisterExternal(Texture|Buffer)", filter=r"(^|/)RenderCore/Public/RenderingThread[.]h$", max_files=0, min_files=0),
    dict(id="weak_branch", query=r"content:(RegisterExternalTexture|xx.+)", pattern=r"(RegisterExternalTexture|xx.+)", stress=True, min_files=0),
]
TRANSPORTS = ("mcp", "mcp-raw", "mcp-legacy", "http-post", "http-get")


def distribution(values):
    values = [v for v in values if v is not None]
    if not values:
        return None
    ordered = sorted(values)
    return dict(min=min(values), p50=statistics.median(values),
                p95=ordered[math.ceil(len(values) * .95) - 1],
                max=max(values), mean=statistics.mean(values))


def compare_baseline(report, baseline):
    """Compare like-for-like requests. Missing or changed baselines are explicit."""
    previous = {(entry.get("transport", "http-post"), entry["case"]["id"]): entry
                for entry in baseline.get("cases", [])}
    comparisons = []
    for entry in report["cases"]:
        key = entry["transport"], entry["case"]["id"]
        old = previous.get(key)
        row = dict(transport=key[0], case=key[1], status="not_comparable")
        same_config = baseline.get("profile") == report.get("profile") and baseline.get("concurrency", 1) == report.get("concurrency", 1)
        if old and "summary" in entry and "summary" in old and old.get("request") == entry.get("request") and same_config:
            old_time = old["summary"].get("transport_ms")
            current_time = entry["summary"].get("transport_ms")
            if old_time and current_time:
                delta = current_time["p95"] - old_time["p95"]
                row.update(status="regression_candidate" if delta > 5 and current_time["p95"] > old_time["p95"] * 1.2 else "within_threshold",
                           delta_p95_ms=delta)
                if set(entry.get("partial_or_limit_signals", [])) != set(old.get("partial_or_limit_signals", [])):
                    row["status"] = "coverage_changed"
                if entry["samples"][-1]["signature"] != old["samples"][-1]["signature"]:
                    row["status"] = "results_changed"
        comparisons.append(row)
    return comparisons


def run_case(client, args, transport, case, report):
    options = dict(MaxWallTime=args.timeout_ms * 1000000, MaxDocDisplayCount=args.max_files,
                   MaxMatchDisplayCount=4000, NumContextLines=args.context_lines,
                   ShardMaxMatchCount=100000, TotalMaxMatchCount=1000000, snippets=args.max_snippets_per_file)
    if case.get("stress"):
        options.update(MaxDocDisplayCount=20, MaxMatchDisplayCount=200, NumContextLines=0)
    entry = dict(transport=transport, case=case, options=options,
                 request=list(request_for(transport, args.repo, case, args.profile, options)), samples=[])
    report["cases"].append(entry)
    first = measure(client, transport, args.repo, case, args.profile, options)
    entry["first_observation"] = first
    if first["errors"]:
        raise RuntimeError(str(first["errors"]))
    for _ in range(args.warmup):
        sample = measure(client, transport, args.repo, case, args.profile, options)
        if sample["errors"]:
            raise RuntimeError(str(sample["errors"]))
    def samples(connection, count):
        result = []
        for _ in range(count):
            sample = measure(connection, transport, args.repo, case, args.profile, options)
            result.append(sample)
            if sample["errors"]:
                raise RuntimeError(str(sample["errors"]))
        return result

    if args.concurrency == 1:
        entry["samples"] = samples(client, args.runs)
    else:
        connections = [client]
        try:
            for _ in range(args.concurrency - 1):
                connection = Client(args.url, max(30, args.timeout_ms / 1000 + 5))
                connections.append(connection)
                entry.setdefault("additional_client_setup", []).append(connection.initialize(transport))
                samples(connection, args.warmup)
            with ThreadPoolExecutor(max_workers=args.concurrency) as executor:
                jobs = [executor.submit(samples, connection, args.runs // args.concurrency + (i < args.runs % args.concurrency))
                        for i, connection in enumerate(connections)]
                for job in jobs:
                    entry["samples"].extend(job.result())
        finally:
            for connection in connections[1:]:
                connection.close()
    entry["summary"] = {key: distribution([s[key] for s in entry["samples"]])
                        for key in ("engine_ms", "queue_ms", "transport_ms", "decode_ms", "validation_ms", "response_bytes", "returned_files")}
    entry["partial_or_limit_signals"] = sorted({f for s in entry["samples"] for f in s["partial_or_limit_signals"]})
    entry["stable_results"] = len({s["signature"] for s in entry["samples"]}) == 1
    if not case.get("stress") and not entry["stable_results"]:
        raise RuntimeError("unstable evidence; cannot treat this as a passing baseline")
    latency = entry["summary"]["transport_ms"]
    engine = entry["summary"]["engine_ms"]
    engine_text = f"{engine['p95']:.2f}" if engine else "unavailable"
    print(f"{transport:10} {case['id']:22} wire p50/p95={latency['p50']:.2f}/{latency['p95']:.2f} ms"
          f" engine p95={engine_text} files={entry['samples'][-1]['returned_files']}"
          f" signals={entry['partial_or_limit_signals']}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:6070", help="Existing server base URL")
    parser.add_argument("--repo", default="ue")
    parser.add_argument("--transport", choices=(*TRANSPORTS, "all"), default="mcp")
    parser.add_argument("--profile", choices=("defaults", "aligned"), default="defaults",
                        help="defaults sends each interface's normal defaults; aligned sends supported explicit controls")
    parser.add_argument("--runs", type=int, default=30)
    parser.add_argument("--warmup", type=int, default=3)
    parser.add_argument("--concurrency", type=int, choices=(1, 4), default=1)
    parser.add_argument("--timeout-ms", type=int, default=3000)
    parser.add_argument("--max-files", type=int, default=200)
    parser.add_argument("--context-lines", type=int, default=5)
    parser.add_argument("--max-snippets-per-file", type=int, default=20)
    parser.add_argument("--case", action="append")
    parser.add_argument("--include-stress", action="store_true")
    parser.add_argument("--baseline", type=Path)
    parser.add_argument("--label", default="unlabelled-server")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.runs < args.concurrency or args.warmup < 0 or not 100 <= args.timeout_ms <= 10000:
        parser.error("runs >= concurrency, warmup >= 0, timeout-ms in 100..10000 required")
    if not 1 <= args.max_files <= 20000 or not 0 <= args.context_lines <= 50 or not 1 <= args.max_snippets_per_file <= 200:
        parser.error("invalid result/context limits")
    transports = ["mcp", "mcp-raw", "http-post", "http-get"] if args.transport == "all" else [args.transport]
    if args.profile == "aligned" and "http-get" in transports and args.context_lines > 10:
        parser.error("HTTP GET supports ctx <= 10; run MCP/POST separately for larger contexts")
    if args.profile == "defaults" and (args.timeout_ms, args.max_files, args.context_lines, args.max_snippets_per_file) != (3000, 200, 5, 20):
        parser.error("explicit result/deadline overrides require --profile aligned")
    cases = [c for c in CASES if (args.include_stress or not c.get("stress")) and (not args.case or c["id"] in args.case)]
    if not cases or (args.case and set(args.case) - {c["id"] for c in cases}):
        parser.error("unknown case or stress case without --include-stress")
    if args.baseline and args.baseline.resolve() == args.output.resolve():
        parser.error("baseline and output must be different files")
    report = dict(timestamp_utc=datetime.now(timezone.utc).isoformat(), label=args.label, url=args.url, repo=args.repo,
                  platform=platform.platform(), processor=platform.processor(), python=platform.python_version(),
                  profile=args.profile, runs=args.runs, warmup=args.warmup, concurrency=args.concurrency, cases=[],
                  timing="wire time excludes client JSON decode and validation; both are recorded separately",
                  cache="existing server, explicitly warmed; first observation is not a cold-cache claim",
                  completion="legacy MCP and HTTP may not expose completeness; zero signals do not prove completeness",
                  controls="raw MCP has fixed limits; GET has no engine deadline option. Actual request is recorded; unsupported controls are not invented.")
    failed = False
    try:
        for transport in transports:
            # One interface at a time, never parallel benchmark suites.
            client = Client(args.url, max(30, args.timeout_ms / 1000 + 5))
            try:
                report.setdefault("setup", []).append({"transport": transport, "requests": client.initialize(transport)})
                if "corpus" not in report:
                    try:
                        from search_benchmark_backends import native_query
                        query = native_query(args.repo, {"query": ""}).split(" case:no", 1)[0]
                        listing, _, _, _ = client.request("POST", "/api/list", {"Q": query})
                        listing = listing["List"]
                        report["corpus"] = {"stats": listing["Stats"], "indexes": [
                            {"repo": item["Repository"]["Name"], "metadata": item["IndexMetadata"]}
                            for item in listing.get("Repos") or []]}
                    except (OSError, ValueError, KeyError, RuntimeError, HTTPException) as error:
                        report["corpus"] = {"unavailable": str(error)}
                        # Metadata is optional on MCP-only servers. Reconnect
                        # after an error; none of this is part of search timing.
                        client.close()
                        client = Client(args.url, max(30, args.timeout_ms / 1000 + 5))
                        report["setup"].append({"transport": transport, "reconnect": True, "requests": client.initialize(transport)})
                for case in cases:
                    try:
                        run_case(client, args, transport, case, report)
                    except (OSError, ValueError, KeyError, RuntimeError, TypeError, HTTPException) as error:
                        failed = True
                        report["cases"][-1]["error"] = str(error)
                        print(f"{transport}/{case['id']}: FAILED: {error}", file=sys.stderr, flush=True)
                        # Do not reuse a potentially interrupted connection/session.
                        break
            except (OSError, ValueError, KeyError, RuntimeError, TypeError, HTTPException) as error:
                failed = True
                report.setdefault("errors", []).append(f"{transport}: {error}")
                print(f"{transport}: FAILED: {error}", file=sys.stderr, flush=True)
            finally:
                client.close()
        if args.baseline:
            baseline = json.loads(args.baseline.read_text(encoding="utf-8"))
            report["comparisons"] = compare_baseline(report, baseline)
            if any(r["status"] != "within_threshold" for r in report["comparisons"]):
                failed = True
    except (OSError, ValueError) as error:
        report["error"] = str(error)
        failed = True
    finally:
        expected = len(cases) * len(transports)
        report["passed"] = not failed and len(report["cases"]) == expected and all("summary" in c for c in report["cases"])
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        print(f"Report: {args.output.resolve()}", flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
