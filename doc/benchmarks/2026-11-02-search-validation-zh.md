# Zoekt 新搜索接口正确性与性能验收报告

测试日期：2026-11-02（UTC）。目标：本机 `http://127.0.0.1:6070`，仓库 `ue`。

## 结论

**本轮新增接口正确性测试通过，运行中服务的四接口正常套件全部通过。限定范围的复杂正则达到毫秒级；全库搜索仍需约 0.23–0.31 秒，弱选择性正则可耗尽 3 秒期限。不能承诺任意复杂正则都在几十毫秒内完成。**

- 6 个 Go 包的指定契约测试通过；9 项 Python 离线适配测试通过。
- 新 MCP、原生 MCP、HTTP POST、HTTP GET：9 用例 × 4 接口 × 100 个计时样本，共 3600 次正常计时请求，全部通过脚本断言。
- MCP 并发 4、扩大返回配置各完成 900 个计时请求；独立压力用例完成 3 个计时请求。预热、首次观察及初始化另外执行，不混入这些数量。
- 没有发现仓库/路径过滤泄漏、已知正向证据缺失或正常组结果签名不稳定。完整性结论受下文的接口状态能力和 oracle 范围限制。

## 环境与测量口径

- 系统：`Windows-11-10.0.26300-SP0`；Python `3.13.7`。服务启动参数、二进制精确提交和服务进程 CPU/内存峰值未采集。
- 索引报告：1 个仓库，17 个分片，168033 个文件，1752728867 内容字节（约 1.75 GB），46918632 个换行。这里只代表已索引部分。
- 每个正常用例先观察 1 次、额外预热 3 次，然后计时 100 次。串行套件顺序执行；并发套件使用 4 条独立连接，总计仍为每用例 100 个样本。
- 表格为发送请求至接收完整响应的延迟，单位 ms；客户端 JSON 解码和结果验证时间另存在 JSON 中。初始化、tools/list 不计入搜索延迟。
- 使用既有服务和热缓存，没有重启服务、重建索引或清理系统页缓存；不属于冷启动测试。服务由用户中途重新启动以开启接口，正式数据在其后重新采集。
- MCP 未公开引擎内部 Stats，报告不把 HTTP 的 Duration 当作 MCP 引擎耗时。各接口真实默认返回限制不同，表格不是严格等负载性能排名。

## 正确性验证

执行命令：

```powershell
$env:GOMAXPROCS='2'
go test -p 1 ./cmd/zoekt-webserver ./web ./internal/json ./index ./search ./gitindex -run '^(TestMCP|TestHTTP|TestAgent|TestClientServer|TestProgressNotEncoded)' -count=1
python -B -m unittest discover -s scripts -p test_benchmark_search.py
```

测试覆盖新 search 与 search_raw、HTTP 两种 JSON 契约、真实索引上的 regex/OR/仓库和路径过滤、默认与显式大小写、首尾行/空文件/CRLF/Unicode/零宽匹配、上下文合并、展示限制、期限、路径规范化及完成信号。Go 测试使用独立 fixture；本轮没有重跑全仓所有测试。

Unreal 在线验证检查已知正向证据、预期零结果、每个返回文件的仓库/路径约束和重复请求结果稳定性。它不是对 Unreal 全部源码进行独立穷举后的完整预期集合比对。raw MCP 和 HTTP 的完成状态记为 unknown；没有跳过信号不能证明全量扫描完整。

## 四接口默认参数性能

| 用例 | 新 MCP p50 / p95 | raw MCP p50 / p95 | POST p50 / p95 | GET p50 / p95 |
|---|---:|---:|---:|---:|
| 文件路径搜索 | 0.79 / 0.89 | 0.73 / 0.95 | 0.70 / 0.79 | 0.69 / 0.99 |
| 限定文件固定串 | 0.92 / 1.30 | 0.90 / 1.08 | 0.87 / 1.19 | 0.91 / 1.12 |
| 限定文件多分支正则 | 5.86 / 6.96 | 5.89 / 6.98 | 5.63 / 6.49 | 5.82 / 7.11 |
| 限定目录调用形态正则 | 19.11 / 21.82 | 18.76 / 21.35 | 18.72 / 21.03 | 18.71 / 21.32 |
| 全库调用形态正则 | 237.82 / 264.58 | 228.60 / 239.11 | 229.93 / 234.69 | 227.88 / 233.62 |
| 有界跨行正则 | 2.53 / 4.76 | 2.57 / 4.75 | 2.52 / 5.02 | 2.62 / 5.07 |
| 文件级 AND（原生查询） | 1.12 / 1.36 | 1.13 / 1.32 | 1.10 / 1.36 | 1.19 / 1.41 |
| 大小写不敏感 | 4.05 / 5.08 | 3.90 / 4.64 | 4.17 / 4.98 | 4.00 / 4.81 |
| 错误路径负向用例 | 1.94 / 2.44 | 1.92 / 2.21 | 2.03 / 2.94 | 1.94 / 2.34 |

文件级 AND 用例在“新 MCP 套件”中实际调用 search_raw；其余 8 个用例调用新 search。完整 pattern、path_regex、原生 query 和实际请求参数均保存在原始 JSON。

全库用例的返回规模：

