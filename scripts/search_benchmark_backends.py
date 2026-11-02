"""Transport adapters for the read-only search benchmark (standard library)."""

import base64
import hashlib
import http.client
import json
import re
import time
from urllib.parse import urlencode, urlsplit


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
        self.protocol = "2025-03-26"

    def request(self, method, path, body=None):
        payload = None if body is None else json.dumps(body).encode("utf-8")
        start = time.perf_counter_ns()
        self.connection.request(method, self.base_path + path, payload, {
            "Content-Type": "application/json", "MCP-Protocol-Version": self.protocol,
            "Accept": "application/json, text/event-stream",
        })
        response = self.connection.getresponse()
        raw = response.read()
        transport_ms = (time.perf_counter_ns() - start) / 1e6
        if response.status not in (200, 202):
            raise RuntimeError(f"HTTP {response.status}: {raw[:300]!r}")
        decode_start = time.perf_counter_ns()
        document = json.loads(raw) if raw else {}
        decode_ms = (time.perf_counter_ns() - decode_start) / 1e6
        if document.get("Error") or document.get("error"):
            raise RuntimeError(str(document.get("Error") or document["error"]))
        return document, transport_ms, len(raw), decode_ms

    def rpc(self, method, params):
        return self.request("POST", "/mcp", {"jsonrpc": "2.0", "id": 1, "method": method, "params": params})

    def initialize(self, transport):
        if not transport.startswith("mcp"):
            return []
        setup = []
        result, wire, size, decode = self.rpc("initialize", {
            "protocolVersion": self.protocol, "capabilities": {},
            "clientInfo": {"name": "zoekt-search-benchmark", "version": "1"},
        })
        setup.append(dict(method="initialize", transport_ms=wire, response_bytes=size, decode_ms=decode))
        self.protocol = result["result"]["protocolVersion"]
        _, wire, size, decode = self.request("POST", "/mcp", {"jsonrpc": "2.0", "method": "notifications/initialized"})
        setup.append(dict(method="notifications/initialized", transport_ms=wire, response_bytes=size, decode_ms=decode))
        result, wire, size, decode = self.rpc("tools/list", {})
        setup.append(dict(method="tools/list", transport_ms=wire, response_bytes=size, decode_ms=decode))
        schemas = {tool["name"]: tool["inputSchema"].get("properties", {}) for tool in result["result"]["tools"]}
        required = {"mcp": {"search": "pattern", "search_raw": "query"},
                    "mcp-raw": {"search_raw": "query"}, "mcp-legacy": {"search": "query"}}[transport]
        for tool, parameter in required.items():
            if parameter not in schemas.get(tool, {}):
                raise RuntimeError(f"{transport} requires {tool}({parameter}); no fallback is allowed")
        return setup

    def close(self):
        self.connection.close()


def native_query(repo, case):
    return "r:" + json.dumps("^" + re.escape(repo) + "$") + " case:no " + case["query"]


