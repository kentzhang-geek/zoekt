# UE 索引提速：前 3 项实施与验收

日期：2026-11-02 UTC。基于 `9a66d70d` 的本地修改。

**已实施诊断、并行输入和计算热点优化。没有实施分片内部并行合并算法；该项仍需用户明确确认。** 用户要求的 10 分钟上限，在本次全量 UE 同盘测试中达到；历史 22 分钟的慢运行仍未复现，不能把当前结果宣传成同条件的“22 分钟降到 52 秒”。

## 实际改动

- 将文件遍历从 Walk 换成 WalkDir，在请求详细文件信息之前做目录和扩展名过滤。
- 新增有界输入流水线：并行读取、DocChecker 检查和语言识别；每个 worker 有独立 checker，按原始遍历顺序提交给唯一的 Builder 协调者。默认读取 worker 数是 `min(GOMAXPROCS, parallelism)`，本机为 20。
- 同时限制在途文件数量和预估内容字节，默认预留预算 64 MiB。允许的超大文件独占预算，但其实际内容可能超过预算；文件读取期间增长也可能超出预估。分片构建自身内存不包含在此预算内。
- 保留原来的 100 MiB 分片阈值、文档顺序、语言识别规则和磁盘格式。本次仍为 17 个分片，没有用增加最终分片数量来换取吞吐。
- 将倒排表的两个 map 改为 `ngram → ID` 加连续 posting 状态数组。常见 trigram 每次只查询一次 map，随后直接更新数组中的压缩列表和上次位置；不引入共享 map 锁，不改变位置编码。
- 新增 PreparedDocument API，允许 worker 并行预处理，再由单一协调者提交，避免内容检查重复执行。
- 新增 Abort：输入出错时，等待活动构建并清理临时分片，保留旧索引。遍历和读取错误返回调用者，不再从遍历 goroutine 中 log.Fatal。流水线返回前会退出并等待其全部 goroutine。
- 启动日志记录 NumCPU、GOMAXPROCS、读/分片并行度、分片大小及符号索引状态；增加每 30 秒输入进度、输入阶段统计和每分片的符号/排序/倒排表/写入耗时。
- 修复 `-cpu_profile ... update UE` 没有应用到命名配置的问题；命令行 profile 优先于配置内 profile。命名配置现在也应用 automaxprocs 的运行时限制。
- 读取配置时复制公共 Options，避免多配置运行时前一个配置的选项修改污染后续配置。

入口与实现见 `cmd/zoekt-index/pipeline.go`、`index/builder.go`、`index/shard_builder.go` 和 `index/write.go`。

## 全量 UE 同盘结果

配置来自现有 `UE.json`，未修改过滤规则或用户配置。CPU 为 i9-10900K（10 核/20 线程），所有测试显式使用 `GOMAXPROCS=20`，分片并行度保持 64，输出均为 C 盘上的独立测试目录。168,033 个文件，17 个分片，无 ctags。

- 未修改源码第一次基线：**79.67 秒**，375.02 CPU 秒，平均 4.71 个等效逻辑核心。输出为沙箱 C 盘临时目录，后续工具会话未能再次读取该临时输出，仅保留运行日志和计时；正确性比较使用后面的持久测试目录。
- 优化版本：**52.11 秒**，387.95 CPU 秒，平均 7.44 个等效逻辑核心；峰值工作集约 **3.10 GiB**，采样峰值 private bytes 约 **3.11 GiB**。此轮开启 CPU profile。
- 随后在与优化版相同父目录下重跑未修改源码：**83.00 秒**，399.55 CPU 秒，平均 4.81 个等效逻辑核心；峰值工作集约 **2.45 GiB**。

最后两轮构成保留输出供正确性检查的对照：约 **1.59 倍**吞吐提升，墙钟时间减少约 **37%**。排序是优化版先、基线后，避免把优化版永远放在最后的缓存优势当成全部收益。

没有清空操作系统缓存，也没有重现历史负载；这些是普通缓存条件下的单次测量，不是冷启动保证或多轮分位数。CPU 时间未随墙钟同比下降，不能声称按核心数线性加速，更不能把 CPU 采样中的系统调用时间等同于纯磁盘等待。

