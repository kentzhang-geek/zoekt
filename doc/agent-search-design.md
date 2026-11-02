# 面向 Agent 的 Zoekt 搜索接口设计（简化修订版）

状态：接口、真实索引正确性测试、接口 Go benchmark、Windows/Linux CI 和多接口性能脚本已实现。当前运行的服务仍是旧版本；本轮没有启动或重启应用，没有重新索引 Unreal。新接口在 Unreal 上的性能验收等用户部署和索引完成后执行，第 11.3 节数字仍然只是旧 HTTP 基线。实现位置、验证结果和运行命令见第 12 节。

## 1. 评审结论

上一版把多种路径选择、搜索目标、返回模式和高级查询组合在一起，增加了参数组合。本版收敛为常用搜索和独立原生查询。

| 项目 | 本版决定 |
|---|---|
| 路径条件 | 仅保留 `path_regex`，可以匹配路径中任意位置，不自动添加前缀或目录边界 |
| 常用语义 | `files` 搜文件路径并返回文件列表；`snippets` 搜内容并返回文件、行号、命中及上下文 |
| 正则 | 常用接口直接接收 regex；为简化接口，不再提供 pattern_type，字面特殊字符用正则转义 |
| 大小写 | 默认不敏感，可显式选择敏感；同时作用于 pattern 和 path_regex |
| 上下文 | 默认前后各 5 行，范围 0–50 |
| 文件数 | 默认 200，上限 20000 |
| 每文件片段数 | 默认 20，上限 200；按合并后片段计数 |
| 输出字节参数 | 删除 max_output_bytes，不要求 agent 管理字节预算 |
| 超时 | 默认 3000 ms，范围 100–10000 ms |
| 原生入口 | 现有查询入口命名为 search_raw，新常用接口使用 search |
| 额外能力 | 删除上一版第 10 节扩展计划；不加入批量、精确路径数组、摘要、符号工具、读取工具或语义索引 |
| P4 | 不感知 changelist、revision 或工作区版本 |

方向没有根本问题。扩大结果上限允许取得更多结果，但不承诺大请求仍能在相同时间内返回全部结果。默认值是上限，不是必须填满的配额。

## 2. 工具与参数

保留现有 `list_repos`。搜索使用两个入口：`search` 处理常用搜索，`search_raw` 保留原生 Zoekt 查询。

### 2.1 search

| 参数 | 必需 | 含义与默认值 |
|---|---|---|
| `pattern` | 是 | 完整正则；files 中匹配索引相对文件路径，snippets 中匹配内容。空字符串报错 |
| `mode` | 否 | files 或 snippets，默认 snippets；同时决定搜索对象和返回形式 |
| `repos` | 否 | 精确仓库名数组，数组内 OR；省略表示所有仓库，空数组报错 |
| `path_regex` | 否 | 附加的索引相对路径正则，默认无；与 pattern 使用 AND 组合 |
| `case` | 否 | insensitive 或 sensitive，默认 insensitive；同时作用于 pattern 和 path_regex |
| `context_lines` | 否 | 仅 snippets 使用，默认 5，范围 0–50 |
| `max_files` | 否 | 默认 200，范围 1–20000 |
| `max_snippets_per_file` | 否 | 仅 snippets 使用，默认 20，范围 1–200 |
| `timeout_ms` | 否 | 默认 3000，范围 100–10000；服务端排队、搜索和结果准备共享期限 |

常见调用只需要 pattern，通常加上 repos、path_regex；其余参数按需调整。repos 是仓库标识选择，按返回的精确名称匹配，不受 case 影响。

case 设置两个正则的初始大小写模式；表达式内显式的 `(?i:...)`、`(?-i:...)` 仍保留标准正则语义。例如默认不敏感时，`(?-i:Renderer)` 仍要求该部分大小写一致。

files 模式显式传入 context_lines 或 max_snippets_per_file 时返回参数错误，避免 agent 误以为文件搜索也生成内容片段。未知参数也报错，不静默忽略过滤条件。

不再公开 target、path_prefixes、paths、exclude_path_prefixes、pattern_type、max_output_bytes 或常用接口内的 raw_query。

### 2.2 文件搜索

```json
{
  "mode": "files",
  "repos": ["zoekt"],
  "pattern": "(parse|query).*[.]go$",
  "path_regex": "(^|/)query/"
}
```

pattern 描述文件名特征，path_regex 限定目录，二者都匹配文件路径。只返回同时满足的文件，不读取源码上下文。

要列出某个目录下的文件，可以使用 pattern 为 `.*` 并提供 path_regex。这只搜索路径索引，不表示搜索全部文件内容。

### 2.3 内容搜索

```json
{
  "repos": ["zoekt"],
  "pattern": "Parse|RegexpQuery",
  "path_regex": "(^|/)query/"
}
```

省略 mode 时按 snippets 执行：在匹配目录的文件内容中查找两个名称，返回文件、行号、真实命中及前后各 5 行上下文。

常用接口不提供“内容搜索但只返回文件”的第三种组合；特殊原生结果控制交给 search_raw。日常只需理解两种行为。

### 2.4 独立 search_raw

```json
{
  "query": "r:^zoekt$ file:(^|/)query/ (content:Parse or content:RegexpQuery) case:no"
}
```

该例属于 search_raw。本次设计先将原入口改名，保留现有 query / prefix 参数、原生语法和返回能力，不向它混入新接口的参数。新参数表的默认值只适用于 search。

原生查询仍保留原生 case:auto 默认语义；需要不敏感时显式写 case:no。这与常用 search 的默认不敏感要在两个工具说明中分别讲清楚。

prefix 仍是原来的文本拼接行为，并非自动全局范围；Boolean `or` 需要正确分组。常用 search 的范围则直接以 AST 构造，regex 的任意分支都必须满足 repo/path_regex。

部署时客户端刷新工具列表，获取新 search schema 与 search_raw 名称。新 search 不增加隐式 legacy 参数分支。本轮不更新运行服务。

## 3. 路径和正则一致性

索引相对路径及工具展示路径统一使用 `/`。Windows 原生路径只在索引输入或文件系统 I/O 边界转换，匹配阶段不因操作系统改变路径表示。

path_regex 是正则，不是目录前缀：

| 正则 | 含义 |
|---|---|
| `TnT/Code` | 任意位置出现此文本，也可能包含 TnT/CodeExtra |
| `TnT/Code/` | 任意位置出现这个目录字符串 |
| `(^\|/)TnT/Code/` | 任意深度的完整 TnT/Code 目录层级 |
| `^TnT/Code/` | 索引路径开头的 TnT/Code 目录 |
| `(Renderer\|Graphics)/.*[.]cpp$` | 两类目录中的 cpp 文件 |

