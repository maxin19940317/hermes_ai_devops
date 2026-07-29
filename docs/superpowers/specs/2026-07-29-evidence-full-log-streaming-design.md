# Evidence 全日志流式扫描设计（差距 #9）

日期：2026-07-29

状态：**已批准**（2026-07-29 评审通过）

## 1. 背景

当前 `runtime/internal/evidence/evidence.go` 的 `readWindow` 会读完整个文件，
但只保留最后 8MB，再在这个尾部窗口中执行签名匹配。它限制了内存，却产生两个
错误语义：

1. 位于文件前部、尾部 8MB 之外的失败签名永远不会命中。
2. `Match.LineNo` 是尾部窗口内的相对行号，不是原日志的真实行号。

这违反 `docs/device-test-sequence.md` 差距 #9 的目标。修复后必须扫描从第一字节
到最后一字节，同时继续遵守 Hermes 输入边界：`evidence.json` 只保留有界上下文，
绝不携带完整原始日志。

## 2. 目标与非目标

### 2.1 目标

- 对 `logcat`、`stdout`、`stderr` 从头到尾执行签名匹配。
- 命中行号使用原文件从 1 开始的全局行号。
- 每个命中继续保留前后各 50 行，每条签名最多 3 个命中。
- 全部签名未命中时，继续提供 stdout/stderr 尾部和 logcat 首批错误行。
- 内存占用只与签名数、上下文上限和单行上限有关，不与文件总大小有关。
- 保持 evidence 总上下文 96KB 的既有预算。
- 缺失文件、非法正则、读取失败继续降级到结构化输出，不让 `Extract` 返回错误。

### 2.2 非目标

- 不改变规则引擎的分类和 verdict 逻辑。
- 不把完整日志写入 evidence snapshot 或发送给 LLM。
- 不在本轮实现 `fetch_log_range`；原始附件仍由 MinIO 保存。
- 不改变 JUnit 的 XML 流式解析路径。
- 不改变 Manifest 的 failure signature 语法。

## 3. 方案选择

采用**单遍按行流式扫描**。

不采用两遍扫描，因为 `Input.Files` 暴露的是不可回退的 `io.Reader`；第二遍需要
临时文件或接口改造，会破坏当前纯函数边界。不采用按任意字节块匹配，因为跨块正则、
行号和前后 50 行上下文会显著增加复杂度。

单遍扫描时，每个文件只消费一次。扫描器同时维护：

- 最近 50 行的共享环形缓冲；
- 每条签名最多 3 个正在等待后 50 行的 capture；
- stdout/stderr 的有界尾部；
- logcat 最先出现的最多 50 条 E/F 错误行；
- 完整文件字节数和全局行号。

内存上界与签名数量成正比，但与日志长度无关。当前每个变体最多只有少量签名；
即使未来增加签名，`maxMatchesPerSignature`、`contextLines` 和单行保留上限仍给出
确定上界。

## 4. 扫描模型

### 4.1 预编译与分组

`Extract` 先按声明顺序编译所有正则：

- 编译失败的签名立即产生 `regex compile` 错误；
- 合法签名按 `Where` 分组到 `logcat`、`stdout`、`stderr`；
- 缺失的目标文件继续产生 `log missing` 错误；
- 同一个文件只建立一个 scanner，所有引用它的签名在同一遍扫描中判断。

签名结果最终仍按输入声明顺序输出。

### 4.2 行状态

每读到一行：

1. 增加全局行号和完整字节计数。
2. 将适合输出的有界行表示加入前文环形缓冲。
3. 把当前行追加给已有 capture，并减少其剩余后文行数。
4. 用完整的、受单行扫描上限约束的行执行该文件全部正则。
5. 对尚未达到 3 个命中的签名创建 capture：
   - 复制当前行之前最多 50 行；
   - 记录真实全局行号；
   - 加入当前命中行；
   - 等待后续最多 50 行。
6. 更新 fallback 尾部或 logcat 错误行集合。
7. EOF 时立即完成仍在等待后文的 capture。

相邻或重叠命中各自保留上下文，不做合并，保持现有输出形态。

### 4.3 单行上限

日志总大小不设扫描上限，但单行设置 `maxScanLineBytes = 1MiB`：

- 使用可持续消费 fragment 的行读取器，超长行的剩余 fragment 必须一直读到换行或
  EOF，不能让后续日志消失；
- 正则只对该行保留的前 1MiB 执行；
- 上下文表示进一步限制为 `maxContextLineBytes = 8KiB`，超出部分保留头尾并插入
  明确的省略标记；