def request_for(transport, repo, case, profile, options):
    query = native_query(repo, case)
    if transport.startswith("mcp"):
        if transport == "mcp" and "pattern" in case:
            args = {"pattern": case["pattern"], "repos": [repo], "mode": case.get("mode", "snippets")}
            if case.get("filter"):
                args["path_regex"] = case["filter"]
            if profile == "aligned" or case.get("stress"):
                args.update(max_files=options["MaxDocDisplayCount"], timeout_ms=options["MaxWallTime"] // 1000000)
                if args["mode"] == "snippets":
                    args.update(context_lines=options["NumContextLines"], max_snippets_per_file=options["snippets"])
            return "search", args
        return ("search" if transport == "mcp-legacy" else "search_raw"), {"query": query}
    if transport == "http-post":
        body = {"Q": query}
        if profile == "aligned" or case.get("stress"):
            body["Opts"] = {key: value for key, value in options.items() if key != "snippets"}
        return "/api/search", body
    params = {"q": query, "format": "json"}
    if profile == "aligned" or case.get("stress"):
        params.update(num=options["MaxDocDisplayCount"], ctx=options["NumContextLines"])
    return "/search?" + urlencode(params), None


def decode_mcp(document, new_search):
    result = document["result"]
    if result.get("isError"):
        raise RuntimeError(str(result.get("content")))
    text = "\n".join(item["text"] for item in result["content"] if item.get("type") == "text")
    files, signals, stats = [], [], {}
    complete = "unknown"
    if new_search:
        status = re.match(r"search_complete=(true|false|unknown) output_truncated=(true|false) partial_reasons=([^\n]*)", text)
        if not status:
            raise ValueError("new MCP search returned no completion header")
        complete = status[1]
        signals = list(filter(None, status[3].split(",")))
        if status[2] == "true":
            signals.append("output_truncated")
        if complete == "false":
            signals.append("search_incomplete")
    else:
        counts = re.match(r"Found (\d+) matches in (\d+) files", text)
        if counts:
            stats = {"MatchCount": int(counts[1]), "FileCount": int(counts[2])}
        elif not text.startswith("No results found for query:"):
            raise ValueError("unrecognized raw MCP response")
    for line in text.splitlines():
        if new_search:
            match = re.match(r'^repo=("(?:\\.|[^"\\])*") path=("(?:\\.|[^"\\])*")$', line)
            if match:
                files.append({"repo": json.loads(match[1]), "path": json.loads(match[2]), "hits": []})
            hit = re.match(r"^(\d+)>\| (.*)$", line)
        else:
            match = re.match(r"^File: (.*) \(Repo: (.*)\)$", line)
            if match:
                files.append({"repo": match[2], "path": match[1], "hits": []})
            hit = re.match(r"^(\d+): (.*)$", line)
        if hit and files:
            files[-1]["hits"].append((int(hit[1]), hit[2]))
    if not new_search and stats.get("FileCount", 0) > len(files):
        signals.append("files_omitted_from_response")
    return files, stats, signals, complete


def decode_http(document, transport):
    files = []
    if transport == "http-post":
        result = document["Result"]
        stats = {key: value for key, value in result.items() if key != "Files"}
        for file in result.get("Files") or []:
            hits = [(m["LineNumber"], base64.b64decode(m["Line"] or "").decode("utf-8", errors="replace").rstrip("\r\n"))
                    for m in file.get("LineMatches") or []]
            files.append({"repo": file["Repository"], "path": file["FileName"], "hits": hits})
    else:
        result = document["result"]
        stats = result["Stats"]
        for file in result.get("FileMatches") or []:
            hits = [(m["LineNum"], "".join(f["Pre"] + f["Match"] + f["Post"] for f in m["Fragments"]).rstrip("\r\n"))
                    for m in file.get("Matches") or []]
            files.append({"repo": file["Repo"], "path": file["FileName"], "hits": hits})
    signals = [key for key in ("FilesSkipped", "ShardsSkipped", "Crashes") if stats.get(key)]
    if stats.get("FileCount", 0) > len(files):
        signals.append("files_omitted_from_response")
    return files, stats, signals, "unknown"


def measure(client, transport, repo, case, profile, options):
    target, arguments = request_for(transport, repo, case, profile, options)
    if transport.startswith("mcp"):
        document, wire_ms, size, decode_ms = client.rpc("tools/call", {"name": target, "arguments": arguments})
    else:
        document, wire_ms, size, decode_ms = client.request("POST" if transport == "http-post" else "GET", target, arguments)
    validate_start = time.perf_counter_ns()
    if transport.startswith("mcp"):
        decoded = decode_mcp(document, transport == "mcp" and target == "search")
    else:
        decoded = decode_http(document, transport)
    files, stats, signals, complete = decoded
    errors = []
    if len(files) < case.get("min_files", 1) or len(files) > case.get("max_files", 20000):
        errors.append("unexpected file count")
    for file in files:
        if file["repo"] != repo:
            errors.append("repository filter leaked")
        if case.get("path") and not re.search(case["path"], file["path"].replace("\\", "/"), re.IGNORECASE):
            errors.append("path filter leaked")
        if case.get("evidence") and not any(re.search(case["evidence"], line, re.IGNORECASE) for _, line in file["hits"]):
            errors.append("expected source evidence missing")
    signature = hashlib.sha256(json.dumps(sorted((f["repo"], f["path"], sorted(f["hits"])) for f in files)).encode()).hexdigest()
    if stats.get("Crashes"):
        errors.append("shard crash or index not ready")
    # A legacy API's lack of signals is not proof of completion.
    allowed = {"file_limit", "files_omitted_from_response", "output_truncated"} if case.get("allow_display_limit") else set()
    if not case.get("stress") and set(signals) - allowed:
        errors.append("unexpected partial result or limit")
    return {
        "engine_ms": stats["Duration"] / 1e6 if "Duration" in stats else None,
        "queue_ms": stats["Wait"] / 1e6 if "Wait" in stats else None,
        "transport_ms": wire_ms, "decode_ms": decode_ms,
        "validation_ms": (time.perf_counter_ns() - validate_start) / 1e6,
        "response_bytes": size, "returned_files": len(files), "search_complete": complete,
        "counters": {k: v for k, v in stats.items() if isinstance(v, (int, float))},
        "partial_or_limit_signals": sorted(set(signals)), "signature": signature,
        "errors": sorted(set(errors)),
    }