索引根为 D:/TnT、存储路径为 Code/main.cpp 时，应匹配 Code/。相对路径和本地绝对路径分别标注，不能混用。

pattern 与 path_regex 都作为完整正则解析。空格是表达式中的空格，不是原生 Zoekt 的隐式 AND；不再包一层查询引号。字段里的 file: 没有过滤器作用，需要原生字段时调用 search_raw。

不把正则里的 `\` 全局换成 `/`：它仍用于 `\b`、`\d`、`\.`。目录分隔符与正则转义各有语义；JSON 仍按 JSON 规则转义。

规范化必须发生在索引生成前，不能只改输出路径。检查本地、Git、自定义索引入口，以及文档名、子仓库路径和删除标记。历史反斜杠索引在未来部署时重建或迁移；源系统允许字面反斜杠文件名时，不能盲目转换造成路径碰撞。

## 4. 上下文与边界

默认值为 5，单行命中通常最多 11 行；更多上下文按需显式提高。

| 命中条件 | context_lines = 5 的结果 |
|---|---|
| 第 100 行，文件足够长 | 第 95–105 行 |
| 第 1 行，文件足够长 | 第 1–6 行 |
| 第 2 行，文件足够长 | 第 1–7 行 |
| 最后一行 N | 第 max(1, N-5)–N 行 |
| 文件总共 3 行 | 最多第 1–3 行 |
| 命中跨越第 100–102 行 | 第 95–107 行 |

边界处不向另一侧补足固定行数。首行命中不会为了凑满 11 行而额外返回第 7–11 行。

[contentprovider.go](../index/contentprovider.go) 的 lineStart 将越界位置夹到文件起止位置，getLines 据此切片；末行没有换行符也能纳入范围。新增真实索引测试验证首尾裁剪、空文件的零宽匹配、短文件、CRLF 和无末尾换行。

多个命中窗口重叠或紧邻时合并，代码输出一次；context_lines 为 0 时只返回命中涉及的行。同一行多个命中不重复输出。

max_snippets_per_file 限制合并后的片段数。一个合并片段可能很长，因此片段数不是总行数的严格上限。必须标出真实命中位置，不能把所有上下文都当成命中。

## 5. 大结果上限与性能

### 5.1 大 context 不等于结果处理没有成本

20000 条文件路径与 20000 个文件的代码片段，数据量差异很大。按“单行命中、片段不重叠、每行平均 80 字节”估算：

| 请求规模 | 潜在源码行数 | 粗略文本量，不含元数据 |
|---|---|---|
| 默认 200 文件 × 20 片段 × 11 行 | 44000 行 | 约 3.52 MB |
| 上限 20000 文件 × 200 片段 × 11 行 | 44000000 行 | 约 3.52 GB |

这是规模示例，不是性能实测或严格上界；窗口合并可能减少数量，长行和跨行命中可能增加数量。1M context 是模型总上下文容量，不等同于单次工具消息容量，也不会消除结果构造、传输和阅读成本。

接受默认 200、最大 20000；每文件默认 20、最大 200。不要按最大值乘积预分配内存，也不要为了填满配额继续搜索。

### 5.2 不公开字节参数，保留可见的部署边界

不公开 max_output_bytes。服务端 `-mcp_max_response_bytes` 默认 16 MiB，可配置范围 1024–268435456 字节，用于新 search 的编码响应预算。格式化时按 JSON 转义后的字节计数，预留信封及状态头空间；原生 search_raw 和 HTTP 不套用这个新限制。

引擎同时使用该值 4 倍的累计结果保留预算，默认约 64 MiB，跨并行分片共享，提前停止继续保留结果。它估算源码片段、命中范围和容器开销；排序丢弃的结果不返还预算，因此较保守。它不是进程堆内存的硬上限，单文件解码、正则匹配、合并及 JSON 编码仍有临时分配。

实际触及边界时，应在完整文件或片段边界停止，并返回 output_truncated 和原因。不能静默截断 JSON、UTF-8 或结果尾部，也不能先构造数 GB 再裁剪。部署限制影响结果时，对调用者可见。

“不公开字节预算参数”和“实现完全不约束内存及响应大小”是不同决定。前者简化接口，后者与保持搜索快速、可控的目标冲突。

### 5.3 复用已有能力，避免额外扫描

- repo/path_regex 在搜索前进入查询树，不在全库搜索后才过滤。
- files 只匹配文件名索引，不生成源码上下文。
- snippets 复用 NumContextLines、ChunkMatches，保持引擎相关性并合并重复窗口。
- 展示限制与扫描限制分别处理。提前停止时明确标为未完成，不能暗示取得全库最佳结果。
- 不额外运行全量计数，不为统计重复扫描，不在工具内部调用模型。
- timeout_ms 从接收请求开始传播到排队、搜索和结果准备；大结果编码也检查取消。网络接收和模型阅读不由这个期限保证。

[index/limit.go](../index/limit.go) 的 MaxMatchDisplayCount 按命中范围扣减，不等同于每文件合并后片段数。新参数需要正确适配，不能直接把 20 填入该字段。

[search/aggregate.go](../search/aggregate.go) 在合并分片时排序、裁剪；提高文件上限会扩大工作集。应比较 200、2000、20000 的开销，只有实际发现瓶颈时再针对聚合算法优化。

## 6. 简洁且可信的返回结果

files 返回仓库、规范化相对路径，可解析时附带明确标注的本地绝对路径。按 `(repo, path)` 去重，不返回源码，不承诺每文件完整匹配次数。

snippets 返回文件、片段起止行、真实命中行和原始代码。行号从 1 开始，起止端点均包含在内。采用紧凑的逐文件展示，例如：

```text
search_complete=true output_truncated=false partial_reasons=