- 出现超长行时将文件加入 `inputs.truncated_files`，并设置顶层
  `truncated = true`，表示该行可能存在无法匹配的尾部内容。

这是唯一允许的扫描降级。普通大文件即使超过 8MB，也不再进入
`truncated_files`。

### 4.4 输出预算

扫描阶段收集每条签名最多 3 个候选上下文。扫描完成后按**签名声明顺序**应用既有
`contextBudgetBytes = 96KiB`：

- 候选加入后不超过预算：保留；
- 候选会越过预算：停止加入新上下文，设置顶层 `truncated = true`；
- 当前签名已有部分命中时，将最后一个保留命中标记为 `truncated`；
- 当前签名没有可保留命中时，写入 `context budget exhausted`；
- 后续签名沿用现有预算耗尽语义。

这样既能单遍读取，也不会因为文件扫描顺序改变签名声明的优先级。

## 5. Fallback 摘录

扫描每个文件时同步收集 fallback 所需状态，因此不需要在确认“全部签名未命中”后
重新读取 `io.Reader`：

- stdout/stderr：保留最后 `excerptFileBytes = 16KiB` 的完整行尾部；
- logcat：从全文件中保留最先出现的最多 50 条 E/F 行，而不是尾部 8MB 内的前 50 条；
- 只有全部合法签名都没有命中时才把这些候选写入 `Evidence.Excerpts`；
- 三类摘录继续共享 96KiB 输出预算。

## 6. 契约与版本

行为变更会影响回放结果和行号，因此：

- `EvidenceVersion` 从 2 升到 3；
- 同步修改 `contracts/evidence.schema.json` 和
  `runtime/internal/evidence/evidence.schema.json`；
- Schema 标题与描述改为 v3；
- `inputs.truncated_files` 描述改为“提取过程中因超长单行发生扫描截断的文件”；
- `Attachment.Size` 记录实际消费的完整字节数，不再是尾部窗口大小；
- 不新增必填字段，不改变 Analyzer 输出契约。

旧 evidence snapshot 保留其原始 `evidence_version`，不会被重写。新 Runtime 只产生
v3 snapshot。

## 7. 错误处理

| 情形 | 行为 |
|---|---|
| 正则编译失败 | 对应签名 `error=regex compile...`，其他签名继续 |
| 目标日志缺失 | 对应签名 `error=log missing...` |
| Reader 中途返回错误 | 已完成命中保留；引用该文件但没有可靠结果的签名记录 read error |
| 单行超过 1MiB | 消费完整行；只扫描前 1MiB；文件进入 `truncated_files` |
| 上下文超过 96KiB | 按声明顺序截断，顶层 `truncated=true` |
| 日志为空 | 无命中、无 excerpt，附件 size 为 0 |

读取错误不能造成死循环；自定义 Reader 返回 `(n > 0, err != nil)` 时必须先消费
这批字节，再处理错误。

## 8. 测试

### 8.1 回归测试

- 现有单命中、多命中、头尾边界、非法正则、缺失文件、fallback、JUnit、预算和
  Schema 测试继续通过。
- 原 `TestExtractFileTruncation` 改为验证大文件被完整扫描。

### 8.2 新增测试

1. 构造超过 8MB 的日志，签名只在文件头：必须命中。
2. 同一大文件在头部和尾部各有命中：都必须命中且使用真实全局行号。
3. 命中跨越扫描内部 buffer 边界：上下文前后各 50 行完整。
4. 大文件无签名命中：stdout/stderr 尾部和全文件最早 logcat 错误仍正确。
5. 超过 1MiB 的单行后仍有普通命中：后续命中不能因超长行丢失。
6. 超长行文件进入 `truncated_files`，普通超过 8MB 的文件不进入。
7. `Attachment.Size` 等于完整输入字节数。
8. 多签名共享一个 Reader，只读取一次。
9. Reader 在返回最后一批字节时同时返回错误：这批字节必须被扫描。
10. 使用生成式大日志或计数 Reader 证明扫描到 EOF，候选内存不随文件长度增长。

## 9. 验收标准

- 位于日志任意位置的签名均可命中，不受 8MB 窗口限制。
- `Match.LineNo` 与原文件真实行号一致。
- 普通大文件完整扫描且不标 `truncated_files`。
- evidence 序列化结果仍受 96KiB 上下文预算约束。
- 无完整原始日志进入 evidence 或 Analyzer 请求。
- `go test ./internal/evidence`、`go test ./...` 和全仓 Python 契约测试通过。
- `docs/device-test-sequence.md` 差距 #9 标记为已实现，并说明单行上限。
