# UE 全量索引性能调查与并行改造方案

调查日期：2026-11-02 UTC。源代码：`9a66d70d`。本次没有修改生产索引器、用户配置或在线索引；新增的是诊断原型和调查材料。

**结论：20 多分钟值得排查，但并不是这份代码在这台 PC 上的必然耗时。现有多核支持真实存在，粒度却是分片；输入端仍然串行。完整并行流水线可以改善利用率，但要获得大幅加速，还必须降低倒排表构建和语言识别的单位工作量。不能把 worker 数提高到 64 就当成算法已经充分并行。**

## 本机和历史索引证据

- CPU：i9-10900K，10 个物理核心、20 个逻辑处理器；内存约 63.9 GiB，开始调查时约 33 GiB 可用。C、D 所在磁盘均报告为 NVMe。
- `C:\Users\64223\.zoekt\UE.json` 的 `Parallelism` 是 64。Go JSON 解码接受这个大小写写法，诊断程序实际解码值也是 64。
- 配置包含 Source、Shaders、Extras、Programs、Plugins 五个根目录。168,033 个符合扩展名的普通文件，共 1,924,256,751 原始文件字节；其中 24 个文件超过默认 2 MiB 文件上限。不能把这些原始字节全部视为已索引内容。
- 在线 UE 索引有 17 个分片，既有搜索报告统计内容约 1.75 GB。索引元数据 `HasSymbols=false`，所以这次历史运行没有建立 ctags 符号索引，ctags 不是这次慢的解释。
- 历史索引元数据开始时间为 **02:37:53 UTC**，最后一个分片最后写入时间为 **03:00:23 UTC**，相差约 **22 分 30 秒**。这是元数据与文件时间戳的差，不是采集到的历史进程端到端计时。
- 当前诊断进程报告 `NumCPU=20, GOMAXPROCS=20`，用户级和机器级持久环境变量未设置 GOMAXPROCS。无法据此还原历史启动 shell 的临时环境、CPU affinity、当时负载或 I/O 状态。
- 现有 exe 的构建信息为 Go 1.25.5、`c63e7435` 加未提交修改；不能假定它与当前提交逐字节对应。因此也单独运行了现有 exe。

## 实测结果及其边界

原始数据在 [indexing-2026-11-02](indexing-2026-11-02/)，可运行的 Windows 诊断程序在 [main_windows.go](../../scripts/indexing-probe/main_windows.go)。