| 接口 | 返回文件 | 响应字节中位数 | 状态 |
|---|---:|---:|---|
| mcp | 200 | 426248 | complete=true；file_limit, output_truncated |
| mcp-raw | 48 | 31990 | complete=unknown；files_omitted_from_response |
| http-post | 237 | 389926 | complete=unknown；未报告裁剪信号 |
| http-get | 50 | 133500 | complete=unknown；files_omitted_from_response |

新 MCP 的 search_complete=true 与 file_limit 可同时出现：引擎完成搜索，但展示只返回 200 个文件。raw 的固定命中展示预算使本次仅输出 48 个文件；GET 默认输出 50 个。不能仅比较延迟就断言哪种接口更高效。

## MCP 并发与更大返回

大返回配置：`max_files=20000`、`max_snippets_per_file=200`、`context_lines=20`、`timeout_ms=3000`。这是提高上限，不是合成了 20000 个实际命中文件；本组最多实际返回 237 文件，未验证最大值乘积的资源成本。

| 用例 | 并发 1 默认 p95 | 并发 4 默认 p95 | 大返回 p95 | 大返回文件数 | 大返回响应字节中位数 |
|---|---:|---:|---:|---:|---:|
| 文件路径搜索 | 0.89 | 2.50 | 0.96 | 2 | 536 |
| 限定文件固定串 | 1.30 | 2.96 | 1.14 | 1 | 3565 |
| 限定文件多分支正则 | 6.96 | 14.25 | 6.72 | 1 | 4415 |
| 限定目录调用形态正则 | 21.82 | 27.89 | 22.15 | 7 | 28555 |
| 全库调用形态正则 | 264.58 | 312.66 | 250.44 | 237 | 1238386 |
| 有界跨行正则 | 4.76 | 10.16 | 4.75 | 1 | 3846 |
| 文件级 AND（原生查询） | 1.36 | 3.14 | 1.37 | 1 | 730 |
| 大小写不敏感 | 5.08 | 8.91 | 4.85 | 1 | 4415 |
| 错误路径负向用例 | 2.44 | 4.60 | 2.27 | 0 | 136 |

两组均通过，正常结果签名稳定。大返回全库用例返回 237 文件，无裁剪信号；与默认 200 文件组返回量不同，不能作为同条件性能回归比较。文件级 AND 仍走固定参数的 search_raw。

## 弱选择性正则压力测试

表达式：`(RegisterExternalTexture|xx.+)`，无路径过滤；新 MCP 期限 3000 ms，20 文件上限、context_lines=0。

3 个计时样本：p50 3012.77 ms，最大 3074.27 ms；脚本计算的 p95 为 3074.27 ms，但 3 样本不足以估计稳定尾延迟。

实际信号：`file_limit, output_truncated, search_incomplete, timeout`。返回 0 个文件，search_complete=false；**不能把该零结果解释为没有匹配**。期限耗尽后没有剩余预算格式化源码片段，本次没有可读部分源码结果。

压力组“通过”仅表示允许的超时/部分结果已被明确呈现，并非搜索完成或性能达标。最长请求超过 3000 ms，体现协作式取消与结果处理开销，不能把 timeout_ms 当作绝对网络响应截止时间。未测量峰值内存，也未验证部署响应预算触顶场景。

## 部署问题与结论边界

- 初次运行时 /mcp 和 /api/search 返回 HTML 首页。用户确认忘记开启接口并重启服务；其后完整套件重新运行并通过。早期失败与中断数据保留，不纳入正式性能表格。
- 限定目录复杂正则：新 MCP p95 约 22 ms，满足设计中本机 100 ms 的建议目标。全库选择性查询约 265 ms；HTTP 对照的引擎 p95 约 231 ms，但不能据此推导 MCP 内部引擎时间。
- 本轮没有冻结条件一致的旧二进制对照，没有运行 --baseline，因此不声称相比旧版本无退化或有加速。当前数据可用作后续相同机器、语料、请求参数的基线。
- 没有操作 Vim 插件 UI；HTTP 字段和编码契约已测，插件端到端跳转与解析仍未验证。Linux CI、冷启动、并发超过 4、最大实际返回规模不在本轮实测范围内。

## 原始记录与复现

- [enabled-defaults](2026-11-02-enabled-defaults.json)
- [mcp-concurrency4](2026-11-02-mcp-concurrency4.json)
- [mcp-large](2026-11-02-mcp-large.json)
- [mcp-stress](2026-11-02-mcp-stress.json)
- [Go 正确性日志](2026-11-02-correctness.log)
- [Python 适配测试日志](2026-11-02-python.log)
- [接口未启用时的记录](2026-11-02-defaults.json)
- [重启中断的早期 GET 对照](2026-11-02-get-aligned.json)

```powershell
python -B scripts/benchmark_search.py --transport all --runs 100 --output doc/benchmarks/2026-11-02-enabled-defaults.json
python -B scripts/benchmark_search.py --transport mcp --concurrency 4 --runs 100 --output doc/benchmarks/2026-11-02-mcp-concurrency4.json
python -B scripts/benchmark_search.py --transport mcp --profile aligned --max-files 20000 --max-snippets-per-file 200 --context-lines 20 --runs 100 --output doc/benchmarks/2026-11-02-mcp-large.json
python -B scripts/benchmark_search.py --transport mcp --include-stress --case weak_branch --runs 3 --warmup 0 --output doc/benchmarks/2026-11-02-mcp-stress.json
```

再次执行请使用新的输出文件名，保留本轮基线。