优化版日志显示输入阶段为 40.41 秒，读取累计工作时间 120.60 秒、预处理累计工作时间 123.35 秒、提交累计时间 72 毫秒。累计工作时间会跨 worker 重叠，不能与墙钟时间直接相加。分片构建与输入阶段也互相重叠。

原始结果在 [indexing-implementation-2026-11-02](indexing-implementation-2026-11-02/)。`baseline-c-repeat.json`、`optimized-c.json` 是对照计时，`optimized-c.pprof` 和 `profile.txt` 是优化后 profile。

## 正确性与回归

完整读取基线和优化版的全部文件，文件名、语言、全文的组合 SHA-256 均为：

```text
3e6cd1cbf2656093ee800ec8ba9558b5b45dd5a177ec3673df1abe94402b7ada
```

两边均为 168,033 个文件，无重复文件名，也没有读取验证中的跳过或崩溃。该摘要与前次研究中的全量结果相同。

另外逐分片比较了 7 个有实际命中的查询，覆盖文件路径、普通文本、显式大小写、正则、OR 和文件级 AND。基线与优化版的全部 FileMatch JSON 一致，包括返回顺序、评分、行内容和匹配位置。对应文件数为 1192、903、903、311、817、41、192；详见 `query-verification.jsonl`。这不是对所有可能查询的穷举证明。

新增测试覆盖：

- 多个读取确实重叠，后一个文件先准备好时仍保持提交顺序。
- 文件扩展名和忽略目录过滤。
- 读取、提交、遍历错误与上下文取消，返回后无仍在运行的读取 worker。
- 输入失败/Abort 保留旧索引并删除临时分片。
- 并行预处理与普通 Add 的多个分片逐字节一致，包含 Unicode、空文件、跳过文件、大小限制、显式允许大文件和语言设置。
- 位置列表与独立的未压缩 rune 窗口 oracle 一致，包含无效 UTF-8、跨文档边界和多字节 varint。
- 既有 v16 字节级兼容测试及相关索引、语言、搜索、MCP、HTTP/gRPC 测试。

已执行仓库规定的 `go build ./cmd/...` 和 `go test ./... -short`；**全仓检查没有全部通过**，主要限制为 Windows 下的 Unix 命令/信号、路径假设和沙箱回环网络限制。解除测试的回环限制后，受影响的索引、语言、搜索、MCP、HTTP/gRPC 回归完成。

以下失败已分别在未修改的源码上复现，未混入本次性能修改：

- `TestIgnoreSizeMax` 的反斜杠转义模式，以及 `TestOptions_shardName`、`TestShardName` 的 Unix 路径期望。
- `web.TestTruncateLine` 返回约 20 KB，而测试要求不超过 10 KB。

索引回归排除前 3 个已验证旧测试，web 回归排除已验证的 TestTruncateLine，其他选定回归通过。完整日志保留在结果目录。`go vet` 还报告原有测试中未命名字段字面量及 Windows mmap 的 unsafe.Pointer 警告。此环境没有可用 CGO 编译器，`-race` 未能运行；不能声称已经通过 race detector。

## 使用

已更新工作区中的 `cmd/zoekt-index/zoekt-index.exe`。保留了修改前的 exe 为 `cmd/zoekt-index/zoekt-index-before-speedup.exe`。在线索引、webserver 和用户配置未改动。

在仓库根目录运行原来的命令即可使用新流水线：

```powershell
.\cmd\zoekt-index\zoekt-index.exe update UE
```

如需留下慢运行证据：

```powershell
.\cmd\zoekt-index\zoekt-index.exe -cpu_profile ue-index.pprof update UE
```

新增 `-read_parallelism` / JSON `read_parallelism` 可以显式控制预处理 worker 数；默认自动设置，当前 UE 配置无需修改。参数需放在 `update` 前。不要把配置中的 `parallelism: 64` 理解为实际有 64 个 CPU 核心。

目前的实现达到本次测试的时间要求，因此没有进入需要额外确认的分片内部并行合并阶段。