1. 全量目录枚举，文件数量和原始字节相同：`filepath.Walk` **39.12 秒**；`WalkDir` **5.61 秒**，这次观察约 7 倍。先运行 Walk，后运行 WalkDir；没有清空系统缓存。后续独立 WalkDir 扫描为 5.72 和 6.15 秒。优化方向也与 [Go 官方文档](https://pkg.go.dev/path/filepath#Walk) 一致：WalkDir 避免逐项调用 Lstat。需要文件大小时仍会查询 DirEntry.Info，不能宣称完全不需要元数据查询。
2. 全量读取＋DocChecker 检查，不构建倒排表：1 worker **56.14 秒**，4 workers **15.30 秒**，20 workers **7.72 秒**。单 worker 到 20 workers 的阶段加速约 **7.27 倍**。每个 worker 有独立 DocChecker。跳过超过默认大小限制的文件内容，与本配置的正常读取行为一致。
3. 当前源码 Builder 的全量对照，先做文件清单、再计时构建，且关闭 ctags：现有串行读取＋检查、100 MiB 分片、并发上限 20，**74.29 秒**，353.31 CPU 秒，平均 **4.76 个等效逻辑核心**。
4. 同批文件的并行原型：按原始文件大小形成约 16 MiB 任务，106 个任务由 20 workers 动态领取；每个任务独立读取、检查、排序、构建倒排表并写分片。**58.17 秒**，558.08 CPU 秒，平均 **9.59 个等效逻辑核心**。加上本轮公共扫描的 6.15 秒，约 **64.3 秒**。相对于上一项构建阶段，只快 **1.28 倍**，并没有达到十倍。
5. 直接执行用户已有 exe，使用原来的 UE 配置、parallelism=64、默认 100 MiB 分片，只把输出重定向到独立 D 盘测试目录：**132.64 秒**，成功生成 17 个分片。此项包含原有遍历、读取、构建及命令启动开销。

等效逻辑核心数的计算是 Windows `GetProcessTimes` 的进程用户＋内核 CPU 秒除以墙钟秒。它不是采样时有多少个线程，也不是物理核心数。本机 20 个逻辑处理器对应 10 个物理核心，不能据此期待 20 倍线性加速。

这几组数据不能混成一个“从 22 分钟到 58 秒”的算法加速比：历史索引写 C 盘，实验写 D 盘；大量文件已被前面的扫描和读取预热；没有控制历史系统负载、杀毒扫描、冷缓存等条件。CLI 检查期间还做了轻量诊断分析，因此它是重现检查而非严格无干扰的基准。原型在现有 Builder 对照之后执行，也受缓存顺序影响；各组不是多轮统计结果。

**当前无法复现历史 22 分半，也无法认定具体是磁盘、杀毒软件或 GOMAXPROCS 导致。** 下一次慢运行应记录启动参数、GOMAXPROCS、进程 affinity、各阶段墙钟时间、文件打开延迟、工作队列深度、活跃 workers、CPU/heap/I/O，而不是直接关闭防护或继续盲加线程。

### 输出检查

通过 Zoekt 自己的读取和搜索 API，遍历现有 Builder、并行原型、真实 CLI 三套结果；每套均为 **168,033 个文件**，无重复文件名，检查中没有文件/分片跳过或崩溃。

对文件名、语言、完整内容 SHA-256 组成的记录排序后再计算摘要，三套均为：

```text
3e6cd1cbf2656093ee800ec8ba9558b5b45dd5a177ec3673df1abe94402b7ada
```

此检查验证了这些字段和可读取性，**不等同于所有查询、排序和截断语义完全相同**。分片变小会改变分片内排序及查询调度，生产改造还需要无限制匹配集合、排名、符号、UTF-8、取消和失败发布的验收。

## 代码中的并行边界

- `index/builder.go:66`：Parallelism 明确是“并行建立分片的最大数量”。默认分片源数据上限为 100 MiB。
- `cmd/zoekt-index/watch.go:605`：配置里的五个根目录按顺序添加。
- `cmd/zoekt-index/watch.go:735`：只有一个 Walk goroutine；缓冲通道只有 100 个文件描述。主消费循环逐文件执行 ReadFile，再调用 AddFile。
- `index/builder.go:596`：Builder.Add 在进入分片队列前同步做 DocChecker.Check，更新共享 todo、size 和复用的 checker。不能简单把这个调用放进多个 goroutine，它不是并发安全 API。
- `index/builder.go:806`：累计足够数据后 flush 才启动分片构建 goroutine。因此有并发名额，不代表当前有足够任务。
- `index/builder.go:948`：单个分片内部依次做符号处理、排序、ShardBuilder.Add，再写出。
- `index/shard_builder.go:90`：倒排表按字符串行处理；每个 trigram 查询 lastOffsets、读取/更新 postings，再更新 lastOffsets，形成热点哈希操作。
- `languages/languages.go:86`：有歧义的扩展名可能进入内容启发式/分类器。UE 大量头文件使这条路径值得专门优化。
- `cmd/zoekt-index/watch.go:260`：watch 的变化会重新调用整个配置的索引流程。当前本地目录 update/watch 没有接入已有的增量索引机制。

全量 profile 按阶段标签筛选后，现有构建阶段约 **52% 的 CPU 采样**归属于 newSearchableString，约 **18%**归属于语言识别。它们是累计采样占比，不是墙钟时间占比；详情见 profile-existing.txt。并行原型仍有约 49% 和 19% 落在这两类计算。原型的总 CPU 工作量还增加了，说明减少串行等待之外，必须检查更小分片带来的重复工作、缓存/内存竞争和分配成本，不能只追求任务管理器满载。

## 建议的改造顺序

### 第一阶段：全流程有界并行

```text
WalkDir / 有界目录任务
          ↓
稳定文件清单与任务编号（尽早产出任务，不等完整清单）
          ↓
全局工作队列 + 按字节计的内存预算
          ↓
10～20 个 worker 动态领取任务
  每个任务：读取 → 检查 → 语言/符号 → 排序 → 倒排表 → 临时文件
          ↓
统一协调成功发布与旧分片清理
```

- 先用 WalkDir，按目录工作队列做有界并行枚举。不要对所有目录/文件无上限创建 goroutine；错误和取消必须能让生产者、消费者一起退出。
- 读取、DocChecker、语言分类和符号解析应有独立 worker 状态。将粗大的 100 MiB 任务切成可调的 16～32 MiB 工作块，数量至少达到 worker 数的数倍，降低启动与尾部空闲。16 MiB 是本次原型参数，不是已确定的最优值。
- 全局并行度按本机 10/16/20 等候选值测量；单独限制 I/O 并发和内存中的原始字节，避免 64 个大分片同时膨胀。分片内外并行共用一个 CPU 配额，不能嵌套成 20×20。
- worker 独占 DocChecker、postings、offset 和解析器，避免给共享 Builder 加一把大锁又退化成串行。
- 分片编号和发布由一个 coordinator 管理。**不要让多个 Builder 指向同一仓库、同一输出目录独立 Finish**：它们会清理彼此的分片。原型为每个任务使用独立目录，只证明计算路径可以并行，没有实现在线仓库的发布协议。
- 保留先生成临时文件、全部成功后再发布的原则。现有多文件 rename 不是整库原子事务；若要求宕机时也保证整代一致，需增加 generation manifest 或对应协议。

这一阶段可解决输入供给问题，但本次实验已经证明：仅实施这一层，不应承诺数量级提升。

### 第二阶段：减少倒排表和分类的计算成本

1. **倒排表数据布局**：尝试用 `ngram → entryID` 的一次查询，索引连续数组里的 `{lastOffset, postingBuffer}`，替换两个 map 上的反复读写。对 ASCII 路径可评估直接查表，保留 Unicode fallback。128³ 个 ASCII trigram 的 uint32 表本身为 8 MiB/worker，必须纳入内存预算，不能无条件分配多份大型结构。
2. **语言识别**：添加可选的、按路径范围生效的语言覆盖与缓存。例如经用户选择，UE 自有 C++ 目录的 `.h` 可直接指定 C++；不能把 ThirdParty 所有 `.h` 全改成 C++，否则改变语言过滤和解析行为。无覆盖时保留完整启发式。
3. **重复分类**：rank 和 DetermineFileCategory 都会询问 generated/vendor/test 等信息，可将这些布尔分类计算一次后复用。单一 Category 不能完整代替所有排名标记，需保留现有优先级语义。
4. 比较单位源字节的 CPU 时间、分配字节和总墙钟时间。实现后再做完整语义核对，不能凭 CPU 占用更高就判定优化成功。

### 第三阶段：同一分片内部真正并行的倒排表算法

如果希望保留约 100 MiB 的最终分片、避免上百个小分片增加搜索开销，可以把“工作块大小”和“最终分片大小”分开，做以下算法：

1. 对一个最终分片中的文件并行完成检查、语言识别和符号处理，随后按现有规则确定文档顺序。
2. 将排好序的文档切成**连续**的小块。每个 worker 独立生成局部 trigram 字典、压缩位置列表、文档长度、rune/byte 映射和符号信息，不共享可变 map。
3. 对各块的 rune 长度、byte 长度做前缀和，求出各块在最终分片中的全局起点。这一步只处理少量块描述，不扫描每个字符。
4. 将 trigram 字典按 key 范围分给合并 workers。对每个 trigram 按块顺序拼接列表：局部首位置加块的全局 rune 起点，再减去前一块末位置，重编码这个边界差值；块内部的后续 delta 编码可以直接复制。缺席该 trigram 的块跳过。元数据和 rune/byte 映射相应加偏移。
5. 计算最终段长度与输出偏移，按索引格式写入临时文件，统一发布。

这样大部分文本处理和列表合并都可由多个 CPU 并行执行，最终仍维持原有大分片数量。需要显式处理 UTF-8 边界、空/跳过文件、跨块偏移、文件名 postings、符号元数据和 uint32 上限。

**现有 `index.Merge` 不是上述廉价合并**：它经 addDocument 重新调用 ShardBuilder.Add，会重新构造倒排表。把它直接放在小分片之后，会重复昂贵计算。要实现上面的算法，需要新的局部索引表示和压缩列表合并路径。这部分目前是设计，尚未实现或测量。

### 高频更新：真正的增量

对 update/watch 的“改几个文件”场景，内容哈希/文件清单缓存、变化文件 delta、删除 tombstone 和后台压实，往往比进一步优化全量构建更有潜力。已有 Builder.IsDelta、MarkFileAsChangedOrRemoved 和 gitindex 的使用可以参考，但目录入口没有自动接入；要处理删除、重命名、配置/版本失效、断电恢复和累计小分片。它不能替代首次全量索引的优化。

## 重现

在仓库根目录使用 Go 1.25.5 / Windows。以下 out 必须是不存在的新目录；程序会拒绝覆盖已有目录。build 模式仅做无 ctags 的研究对照，不会把任务分片安装进在线服务。

```powershell
go build -o indexing-probe.exe ./scripts/indexing-probe
./indexing-probe.exe -mode scan -config C:\Users\64223\.zoekt\UE.json
./indexing-probe.exe -mode stages -workers 20 -config C:\Users\64223\.zoekt\UE.json
./indexing-probe.exe -mode build -workers 20 -shard-mib 16 -baseline-shard-mib 100 -config C:\Users\64223\.zoekt\UE.json -out D:\Code\zoekt\index-probe-fresh -profile D:\Code\zoekt\index-probe.pprof
./indexing-probe.exe -mode verify D:\Code\zoekt\index-probe-fresh\existing D:\Code\zoekt\index-probe-fresh\tasks
go tool pprof -top -relative_percentages -tagfocus=stage=existing D:\Code\zoekt\index-probe.pprof
```

原型用根目录 basename 作文件前缀，适用于本次五个互不重名的 UE 根目录；没有复刻生产代码处理重复根目录名的完整逻辑。verify 输出摘要，比较时须确认文件数、摘要相同。go vet 用于检查诊断程序；生产源码未修改，未将本次调查描述为生产优化已完成。
