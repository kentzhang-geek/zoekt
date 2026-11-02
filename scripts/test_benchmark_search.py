"""Offline checks for benchmark adapters; never connects to a running server."""
import base64
import contextlib
import io
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

import benchmark_search as runner
from search_benchmark_backends import Client, decode_http, decode_mcp, measure, request_for


class AdapterTests(unittest.TestCase):
    def setUp(self):
        self.options = dict(MaxWallTime=3000000000, MaxDocDisplayCount=200,
                            MaxMatchDisplayCount=4000, NumContextLines=5, snippets=20)

    def test_new_mcp_files_and_raw_file_and(self):
        tool, args = request_for("mcp", "ue", runner.CASES[0], "aligned", self.options)
        self.assertEqual(tool, "search")
        self.assertEqual(args["mode"], "files")
        self.assertNotIn("context_lines", args)
        tool, args = request_for("mcp", "ue", runner.CASES[6], "defaults", self.options)
        self.assertEqual(tool, "search_raw")
        self.assertIn("content:ERDGTextureFlags", args["query"])

    def test_http_defaults_are_not_overridden(self):
        _, body = request_for("http-post", "ue", runner.CASES[1], "defaults", self.options)
        self.assertNotIn("Opts", body)
        _, body = request_for("http-post", "ue", runner.CASES[1], "aligned", self.options)
        self.assertEqual(body["Opts"]["NumContextLines"], 5)
        self.assertNotIn("snippets", body["Opts"])

    def test_url_encoding(self):
        query = r'content:"a\\+ & # 中文"'
        target, _ = request_for("http-get", "ue", dict(query=query), "defaults", self.options)
        self.assertTrue(parse_qs(urlsplit(target).query)["q"][0].endswith(query))

    def test_mcp_coverage_and_evidence(self):
        text = 'search_complete=false output_truncated=true partial_reasons=timeout\n\nrepo="ue" path="Source/file.cpp"\nlines=1-2\n1>| Hit()\n2 | context\n'
        files, stats, signals, complete = decode_mcp({"result": {"content": [{"type": "text", "text": text}]}}, True)
        self.assertEqual(files[0]["hits"], [(1, "Hit()")])
        self.assertEqual(complete, "false")
        self.assertIn("search_incomplete", signals)
        self.assertNotIn("Duration", stats)
        with self.assertRaises(RuntimeError):
            decode_mcp({"result": {"isError": True, "content": []}}, True)
        with self.assertRaises(ValueError):
            decode_mcp({"result": {"content": [{"type": "text", "text": "No results"}]}}, True)

    def test_legacy_is_unknown(self):
        files, _, signals, complete = decode_mcp({"result": {"content": [{"type": "text", "text": "Found 3 matches in 2 files\n\nFile: D:\\src\\a.cpp (Repo: ue)\n1: Hit()\n"}]}}, False)
        self.assertEqual(complete, "unknown")
        self.assertIn("files_omitted_from_response", signals)
        self.assertEqual(files[0]["hits"], [(1, "Hit()")])

    def test_post_base64(self):
        line = base64.b64encode("中文()\r\n".encode()).decode()
        files, _, _, complete = decode_http({"Result": {"Files": [{"Repository": "ue", "FileName": "x", "LineMatches": [{"LineNumber": 7, "Line": line}]}]}}, "http-post")
        self.assertEqual(files[0]["hits"], [(7, "中文()")])
        self.assertEqual(complete, "unknown")

    def test_missing_new_schema_never_falls_back(self):
        class OldServer(Client):
            def __init__(self):
                self.protocol = "2025-03-26"
            def request(self, *args, **kwargs):
                return {}, 0, 0, 0
            def rpc(self, method, params):
                if method == "initialize":
                    return {"result": {"protocolVersion": self.protocol}}, 0, 0, 0
                return {"result": {"tools": [{"name": "search", "inputSchema": {"properties": {"query": {}}}}]}}, 0, 0, 0
        with self.assertRaises(RuntimeError):
            OldServer().initialize("mcp")

    def test_regression_rejects_changed_requests(self):
        entry = dict(transport="mcp", case={"id": "sample"}, request=["search", {"pattern": "foo"}],
                     summary={"transport_ms": {"p95": 10}}, samples=[{"signature": "a"}], partial_or_limit_signals=[])
        baseline = dict(profile="defaults", cases=[entry])
        changed = dict(entry, summary={"transport_ms": {"p95": 20}})
        report = dict(profile="defaults", cases=[changed])
        self.assertEqual(runner.compare_baseline(report, baseline)[0]["status"], "regression_candidate")
        changed["request"] = ["search", {"pattern": "bar"}]
        self.assertEqual(runner.compare_baseline(report, baseline)[0]["status"], "not_comparable")

    def test_concurrent_samples_use_independent_clients(self):
        clients = []

        class FakeClient:
            def __init__(self, *args):
                self.calls = 0
                self.closed = False
                clients.append(self)

            def initialize(self, transport):
                self.initialized = transport
                return []

            def close(self):
                self.closed = True

        def sample(client, *args):
            client.calls += 1
            return dict(errors=[], engine_ms=None, queue_ms=None, transport_ms=1,
                        decode_ms=0, validation_ms=0, response_bytes=123, returned_files=1,
                        partial_or_limit_signals=[], signature="stable")

        args = SimpleNamespace(timeout_ms=3000, max_files=200, context_lines=5,
                               max_snippets_per_file=20, profile="defaults", repo="ue",
                               warmup=1, runs=9, concurrency=4, url="http://unused.invalid")
        client = FakeClient()
        report = {"cases": []}
        with patch.object(runner, "Client", FakeClient), patch.object(runner, "measure", sample), contextlib.redirect_stdout(io.StringIO()):
            runner.run_case(client, args, "mcp", runner.CASES[1], report)
        self.assertEqual(len(report["cases"][0]["samples"]), 9)
        self.assertEqual([c.calls for c in clients], [5, 3, 3, 3])
        self.assertTrue(all(c.closed for c in clients[1:]))
        self.assertFalse(client.closed)
        self.assertTrue(all(c.initialized == "mcp" for c in clients[1:]))


if __name__ == "__main__":
    unittest.main()