repo="zoekt" path="query/parse.go"
lines=95-105
95 | ...
100>| func Parse(...) ...
101 | ...
```

省略号仅为文档占位，实际返回不凭空改写源码；`>` 区分命中行与上下文。路径只在文件头输出一次，不在每行重复。

首版可沿用 MCP 文本 content。适配层保留统一结果结构方便测试，但不默认在文本与 structuredContent 中重复发送两份完整代码。

必要的完成状态：

- search_complete：是否完成指定范围搜索；未知时明确返回未知。
- output_truncated：已发现结果是否因展示或响应限制被省略。
- partial_reasons：实际发生的 timeout、match_limit、response_limit、index_not_ready、shard_error 等。

数量恰好等于上限，不能证明还有更多结果；状态来自实际执行和裁剪。超时后的零结果不能描述成“不存在”。完成状态仅针对当前索引，不引入 P4 版本或同步感知。

## 7. 工具提示：高价值正则配方

值得增加复杂正则例子，但要说明用途和代价。语法以项目解析器为准，参考 [Go regexp/syntax](https://pkg.go.dev/regexp/syntax)。

### 7.1 示例

以下为解码后的正则文本，JSON 传参时相应转义。

| 意图 | 正则 | 说明 |
|---|---|---|
| 一次查多个名称 | `xxx\|yyy` | 查任意一个，可减少相关查询往返 |
| 名称及后续字符 | `xx.+\|yy.+` | 合法，但要求后面至少一个字符；不匹配孤立的 xx 或 yy |
| 初始化命名族 | `(Create\|Init\|Setup).*Renderer` | 一次覆盖多种命名 |
| 精确标识符 | `\b(CreateRenderer\|DestroyRenderer)\b` | 减少长名称中的部分字符串误匹配 |
| 调用形态 | `\b(CreateRenderer\|DestroyRenderer)\s*\(` | 也可能命中声明，不能称为语义调用关系 |
| 同一行先后出现 | `Renderer.*Cache` | 有顺序，默认点号不跨换行 |
| 有界跨行 | `(?s:BeginFrame.{0,300}EndFrame)` | 允许跨行并限制中间字符数；需要跨行关系时再用 |
| 目录与扩展名 | `(^\|/)(Renderer\|Graphics)/.*[.](cpp\|h)$` | 用于 path_regex 或 files 的 pattern |

单个 pattern 的 regex `|` 不会解除外层 repo/path_regex 约束，和 search_raw 的 Boolean `or` 分开说明。

### 7.2 可利用索引比表达式复杂更重要

[index/eval.go](../index/eval.go) 的 regexpToMatchTreeRecursive 从正则提取固定片段；无法提取可用条件时可能退回更宽的候选扫描，OR 中一个不能有效过滤的分支也可能扩大整个候选集。

`xx.+|yy.+` 只有两字符固定串，在缺少其他约束时较难利用三元组索引。用真实、较长且有区分度的名字替换占位符，并先限定仓库或目录。存在三个固定字符是有用信号，不是性能保证。

不要无目的地在每个分支前后加 `.*`。搜索本来就是非锚定匹配，查出现位置通常不需要 `.*Name.*`。大 OR 会扩大候选集，不能承诺总比所有分开查询都快。这符合 [三元组检索的一般机制](https://swtch.com/~rsc/regexp/regexp4.html)。

### 7.3 分清行内、跨行、文件级关系

要找同时包含 Renderer 与 Cache 的文件，而不限制顺序、行号，应使用 search_raw：

```text
r:^zoekt$ content:Renderer content:Cache case:no
```

原生空格表示 AND。不要用 Renderer.*Cache 表达“同一文件包含两词”，那会漏掉逆序和不同行。也不要生成需要 lookaround 或反向引用的 PCRE 表达式，项目解析器不支持这些语法。

### 7.4 建议的精简工具提示

```text
Search indexed code with regex, case-insensitive by default.
mode=files searches paths and returns filenames; mode=snippets searches content
and returns matching lines with context. Paths always use /.
path_regex matches any part of the indexed relative path unless anchored.
Use related alternatives such as Foo|Bar or (Create|Init|Setup).*Renderer to
cover naming variants in one call. Prefer distinctive literal terms and narrow
repo/path scope; very short or wildcard-only branches can broaden scanning.
Patterns are regex strings, not native Zoekt expressions or /regex/ literals.
For file-level Boolean combinations and other native syntax, use search_raw.
Check completion/truncation flags before drawing absence conclusions.
```

## 8. 在现有工具内提高 AI 搜索能力

不恢复上一版已删除的额外接口计划。优先改进查询和证据质量：

1. **按命名族搜索**：用小规模 OR 覆盖同一概念的真实命名差异，不把无关调查塞进巨大 OR。
2. **先确定关系**：区分查名字、调用形态、同一行关系、同一文件共现。表达错误的关系很容易漏搜。
3. **沿证据调整范围**：宽泛词发现候选，再用准确路径和标识符验证；路径已知时直接查内容，不强制先查文件。工具不自动放宽范围。
4. **保留原始局部证据**：标记命中、合并重复上下文、保持相关性，不用自动摘要替代代码。
5. **用实际任务评测工具提示**：比较关键文件命中率、遗漏率、调用次数和延迟。返回更多内容不等于能力更强。

这些主要复用已有能力，不要求新增服务或索引体系。能否“大幅提高”需要任务验证；命名族搜索和准确表达关系最值得优先验证。

## 9. Subagent 保持条件式建议

复杂问题存在独立调查分支时，可提示宿主按目录或子问题委派，让子任务返回短结论、文件行号证据及未覆盖范围；小查询直接执行。

工具不创建 subagent，也不决定如何拆分调查。是否可用服从宿主能力、用户授权和上层规则，工具说明不是授权。MCP sampling 也不等于宿主 subagent 管理，参见 [MCP sampling](https://modelcontextprotocol.io/specification/2025-11-25/client/sampling)。

宿主支持时，子任务只携带必要问题与约束，原始结果留在子任务，主对话收到压缩发现。完整历史反复复制、结果原样转发，通常不利于降低总体消耗。分别衡量主对话占用、合计 token 和总延迟，不保证三项同时下降。

## 10. 实现和验收

主要修改点是 [MCP 入口](../cmd/zoekt-webserver/mcp.go)、查询/结果适配函数、路径索引入口及必要的完成状态传播。参数校验和 AST 构造开销较小，主要测量候选扫描、聚合、上下文生成与传输。

```text
校验 → RepoSet + path_regex + 文件/内容 pattern 的 AND 查询树
     → 带期限搜索 → 文件列表或合并片段 → 限额与完成状态 → 编码
```

新 search 不用 query.Parse 解释整个 pattern，避免把正则空格、括号当作原生查询分词。search_raw 继续用原生解析器。大小写设置必须进入实际匹配对象，不能只改说明。

| 验收项 | 预期 |
|---|---|
| 未传 case | pattern 与 path_regex 都默认不敏感 |
| mode=files | 搜路径、返回文件，不加载代码上下文 |
| mode=snippets | 搜内容、返回文件、行号、命中、上下文 |
| path_regex 未锚定 | 可匹配中间目录，不被强行解释成前缀 |
| pattern 含多个 OR 分支 | 所有结果满足外层 repo/path_regex |
| regex 中有空格、括号、转义 | 按完整正则解析 |
| 首尾行、短文件、空文件、无末尾换行 | 上下文与行号正确 |
| 窗口重叠或紧邻 | 去重且真实命中仍可辨认 |
| 文件上限 200/2000/20000 | 参数生效，测量成本，不承诺期限内填满 |
| 每文件片段 20/200 | 按合并片段限制，不误用全局命中数上限 |
| 超时或响应边界 | 完成/裁剪状态真实，保留可用结果 |
| search_raw | 原解析行为与已支持能力保留 |

验证使用离线内存索引测试行为，并在同一语料上测量扫描、加载、聚合、编码及响应体积。默认值与高上限分开比较。接口实现和 fixture 验证已完成，新 MCP 的 Unreal 性能验收尚待部署；已有旧 HTTP 基线保留作历史参考。

### 10.1 实现前的测试覆盖审计

以下保留实现前的覆盖审计，用来说明新增测试的原因；当前新增覆盖与执行结果见第 12 节。

| 接口 | 已有正确性测试 | 现有覆盖的不足 | 已有接口性能测试 |
|---|---|---|---|
| 旧 MCP `search(query, prefix)` | [mcp_test.go](../cmd/zoekt-webserver/mcp_test.go)：initialize、tools/list、仓库列表、搜索文本/行号、本地绝对路径、Origin 校验 | 搜索使用 fakeStreamer，忽略传入查询和选项；未覆盖真实索引上的 regex、过滤和搜索选项 | 未发现 MCP 接口 benchmark 或自动 Unreal 性能套件；此前工具调用只是语义抽查 |
| 新 MCP `search(pattern, ...)` | 尚未实现，无对应自动测试 | 需要 files/snippets 的完整接口测试 | 尚无 |
| MCP `search_raw(query, prefix)` | 尚未改名上线；可迁移旧 search 的测试 | 需要保留原生行为并补真实索引测试，不能只改测试中的工具名 | 尚无 |
| `POST /api/search`，以及 `/api/list` | [internal/json/json_test.go](../internal/json/json_test.go)：HTTP JSON 往返、查询解析、RepoIDs/空数组、本地路径、Progress 不进入 JSON | 主要使用 mock 搜索器；未形成真实索引上的完整查询/选项/错误契约矩阵 | 已有本轮脚本的 `/api/search` Unreal 基线；未发现对应 Go 接口 benchmark |
| `GET /search?format=json` | [web/e2e_test.go](../web/e2e_test.go)：真实小索引、JSON 片段、上下文、首尾行、无末尾换行、空白行、ctx 校验；另有路径/链接测试 | 已有覆盖较多，但未覆盖本次复杂 regex、跨接口一致性和插件使用方式的完整矩阵 | 当前脚本未覆盖，未发现该入口的接口 benchmark |

这里的 HTTP 接口在项目中称为 [JSON HTTP API](json-api.md)，无需按是否严格符合 REST 风格区分测试责任。`POST /api/search` 的 JSON API 与 `GET /search?format=json` 的网页 JSON 输出是两个不同契约，MCP 也是独立契约，不能只测它们共用的搜索引擎。

### 10.2 接口正确性测试要求

保留已有测试并补充以下内容。真实匹配测试使用相同的内存序列化索引 fixture，包含两个仓库、同名文件、相似目录、命中/不命中内容、首尾行、空文件、短文件、Unicode、CRLF 和无末尾换行。预期结果手工定义；不能只比较几个接口相互一致，否则共同的错误也可能通过。超时和故障注入可以使用受控搜索器，正常匹配必须经过真实索引。

| 测试组 | 必须验证的行为 |
|---|---|
| MCP 协议与 schema | 经 HTTP handler 完成 initialize → tools/list → tools/call；工具清单包含 list_repos、search、search_raw，参数说明与实际默认值一致；无效 JSON、未知工具、参数类型错误有明确错误，新 search 拒绝未知参数；保留已有协议和 Origin 测试 |
| 新 search 查询语义 | files 仅匹配路径并返回文件；snippets 仅匹配内容；pattern 中空格/括号/转义保留为 regex；分组、OR、单词边界和有界跨行匹配；默认不敏感及显式敏感同时作用于 pattern/path_regex |
| 新 search 范围 | repo 精确名称数组 OR，与 path_regex、pattern 做 AND；同名文件不会跨仓库泄漏；路径在任意深度匹配，锚定有效；regex 的所有 OR 分支均受外层范围限定；空 repo 数组报错 |
| 路径与索引入口 | Windows 输入在索引边界规范化后可用 `/` 查询；返回的索引相对路径和本地绝对路径分别正确；索引根与磁盘路径不能混用；`\b`、`\d`、`\.` 不被路径规范化改写；相关 fixture 在 Windows 和 Linux 执行 |
| 新 search 结果和边界 | 行号、命中范围、前后 context、首尾裁剪、跨行命中、重叠/相邻窗口合并、每文件片段数、文件上限准确；上下文不得冒充命中；确实省略结果才设置裁剪，期限和错误状态不得伪装成完整零结果 |
| 新 search 参数 | 缺失/空 pattern、非法 regex、越界数字、无效枚举；files 模式传 snippets 参数报错；默认 200 文件、20 片段、5 行、3000 ms 与第 2 节一致；大上限校验与实际结果限制分别测试 |
| search_raw 兼容 | query/prefix 拼接、引号转义、原生 file/content、AND/or/否定/type:filename 和 case:auto/no/yes；保留原有输出与能力；错误 regex 和无结果均覆盖。prefix 不是全局过滤，不能把新 search 的 AST 范围保证错误地套到 raw 上 |
| HTTP POST | 经实际挂载的 `/api/search`、`/api/list` 路由调用，不只测内部 `/search` handler；Q/RepoIDs/Opts 的解析和传递；RepoIDs 省略、空数组及非空数组；真实 regex/路径/大小写/文件查询、NumContextLines、显示限制和期限；缺少 Q、非法 JSON/regex、错误方法和引擎错误的状态码及响应体 |
| HTTP GET | 经 `/search?format=json` 验证 q/num/ctx；URL 编码后正则中的 `+`、`&`、`#`、反斜杠、空格和 Unicode 正确传递；保留当前 ctx 0–10、默认值及错误行为，不能直接套用新 MCP 的 0–50；补复杂正则、过滤、文件列表、零结果和上下文边界 |
| 跨接口结果 | 在小 fixture 无裁剪、参数明确对齐时，比较仓库、逻辑文件路径、真实命中行/范围和解码后的文本；允许新 MCP 合并上下文、HTTP 返回结构不同。原生接口显式 case:no 才能与新 search 默认不敏感做等价比较 |

search_raw 的兼容性基线来自旧 MCP search，相同输入除工具名外应保持原生查询含义。对包含 Boolean or 的范围查询，分别验证原来的未分组语义与正确分组语义，不通过测试把错误的全局过滤假设固化下来。

### 10.3 HTTP / Vim 插件兼容性与执行方式

当前上下文中尚未提供 Vim 插件代码或真实请求样例，因此同时覆盖 POST JSON 和 GET JSON 的字段、编码、行号和既有本地路径契约。插件内部的解析、UI 刷新及文件跳转未做端到端验证，不能把接口测试等同于插件实测。

HTTP 的兼容性要求：

- 保留请求路由、字段名、默认行为和响应层级：POST 使用 `Result.Files/LineMatches`；GET 使用 `result.FileMatches/Matches`。不能因新增 MCP search 而统一成同一种响应格式。
- 保留现有字段类型和编码：POST 的非空 Line/Before/After 字节字段使用 JSON base64 表示，空值编码也保持兼容；GET 的 Fragments 是包含 Pre/Match/Post 字符串的对象数组，Before/After 是字符串。断言换行、Unicode、行号和偏移量正确，Vim quickfix/location list 所需的文件路径、命中行及正文应能正确提取。
- 新 MCP 的 `/` 相对路径规范不能直接覆盖 HTTP 既有 FileName 的绝对路径行为；已能跳转到本地文件的 Windows 路径必须继续可用。跨接口比较时只在测试归一化层转换，不改变线上响应来让测试通过。
- 除明确提出并测试的修复外，保留现有参数/错误语义，包括 GET 的错误状态和 POST 的错误对象；MCP 增加参数或改名不隐式修改 HTTP。插件若依赖文件详情入口，再补该实际调用链的回归。

普通 CI 执行新旧 MCP、HTTP 和共享 fixture 的正确性测试；对路径相关测试增加 Windows/Linux 覆盖。MCP handler 的 fake 测试用于协议和错误分支，另加真实索引与 `httptest.NewServer` 集成测试覆盖传输和匹配。实现后可用以下命令运行对应测试包，具体依赖遵循现有 CI 配置：

```powershell
go test ./cmd/zoekt-webserver ./internal/json ./web -count=1
```

Unreal 性能回归在有该索引的固定测试机上执行，不把 Unreal 源码放入普通 CI。新增测试已实现并执行，具体命令与 Windows 环境限制见第 12 节。

## 11. Unreal 实测与自动性能回归

### 11.1 实现前基线与部署后验证

已有引擎的正则、文件过滤和性能已用于实现前基线。新参数的 AST 构造、默认大小写、每文件片段数、上下文合并和完成状态已有 fixture 测试；Unreal 覆盖和真实 MCP 编码、传输开销需要部署后再次验证。旧 MCP 成功不代表新接口已经通过大库验收。

Unreal 性能测试以真实 MCP 接口为主：新 search 的 files/snippets 和 search_raw 都必须通过 `tools/call` 跑完对应用例。HTTP 性能作为独立的 Vim 插件兼容性回归和诊断对照；只有 HTTP 或引擎达标，不能判定 MCP 达标。当前已有数字来自旧 HTTP 接口，不是 MCP 性能基线。

测试分四层：

| 层次 | 语料及目的 | 执行时机 |
|---|---|---|
| 正确性 | 小型可控临时索引；通过各接口断言命中、上下文、过滤、编码与兼容性，详见第 10 节 | 普通 CI，每次修改 |
| 引擎基线 | 当前 Unreal 索引；原生查询与 SearchOptions 固定，观察候选扫描、加载和返回成本 | 现在，以及新 build/index 完成后 |
| MCP 主验收 | 同一 Unreal 语料；新 search 的 files/snippets 与 search_raw，包含真实 MCP 请求、搜索、格式化和完整响应 | 接口实现后，部署必做 |
| HTTP 独立回归 | 同一 Unreal 语料；POST `/api/search` 和 GET `/search?format=json`，覆盖插件默认调用和复杂查询 | 接口实现后，部署必做 |

Unreal 是主测试目标：它就是用户经常搜索的大型 C++ 代码库，比换一个较小仓库更贴近实际需求。小型 fixture 用于保证准确性，不能代替大库性能测试。暂不增加 Chromium 等大型语料；未来若要测大量独立仓库，可复用已有合成分片 benchmark。

### 11.2 已有性能测试

仓库已经有 Go benchmark，例如：

- [BenchmarkShardedSearch](../search/shards_test.go)：构建 3000 个测试仓库、每个 300 个文件，覆盖 substring、regexp 捕获/非捕获、repo set，以及零/部分/大量结果。它是合成语料测试，初始化成本也较高。
- [BenchmarkMinFrequencyNgramOffsets](../index/indexdata_test.go)、[BenchmarkCompressedPostingIterator](../index/hititer_test.go)：三元组选择与倒排遍历。
- [BenchmarkScoreChunkMatches](../index/index_test.go)、[BenchmarkColumnHelper](../index/contentprovider_test.go)：片段评分与列位置计算。
- [BenchmarkYield](../search/sched_test.go)：调度器；另有索引构建、元数据读取和序列化 benchmark。

示例命令，按需要选择；普通 `go test ./...` 不执行 benchmark：

```powershell
go test ./index -run '^$' -bench '^Benchmark(MinFrequencyNgramOffsets|CompressedPostingIterator)$' -benchmem -count=5
go test ./search -run '^$' -bench '^BenchmarkShardedSearch$' -benchmem -benchtime=1s -count=3
```

原有 [CI](../.github/workflows/ci.yml) 执行 Go 测试。本轮另外增加 [接口契约 CI](../.github/workflows/search-interfaces.yml)，不在 CI 下载或索引 Unreal。上述已有引擎 benchmark 未在本轮重跑；新增 MCP/HTTP fixture benchmark 与 Unreal 脚本分别见第 12 节和下文。

### 11.3 当前 Unreal 基线

测试时间：2026-10-27 晚（Vancouver），报告记录为 2026-10-28 UTC。旧运行服务、仓库 `ue`，索引报告 168033 个文件、17 个分片、约 1.75 GB 内容、46918632 个换行。这里只代表已索引部分，不假设包含整套 Unreal 的所有文件。

先通过现有 Zoekt MCP 验证语义，再通过本机 `/api/search` 获取 `Stats.Duration` 和完整 HTTP 响应耗时。正常用例先记录一次观察、额外预热 3 次，然后连续计时 30 次；串行执行、复用连接。缓存是既有服务的实际状态，没有清理系统页缓存，首次观察不能称为冷启动。

完整原始计时、选项、索引信息和结果指纹保存在 [正常基线](benchmarks/unreal-legacy-baseline.json) 和 [短词分支压力探测](benchmarks/unreal-legacy-stress.json)。这些报告不保存 Unreal 源码正文。报告中的 `passed` 表示脚本预期检查通过，不表示所有查询满足延迟目标，也不表示搜索完整。

| 用例 | 引擎 p50 / p95（ms） | 本机 HTTP p95（ms） | 观察 |
|---|---:|---:|---|
| RenderCore 文件名正则 | 0.52 / 0.65 | 1.58 | 2 文件，FilesLoaded=0 |
| 精确文件 + RegisterExternalTexture | 0.58 / 0.72 | 1.18 | 1 文件 |
| 精确文件 + `(Register\|Queue)External(Texture\|Buffer)` | 5.49 / 7.05 | 7.47 | 1 文件，5 个命中行 |
| RenderCore + `\bRegisterExternal(Texture\|Buffer)\s*\(` | 19.88 / 26.76 | 28.03 | 7 文件，加载 9 文件 |
| 全 Unreal + 同一个调用正则 | 236.66 / 252.27 | 258.78 | 引擎报告 237 文件，返回上限 200，已发生展示裁剪 |
| 精确文件 + 有界跨行正则 | 2.37 / 5.32 | 6.79 | 两段声明，旧接口返回 7 个命中行 |
| 同一文件共现 AND | 0.72 / 1.18 | 1.96 | 两词不要求出现在同一行 |
| 小写 pattern/path + case:no | 3.80 / 4.45 | 5.75 | 能匹配大小写混合的路径及函数名 |
| 将内容限定到另一已存在文件 | 1.71 / 2.38 | 2.97 | 0 文件，没有越过文件过滤 |

表格中的 `\|` 是 Markdown 表格转义；实际正则中的分支操作符是 `|`。本轮实际验证的查询示例：

```text
r:^ue$ case:no file:(^|/)RenderCore/Public/RenderGraphBuilder[.]h$ content:(Register|Queue)External(Texture|Buffer)
r:^ue$ case:no file:(^|/)RenderCore/Public/RenderGraphBuilder[.]h$ content:(?s:RegisterExternalTexture\(.{0,250}ERDGTextureFlags)
```

当前文件的索引相对路径是 `Source/Runtime/RenderCore/Public/RenderGraphBuilder.h`，磁盘绝对路径包含 `Engine/Source/...`。因此固定写 `/` 还不够：工具也必须明确路径相对于索引根。上面的非根锚定目录表达式不依赖索引根是否包含 Engine。

另做一次压力探测：全库 `content:(RegisterExternalTexture|xx.+)`，引擎期限 3000 ms，返回上限 20 文件/200 范围、无上下文。一次初始观察后计时 3 次，引擎约 3010–3020 ms，并有 FilesSkipped 和实际展示裁剪；不是完整搜索。最后一次加载约 4 万文件、约 425 MB 内容。3 个样本仅用于发现问题，不能用于稳定的尾延迟统计。

这说明较弱的 OR 分支可能使整个查询扫描大量候选，即使另一个分支包含很长的函数名。正则写得长或复杂，不代表索引选择性好。提高返回上限或去掉上限不会解决这个问题。

### 11.4 已提供的脚本

[scripts/benchmark_search.py](../scripts/benchmark_search.py) 使用 Python 3.10+ 标准库，只读访问已经运行的服务。默认包含 9 个正常用例；较重的短词分支必须显式启用。它不构建、不索引、不重启、不修改缓存。

```powershell
# 新服务部署和 Unreal 索引完成后运行，默认使用新 MCP。
python scripts/benchmark_search.py --repo ue --runs 100 --warmup 3 --label after-build --output benchmark-mcp.json

# 依次运行 MCP、新名 raw、HTTP POST、HTTP GET；不会同时跑不同接口。
python scripts/benchmark_search.py --repo ue --transport all --runs 100 --output benchmark-all.json

# 比较同机、同语料、同参数基线；报告结果/覆盖变化及 p95 回归候选。
python scripts/benchmark_search.py --repo ue --runs 100 --baseline benchmark-mcp.json --output benchmark-mcp-next.json

# 明确扩大结果上限；其他默认值不能在 defaults 配置中被悄悄覆盖。
python scripts/benchmark_search.py --repo ue --profile aligned --max-files 2000 --max-snippets-per-file 200 --runs 100 --output benchmark-large.json

# 并发 4 使用四条独立连接，预热及初始化不计入搜索样本。
python scripts/benchmark_search.py --repo ue --concurrency 4 --runs 100 --output benchmark-concurrency4.json

# 若要单独对旧服务建立 MCP 基线，必须显式指定 legacy。
python scripts/benchmark_search.py --repo ue --transport mcp-legacy --runs 100 --output benchmark-legacy.json

# 独立的压力用例；避免混进正常查询的平均值。
python scripts/benchmark_search.py --repo ue --include-stress --case weak_branch --runs 3 --warmup 0 --timeout-ms 3000 --output benchmark-stress.json
```

`--profile defaults` 使用各接口自身默认值；`aligned` 显式传递各接口支持的控制项。新 search 对应 200 文件、每文件 20 合并片段和前后各 5 行；POST 对照使用全局 4000 个命中范围，两者不是相同限制。旧 raw 仍有固定上限，GET 不支持引擎期限参数，报告保留实际请求及这些差异，不能宣称完全等价。GET ctx 上限为 10；更大上下文单独测 MCP 或 POST。

`--transport mcp` 中仅文件级 AND 用例调用 search_raw，其余用例调用新 search；`mcp-raw` 全部调用 search_raw。新 schema 缺失时明确失败，不会退回旧工具或 HTTP。`all` 不含 `mcp-legacy`，旧服务需要单独测试。`--url` 默认 `http://127.0.0.1:6070`，仓库名默认 `ue`，均可修改。

脚本检查结果确实存在/不存在、返回仓库、文件过滤和重复运行的结果稳定性；保存实际查询、耗时、响应字节数、加载量及已有的部分结果信号。一般用例出现意外裁剪、扫描跳过或崩溃会失败；全库调用用例允许已知的显示裁剪，压力用例允许部分结果，但均明确记录。旧 API 缺少可靠的整体完成标记，没有这些信号也不保证完整，尤其不能把超时后的零结果当成负向测试成功。

性能口径：

- `engine_ms` 来自 `Stats.Duration`，依据本地实现，包含搜索和聚合，不包含排队；`queue_ms` 单独记录。运行中的旧二进制精确源码版本未知，数值以它返回的字段为准。
- 新报告的 `transport_ms` 从发送真实 MCP/HTTP 请求到收完响应，包含服务端处理、编码和传输；`decode_ms` 与 `validation_ms` 单列。旧报告的 `http_ms` 只代表旧 HTTP 请求，不能改标签当作新 MCP 数据。
- MCP 初始化、通知和 tools/list 耗时放在独立 setup 字段；未暴露 Stats 的接口其 `engine_ms`、`queue_ms` 为 null，不使用额外 HTTP 调用冒充本次 MCP 的内部时间。
- 分片的 `MatchTreeConstruction`/`MatchTreeSearch` 是累计工作量，不能直接与总墙钟耗时相加。
- 文件名模式应保持 `FilesLoaded=0`；候选数、加载字节和响应大小用于解释退化，而不是只盯最终耗时。

### 11.5 纳入实现计划的性能验收

1. **先做确定性 fixture**：检验完整正则参数、文件过滤、大小写、首尾行/短文件/空文件/无末尾换行、跨行片段、重叠合并、真实裁剪与超时传播。少返回正确结果不能抵消漏搜或过滤错误。
2. **MCP 主套件**：脚本已支持新 MCP `search` 的 files/snippets 和 `search_raw`，通过真实 `tools/call` 测试。旧版对比使用独立 legacy 配置；新工具缺失时明确失败。统一用例意图，但分别映射新旧参数；不能把旧的全局命中数限制当作新的每文件片段限制。
3. **补接口 Go benchmark 与 HTTP 套件**：分别测 AST 构造、上下文合并、各接口的输出编码与分配；增加 MCP handler、POST JSON handler、GET JSON handler benchmark。随后在实际 Unreal 索引上跑 MCP、HTTP POST、HTTP GET 三组端到端测试。各接口耗时和返回大小单列，微基准不能替代真实接口验收。
4. **区分负载等级**：正常选择性查询、全库较多结果、短词/通配压力查询分组报告。context_lines 测 0/5/20；files 测 200/2000/20000；snippets 分开测每文件 20/200 和受控的大文件数。大响应压力测试必须受期限和部署边界保护，不能在普通 CI 里无约束物化最大值乘积。
5. **固定比较条件**：同一机器、CPU/并发设置、build 模式、原始语料和索引配置；索引完成后测试，保持查询和返回限制相同。当前报告记录语料规模及索引元数据，但它们不等同于完整语料校验。新旧 build 的严格对比应使用冻结的源文件集合。
6. **自动汇总和判定**：脚本已支持 `--baseline`，按接口、用例输出 p50/p95/max、延迟、响应体积、扫描/裁剪信号与结果签名差异。请求、profile 或并发不同记为不可比；覆盖或结果变化单列，不算性能改善。p95 超过基线 20% 且绝对增加超过 5 ms 时记为回归候选并非零退出；需再运行一轮确认，脚本不会自动重跑或替换基线。历史 HTTP 报告缺少新请求配置字段时不能直接自动比较，须冻结语料后另建同格式基线。
7. **分开测热、冷和并发**：日常性能门禁使用明确预热、至少 100 次正常查询样本，主要看 p95；当前 30 次是初步基线。冷启动需单独受控启动并注明页缓存状态，不以“第一次查询”冒充冷缓存；并发 1 与 4 分开测试，统计排队。压力探测独立执行，不掺入正常组。

建议初始目标，待新接口在固定机器上实测后冻结为门禁：

| 查询组 | 建议目标 | 限定条件 |
|---|---|---|
| 指定目录/文件的有效正则 | 引擎热态 p95 ≤ 50 ms，本机新 MCP p95 ≤ 100 ms | 固定用例，正常默认参数、小响应，无意外漏搜/超时 |
| 全 Unreal 的选择性正则 | 引擎热态 p95 ≤ 500 ms | 单独跟踪返回数量和裁剪；不承诺所有全库查询都符合 |
| 缺少有效固定串、巨大结果 | 不要求几十毫秒完成；要求期限、取消和部分结果状态正确 | 允许超时，但必须显式呈现，限制资源占用 |

首版更有效的优化方向是保持有区分度的固定串、在查询树内提前施加目录过滤、避免不必要的正文读取和重复上下文，之后再依据 CPU/内存 profile 调整候选扫描或聚合。不能通过悄悄放宽过滤、缩减覆盖范围或只改说明来达成性能目标。现有实测支持限定范围的复杂正则达到几毫秒至几十毫秒，也明确显示某些全库正则会达到几百毫秒甚至秒级。

### 11.6 MCP 主验收与 HTTP 回归的具体执行口径

以下接口适配均已实现；真实 Unreal 的新服务验收尚未执行。

| 套件 | 真实请求链路 | 验收责任 |
|---|---|---|
| 新 MCP search | 初始化并获取 schema 后，通过 `/mcp` 的 tools/call 分别调用 files/snippets | 主要性能目标；覆盖默认参数、复杂 regex/路径过滤、上下文和较大返回 |
| MCP search_raw | 同一 MCP 会话/协议配置，通过 tools/call 调用原生查询 | 原生兼容性及独立性能回归，不能由新 search 的结果代替 |
| 旧 MCP 基线 | 对旧服务显式调用 search(query, prefix)，结果单独标注 legacy | 与改名后的 search_raw 做同参数比较；不得假定旧服务支持新参数 |
| HTTP POST | `/api/search` 的真实 JSON 请求；按需调用 `/api/list` 准备仓库选择 | 复用现有基线，加插件实际默认参数与缺省 Opts 的用例 |
| HTTP GET | `/search?format=json&q=...&num=...&ctx=...`，由 URL 编码器生成请求 | 新增独立基线，覆盖网页 JSON 格式化与插件实际请求方式 |

MCP 的主要延迟从发出 tools/call 到收到完整响应计算，包含请求解析、排队、搜索、片段生成、结果编码和传输；初始化和 tools/list 单列，不能混入每次搜索计时。解析结果还需验证 JSON-RPC error 和工具 isError，HTTP 200 不代表搜索成功。缺少预期工具、参数被忽略或不能解析结果均为失败。

HTTP 继续记录发送至收完响应的时间。MCP/HTTP 均另行记录客户端解码及结果验证耗时，避免旧 HTTP 基线不含 JSON 解码、新 MCP 基线却包含，造成错误比较。Vim UI 刷新和模型阅读不计入服务接口耗时；插件实际使用的编码、解析和文件跳转通过兼容性用例验收。

各接口运行两种配置：一组使用该接口真实默认值和插件实际请求，发现使用体验退化；另一组显式对齐可等价的语义与选项，用于定位接口额外开销。保留 HTTP 默认扫描限额估算的测试，不能因为当前引擎对照脚本绕过该估算，就宣称覆盖了插件默认性能。GET ctx 上限 10 与 MCP context_lines 上限 50 分别测试，跨接口对照只选共同支持的范围。

MCP 若未暴露引擎 Stats，主报告仍以真实 tools/call 总耗时为准；可在独立的 HTTP 对照运行中采集引擎指标，但不能把那次查询的 Duration 当成本次 MCP 的内部耗时。额外统计请求不放入主计时区间；并发测试也不能同时运行另一套基准干扰结果。

在小 fixture 中严格验证完整预期集合；在 Unreal 上至少验证已知正向证据、路径/仓库约束、返回完整性或裁剪状态、同语料下的稳定性。对原生接口缺乏可靠完成标记的情况明确记为未知，不伪造新 MCP 的完成状态。结果正确性、MCP 延迟、HTTP 兼容性和 HTTP 延迟分别出结论，任一必做套件未运行或失败，都不能标记本次部署验收全部通过。

## 12. 实现交付与本机验证

### 12.1 代码与行为

| 实现 | 位置 |
|---|---|
| 新 search 参数、完整 regex AST、范围过滤、上下文合并及紧凑结果 | [mcp_search.go](../cmd/zoekt-webserver/mcp_search.go) |
| 三个工具的协议注册、原接口改名 search_raw、编码后期限状态复核 | [mcp.go](../cmd/zoekt-webserver/mcp.go) |
| 新接口专用的跨分片预算和完成信号 | [searchbudget](../internal/searchbudget/budget.go)、[index/eval.go](../index/eval.go)、[search/shards.go](../search/shards.go) |
| Windows 索引路径、子仓库及删除标记规范化 | [index/builder.go](../index/builder.go)、[index/shard_builder.go](../index/shard_builder.go)、[gitindex/tree.go](../gitindex/tree.go) |
| 两仓库真实索引 fixture | [internal/searchtest](../internal/searchtest/fixture.go) |
| MCP 新旧接口正确性及微基准 | [mcp_search_test.go](../cmd/zoekt-webserver/mcp_search_test.go)、原有 [mcp_test.go](../cmd/zoekt-webserver/mcp_test.go) |
| HTTP 两入口的过滤、行号、编码、上下文和错误契约及微基准 | [web/agent_search_test.go](../web/agent_search_test.go)、原有 [JSON 测试](../internal/json/json_test.go) |
| 索引路径、子仓库、删除标记、预算及跨分片完成测试 | [index 测试](../index/agent_search_test.go)、[search 测试](../search/agent_search_test.go)、[Git 路径测试](../gitindex/agent_search_test.go) |
| Unreal MCP/HTTP 多接口性能脚本及离线适配测试 | [runner](../scripts/benchmark_search.py)、[adapters](../scripts/search_benchmark_backends.py)、[tests](../scripts/test_benchmark_search.py) |

查询直接生成 `RepoSet AND path_regex AND pattern`，原生查询字符串拼接不参与新 search。保留 regex 内嵌大小写标志，并修正单词边界优化对 FoldCase 的处理，避免默认不敏感模式下 `\balpha\b` 漏掉 `Alpha`。

`partial_reasons` 区分扫描期限/取消/引擎限额与展示裁剪：`timeout`、`cancelled`、`match_limit`、`index_not_ready`、`shard_error`、`search_error`、`search_incomplete` 表示搜索覆盖不完整；`file_limit`、`snippet_limit` 表示省略已发现结果。`response_limit` 可能在引擎保留或最终格式化阶段触发，分别影响搜索完成和输出裁剪状态。元数据查询失败记为 `path_metadata_unavailable`，仍可返回仓库及逻辑路径。无法取得可靠完成证据的搜索器返回 `search_complete=unknown`。

默认 3000 ms 期限从 HTTP handler 收到请求时开始，传至搜索调度、引擎、元数据读取和格式化。JSON 编码后再次检查期限并更新状态，始终返回完整 JSON；它不是强制在精确毫秒边界中断所有分配或网络写入的保证。

### 12.2 自动测试与构建

2026-10-28 在 Windows 本机执行以下新增契约测试及离线 Python 测试，均通过。`httptest.NewServer` 仅使用操作系统分配的临时端口；没有启动 zoekt 应用、后台索引器或连接现有 Unreal 服务。

```powershell
$env:GOMAXPROCS='2'
go test -p 1 ./cmd/zoekt-webserver ./web ./internal/json ./index ./search ./gitindex -run '^(TestMCP|TestHTTP|TestAgent|TestClientServer|TestProgressNotEncoded)' -count=1
python -B -m unittest discover -s scripts -p test_benchmark_search.py
go build -p 1 ./cmd/zoekt-webserver ./cmd/zoekt-index ./cmd/zoekt-git-index
```

新增 [CI](../.github/workflows/search-interfaces.yml) 在 Windows 和 Linux 跑这些接口与路径契约测试及 Python 离线测试。Linux 的 CI 执行结果须以实际工作流为准，本机没有宣称验证 Linux 运行结果。

扩大检查范围后，根包、query、search、internal/json、webserver 和 zoekt-index 的短测试通过。index/web 有 4 个既有失败：`TestIgnoreSizeMax`、`TestOptions_shardName`、`TestShardName`、`TestTruncateLine`；在临时目录解出的干净 HEAD 中独立复现，相应旧测试文件与实现前版本一致。排除这 4 个已确认失败后，其余 index/web 短测试通过。Git 全量集成测试还依赖本机不存在的 `/bin/sh` / `sh`，并有旧 mmap 文件清理失败，故不标记全仓测试全部通过。

Windows 上的 `go build ./cmd/...` 包含使用 Unix 信号和 Umask 的 `zoekt-sourcegraph-indexserver`，不能整体构建；并行链接也曾耗尽本机内存。因此对 webserver、普通索引器和 Git 索引器使用 `-p 1` 构建并通过。没有改动无关的 Unix 专用命令来规避这些限制。

### 12.3 接口微基准与部署验收

```powershell
$env:GOMAXPROCS='2'
go test -p 1 ./cmd/zoekt-webserver ./web -run '^$' -bench '^(BenchmarkMCPSearch|BenchmarkHTTPJSONSearch)$' -benchmem -benchtime=100ms -count=1
```

本机 i9-10900K、Windows amd64、GOMAXPROCS=2 的一次短测：

| 小 fixture 接口 | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| MCP search | 76155 | 31316 | 381 |
| MCP search_raw | 68323 | 33902 | 431 |
| HTTP POST | 82211 | 36383 | 453 |
| HTTP GET JSON | 77619 | 38567 | 491 |

这些 benchmark 使用真实小索引及 handler/编码，但没有网络传输，只是单轮均摊结果，不是 p95，也不能据此推断 Unreal 的毫秒级性能。真实 MCP 延迟、较大返回、冷启动和固定机器回归门禁仍以第 11 节的部署验收为准。

部署时先使用新构建完成 Unreal 索引，再刷新客户端工具清单，确认 `search(pattern, ...)` 和 `search_raw(query, prefix)`。历史索引若保存了 Windows 反斜杠路径，需要完整重建；仅更新二进制或工具提示不会改写已有索引。本轮保留正在运行的旧服务及已有索引，未执行任何 Unreal 性能新测。
