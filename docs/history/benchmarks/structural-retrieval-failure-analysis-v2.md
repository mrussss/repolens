# Structural Retrieval Go/No-Go Failure Analysis — RealBench v2

## Baseline

- 分析代码 HEAD：`2f098e3e505d8534b524f257f4fc021a7c68d4c6`，本轮开始时与远端 main 一致。
- Paired run：`artifacts/realbench/20261006T090504Z-123b27a3/`，产物记录同一 HEAD。
- Dataset：`realbench-v2`，REAL-004…REAL-013，10/10 完成，infra/product failures 均为 0。
- Dataset hash：`a7fa7d404d37b9a5b94c6f448d8bf3c4ad26ee4fad7b2f2ce4b3b442936af1dc`。
- BM25 与 BM25_STRUCTURAL：Mean Recall@8 均为 0.850000，MRR 均为 0.611667；better/equal/worse=0/10/0。
- Production default 保持 BM25；本报告没有实现或优化 Structural v2。

### Method and fidelity

使用已有 `retrieval_compare.json`、prediction、每 case 的 READY RetrievalBuild artifact 与 `state.sqlite`。
临时 Go scratch 加载 `artifact.LoadIndexVerified`，重新构造当前 `retrieval.BuildQuery` 并断言与保存 query 相同。
使用当前 `idx.Search(query,16)` 获取生产 Top2K pool；实际调用未改动的
`structural.NewEngine(idx, measuredCIStore, CodeIndexBuildID).Search(ctx,query,8)`，其输出
身份、顺序与分数逐项与 paired Top8 对照，10/10 一致。

为了展示未入 Top8 的候选，临时复制当前四个 signal 判断及加分顺序，按 final score、base rank
排序还原完整 Top16；还原的 Top8 再与真实 Engine 校验。
按 signal 类别移除的反事实排序，从 BaseScore 按生产相同顺序重新累加其它 signal，避免浮点相减影响 tie。
反事实仅解释当前排序，不是新策略或正式评测结果。

对 REAL-007/011 同时查看 Top50 和完整正分 BM25 结果的首次 gold 排名；扩大观察窗口只用于分析，
生产 candidate depth、query、TopK 均未改变。SQLite 以 `mode=ro&_query_only=1` 打开，
store wrapper 和 Gorm Trace logger 分别计时/计数真实 ListRelatedTests 与 SQL。
临时 source 最终删除，不提交；诊断性中间数据仅在 `/tmp/repolens-structural-analysis-v2.json`。

指标仍只采用 PrimaryFiles，SupportingFiles 不参与 gold。下文明确区分事实与下一阶段建议。

## Ranking Delta

单位是 Symbol result（path、symbol、起止行）；file set 对路径去重。最早命中每个 PrimaryFile 的 rank 单独比较，
不把同文件多个 Symbol 当作多个 file recall。

- 完全同序：**7/10**（Symbol 顺序与 path 顺序均相同）。
- 同 Symbol 集合、顺序变化：**2/10**，REAL-012、REAL-013。
- Top8 Symbol 集合变化：**1/10**，REAL-009；该 case 的 file set 也变化。
- Top8 path set 相同：**9/10**；ordered paths 相同：**7/10**。
- Top8 共有 Symbol 换位共 **8** 个；新增 Symbol **1** 个，丢失 Symbol **1** 个。
- 新增 file **0** 个，离开 file **1** 个：REAL-009 的 `string_to_string_test.go`，不是 PrimaryFile。
- PrimaryFile 首次 rank 改善 case **0**，恶化 case **0**；已检索的 gold 最早排名及未检索的 gold 状态均未改变。
- 完整 Top16 中 **35/160** 个候选的 final rank 与 base rank 不同。相同聚合质量不能解释为没有执行 rerank。

| Case | Top8 Symbol comparison | 共有 Symbol 换位 | 新增/丢失 files | PrimaryFile 首次 Top8 rank（BM25 → Structural） |
|---|---|---:|---|---|
| REAL-004 | 完全同序 | 0 | +无 / −无 | context.go: 1 → 1 |
| REAL-005 | 完全同序 | 0 | +无 / −无 | text_formatter.go: 3 → 3 |
| REAL-006 | 完全同序 | 0 | +无 / −无 | command_parse.go: 5 → 5 |
| REAL-007 | 完全同序 | 0 | +无 / −无 | help.go: 3 → 3; command_run.go: 不在 Top8 → 不在 Top8 |
| REAL-008 | 完全同序 | 0 | +无 / −无 | flag.go: 1 → 1 |
| REAL-009 | 集合变化 | 2 | +无 / −string_to_string_test.go | string_to_string.go: 1 → 1 |
| REAL-010 | 完全同序 | 0 | +无 / −无 | backend_inotify.go: 4 → 4 |
| REAL-011 | 完全同序 | 0 | +无 / −无 | internal/transport/http2_server.go: 不在 Top8 → 不在 Top8 |
| REAL-012 | 同集合，顺序变化 | 4 | +无 / −无 | redis.go: 1 → 1 |
| REAL-013 | 同集合，顺序变化 | 2 | +无 / −无 | redirect.go: 1 → 1 |

### Top8 ordered paths / symbols

保留重复路径，直接展示真正的 Symbol 排名；完整 score/reason 仍见原 paired report。

<details>
<summary>REAL-004 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | context.go · SaveUploadedFile | context.go · SaveUploadedFile |
| 2 | context_test.go · TestSaveUploadedOpenFailed | context_test.go · TestSaveUploadedOpenFailed |
| 3 | context_test.go · TestContextMultipartForm | context_test.go · TestContextMultipartForm |
| 4 | context_test.go · TestContextFormFile | context_test.go · TestContextFormFile |
| 5 | context_test.go · TestSaveUploadedCreateFailed | context_test.go · TestSaveUploadedCreateFailed |
| 6 | context_test.go · TestSaveUploadedFileWithPermissionFailed | context_test.go · TestSaveUploadedFileWithPermissionFailed |
| 7 | context_test.go · TestSaveUploadedFileWithPermission | context_test.go · TestSaveUploadedFileWithPermission |
| 8 | path.go · cleanPath | path.go · cleanPath |

</details>

<details>
<summary>REAL-005 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | formatter_bench_test.go · BenchmarkStringerTextFormatter | formatter_bench_test.go · BenchmarkStringerTextFormatter |
| 2 | text_formatter_test.go · TestNewlineBehavior | text_formatter_test.go · TestNewlineBehavior |
| 3 | text_formatter.go · appendValue | text_formatter.go · appendValue |
| 4 | text_formatter_test.go · TestTextEntryFieldValueError | text_formatter_test.go · TestTextEntryFieldValueError |
| 5 | example_default_field_value_test.go · ExampleDefaultFieldHook | example_default_field_value_test.go · ExampleDefaultFieldHook |
| 6 | text_formatter_test.go · TestPadLevelText | text_formatter_test.go · TestPadLevelText |
| 7 | text_formatter.go · appendKeyValue | text_formatter.go · appendKeyValue |
| 8 | text_formatter_test.go · TestDisableTimestampWithColoredOutput | text_formatter_test.go · TestDisableTimestampWithColoredOutput |

</details>

<details>
<summary>REAL-006 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | args.go · Argument | args.go · Argument |
| 2 | args.go · Args | args.go · Args |
| 3 | funcs.go · ArgValidatorFunc | funcs.go · ArgValidatorFunc |
| 4 | flag.go · RequiredFlag | flag.go · RequiredFlag |
| 5 | command_parse.go · flagFromError | command_parse.go · flagFromError |
| 6 | flag.go · SchemaItemsTyper | flag.go · SchemaItemsTyper |
| 7 | fish.go · ToFishCompletion | fish.go · ToFishCompletion |
| 8 | flag.go · Flag | flag.go · Flag |

</details>

<details>
<summary>REAL-007 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | funcs.go · ShellCompleteFunc | funcs.go · ShellCompleteFunc |
| 2 | funcs.go · AfterFunc | funcs.go · AfterFunc |
| 3 | help.go · checkShellCompleteFlag | help.go · checkShellCompleteFlag |
| 4 | completion_test.go · TestCompletionBashGreedyColonParsing | completion_test.go · TestCompletionBashGreedyColonParsing |
| 5 | help_test.go · Test_checkShellCompleteFlag | help_test.go · Test_checkShellCompleteFlag |
| 6 | completion_test.go · TestCompletionSubcommand | completion_test.go · TestCompletionSubcommand |
| 7 | command_stop_on_nth_arg_test.go · TestCommand_StopOnNthArg | command_stop_on_nth_arg_test.go · TestCommand_StopOnNthArg |
| 8 | completion_test.go · TestCompletionBashAppendsSpace | completion_test.go · TestCompletionBashAppendsSpace |

</details>

<details>
<summary>REAL-008 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | flag.go · Output | flag.go · Output |
| 2 | flag.go · SetOutput | flag.go · SetOutput |
| 3 | flag.go · FlagSet | flag.go · FlagSet |
| 4 | flag.go · defaultUsage | flag.go · defaultUsage |
| 5 | flag.go · usage | flag.go · usage |
| 6 | flag.go · PrintDefaults | flag.go · PrintDefaults |
| 7 | flag_test.go · parseReturnStderr | flag_test.go · parseReturnStderr |
| 8 | count.go · Count | count.go · Count |

</details>

<details>
<summary>REAL-009 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | string_to_string.go · StringToString | string_to_string.go · StringToString |
| 2 | example_test.go · ExampleFlagSet_StringToString | string_to_string.go · StringToString |
| 3 | string_to_string.go · StringToString | example_test.go · ExampleFlagSet_StringToString |
| 4 | string_to_string.go · StringToStringVar | string_to_string.go · StringToStringVar |
| 5 | string_to_string.go · StringToStringVar | string_to_string.go · StringToStringVar |
| 6 | string_to_string.go · StringToStringVarP | string_to_string.go · StringToStringVarP |
| 7 | string_to_string.go · StringToStringVarP | string_to_string.go · StringToStringVarP |
| 8 | string_to_string_test.go · TestS2SStablePointers | string_to_string.go · GetStringToString |

</details>

<details>
<summary>REAL-010 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | fsnotify.go · WatchList | fsnotify.go · WatchList |
| 2 | fsnotify.go · Event | fsnotify.go · Event |
| 3 | backend_inotify_test.go · TestRenameRecursiveDoesNotRenameSiblingPrefixWatch | backend_inotify_test.go · TestRenameRecursiveDoesNotRenameSiblingPrefixWatch |
| 4 | backend_inotify.go · WatchList | backend_inotify.go · WatchList |
| 5 | backend_inotify_test.go · TestRemoveRecursiveDoesNotRemoveSiblingPrefixWatch | backend_inotify_test.go · TestRemoveRecursiveDoesNotRemoveSiblingPrefixWatch |
| 6 | fsnotify.go · Add | fsnotify.go · Add |
| 7 | fsnotify.go · backend | fsnotify.go · backend |
| 8 | fsnotify.go · Remove | fsnotify.go · Remove |

</details>

<details>
<summary>REAL-011 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | test/end2end_test.go · testClientInitialHeaderEndStream | test/end2end_test.go · testClientInitialHeaderEndStream |
| 2 | test/end2end_test.go · testClientSendDataAfterCloseSend | test/end2end_test.go · testClientSendDataAfterCloseSend |
| 3 | test/end2end_test.go · TestRecvWhileReturningStatus | test/end2end_test.go · TestRecvWhileReturningStatus |
| 4 | test/gracefulstop_test.go · TestGracefulStopClosesConnAfterLastStream | test/gracefulstop_test.go · TestGracefulStopClosesConnAfterLastStream |
| 5 | stats/stats.go · InPayload | stats/stats.go · InPayload |
| 6 | test/servertester.go · greet | test/servertester.go · greet |
| 7 | internal/transport/transport_test.go · TestClientSendsRSTStream_ReadUnreadData | internal/transport/transport_test.go · TestClientSendsRSTStream_ReadUnreadData |
| 8 | stats/stats.go · OutPayload | stats/stats.go · OutPayload |

</details>

<details>
<summary>REAL-012 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | redis.go · enableMaintNotificationsUpgrades | redis.go · enableMaintNotificationsUpgrades |
| 2 | csc_test.go · TestInvalidateHandlerReleasedOnClose | redis.go · createInitConnFunc |
| 3 | redis.go · createInitConnFunc | csc_test.go · TestInvalidateHandlerReleasedOnClose |
| 4 | internal/pool/conn_close_hooks_test.go · TestConn_GetNetConnSurvivesClose | internal/pool/conn_close_hooks_test.go · TestConn_GetNetConnSurvivesClose |
| 5 | autopipeline_test.go · panicPipelineHook | csc_integration.go · fulfillCached |
| 6 | csc_integration.go · fulfillCached | autopipeline_test.go · panicPipelineHook |
| 7 | autopipeline_internal_test.go · TestDivertRegistrationRacesClose | autopipeline_internal_test.go · TestDivertRegistrationRacesClose |
| 8 | autopipeline.go · Do | autopipeline.go · Do |

</details>

<details>
<summary>REAL-013 Top8</summary>

| Rank | BM25 path · symbol | Structural path · symbol |
|---:|---|---|
| 1 | redirect.go · Back | redirect.go · Back |
| 2 | redirect_test.go · Test_Redirect_Back_WithReferer | redirect_test.go · Test_Redirect_Back_WithReferer |
| 3 | res.go · Redirect | res.go · Redirect |
| 4 | redirect_test.go · Test_Redirect_Back | ctx.go · Redirect |
| 5 | ctx.go · Redirect | redirect_test.go · Test_Redirect_Back |
| 6 | extractors/extractors.go · FromForm | extractors/extractors.go · FromForm |
| 7 | redirect.go · To | redirect.go · To |
| 8 | client/request.go · Referer | client/request.go · Referer |

</details>

## Signal Activation

统计窗口为 10 case × 16 candidates = 160；按实际入 pool 的候选计数，不把整个 corpus 当分母。
“触发且换位”只表示该候选综合 final rank 不同，不将它全部归因给该单一 signal；
“移除类别改变排序”来自逐类反事实，对整个 Top16 的因果对照，不等于质量改善。

| Signal | Pool 触发候选 | 触发 case | 触发且综合换位候选 | 触发的 base Top8 / final Top8 | 移除类别改变 Top16 的 case |
|---|---:|---:|---:|---:|---:|
| EXACT_SYMBOL_MATCH | 0 | 0 | 0 | 0 / 0 | 0 |
| SUBSTRING_SYMBOL_MATCH | 26 | 8 | 9 | 14 / 14 | 5 |
| RELATED_TEST_DISCOVERY | 98 | 10 | 19 | 47 / 48 | 7 |
| TEST_CONTEXT_MATCH | 19 | 2 | 6 | 12 / 12 | 1 |

| Case | Exact | Substring | Related-test | Test-context | Full16 换位候选 |
|---|---:|---:|---:|---:|---:|
| REAL-004 | 0 | 3 | 7 | 8 | 6 |
| REAL-005 | 0 | 2 | 5 | 11 | 6 |
| REAL-006 | 0 | 1 | 14 | 0 | 2 |
| REAL-007 | 0 | 0 | 5 | 0 | 0 |
| REAL-008 | 0 | 5 | 13 | 0 | 2 |
| REAL-009 | 0 | 2 | 12 | 0 | 4 |
| REAL-010 | 0 | 5 | 12 | 0 | 2 |
| REAL-011 | 0 | 0 | 6 | 0 | 2 |
| REAL-012 | 0 | 3 | 11 | 0 | 7 |
| REAL-013 | 0 | 5 | 13 | 0 | 4 |

**事实与解释：**

- Exact 在这 10 个组合 query 中完全没触发；代码比较整个 query 与 SymbolName，不是抽取 query 中的单个符号。不能推断它对所有 query 都永远无效。
- Related-test 确实工作：98/160 个候选加 0.3，所有 case 至少一次。它只检查非空关系集合，不区分这些 tests 对当前 issue 的相关度、数量或 confidence。
- 许多候选同时拿相同分：REAL-006 为 14/16，REAL-008 为 13/16；广泛的“有测试”很难区分同一个故障需要哪个 symbol。
- 47/98 个 related-test 命中原本就在 base Top8；其 final Top8 数仅为 48。Substring 的 base/final Top8 都为 14；test-context 都为 12。boost 常作用于已经靠前的候选。
- REAL-007 的 5 次 related-test boost 没改变完整 Top16 顺序。REAL-011 的 related-test boost 改变第 12/13 位，Top8 和 gold 仍无改善。
- REAL-009/012/013 的 Top8 真正变化，但没有改善 gold 的首次 rank。当前是“部分 signal 改变排序、没有改变正式质量指标”，不是“signals 没运行”。
- 移除某个 signal 类别也会改变部分 rank；不能据此把其质量作用宣称为独立贡献，更不能用这些反事实调参。

## REAL-007

Query（与生产 BuildQuery 一致）：

> Shell completion after -- executes the command action When a shell completion request contains the double dash separator the command should stop suggesting flags and should never execute the command action The completion request unexpectedly ran command action after

PrimaryFiles：`help.go`, `command_run.go`。

| BM25 rank | Structural rank in Top16 | Path | Symbol | Base score | Boost / reason | Final score |
|---:|---:|---|---|---:|---|---:|
| 1 | 1 | funcs.go | ShellCompleteFunc | 28.701609 | RELATED_TEST_DISCOVERY=+0.3 | 29.001609 |
| 2 | 2 | funcs.go | AfterFunc | 24.485142 | RELATED_TEST_DISCOVERY=+0.3 | 24.785142 |
| 3 | 3 | help.go | checkShellCompleteFlag | 22.381127 | RELATED_TEST_DISCOVERY=+0.3 | 22.681127 |
| 4 | 4 | completion_test.go | TestCompletionBashGreedyColonParsing | 22.092984 | none | 22.092984 |
| 5 | 5 | help_test.go | Test_checkShellCompleteFlag | 21.029189 | none | 21.029189 |
| 6 | 6 | completion_test.go | TestCompletionSubcommand | 18.401961 | none | 18.401961 |
| 7 | 7 | command_stop_on_nth_arg_test.go | TestCommand_StopOnNthArg | 18.312714 | none | 18.312714 |
| 8 | 8 | completion_test.go | TestCompletionBashAppendsSpace | 17.271553 | none | 17.271553 |
| 9 | 9 | funcs.go | ActionFunc | 16.694130 | RELATED_TEST_DISCOVERY=+0.3 | 16.994130 |
| 10 | 10 | funcs.go | ConfigureShellCompletionCommand | 16.368754 | RELATED_TEST_DISCOVERY=+0.3 | 16.668754 |
| 11 | 11 | completion_test.go | TestCompletionFishOmitsPositionalTokenFromDynamicCompletion | 16.072051 | none | 16.072051 |
| 12 | 12 | examples_test.go | ExampleCommand_Run_shellComplete_bash | 15.480283 | none | 15.480283 |
| 13 | 13 | examples_test.go | ExampleCommand_Run_shellComplete_zsh | 15.416148 | none | 15.416148 |
| 14 | 14 | examples_test.go | ExampleCommand_Run_shellComplete_fish | 15.416148 | none | 15.416148 |
| 15 | 15 | command_test.go | TestVersionFlagWorksWhenAliasYieldedToUserFlag | 15.230387 | none | 15.230387 |
| 16 | 16 | completion_test.go | TestCompletionSubcommandOrder | 14.639965 | none | 14.639965 |

**结论：CANDIDATE_GENERATION_PROBLEM，针对缺失的 PrimaryFile。**

- `help.go` 已在 base rank 3（checkShellCompleteFlag）；+0.3 related-test 后仍为 rank 3。
- 缺失的 `command_run.go` 不在 Top16，首次出现 **rank 38**（Command.Run），因此也不在 Top32、但在 Top50。
- 缺失文件并非没有索引：它有 **8** 个 documents；help.go 有 31 个。
- 当前 Structural 没有机会对 rank 38 执行 signals，更不存在其当前 Top16 的 final rank；不能将“未入 pool”误报为 boost 不够。
- 无论如何修改 Top16 内部排序，缺失 file 都不可能进入 Top8。这不是当前 rerank 可解决的失败。

**已存在的一跳证据（只查库，不执行 expansion）：**

- base rank 7 的 `TestCommand_StopOnNthArg` → `Command.Run`：CALL_CANDIDATE，SEMANTIC，confidence=1，SEMANTIC_METHOD_SELECTION。
- base rank 6 的 `TestCompletionSubcommand`、rank 12/13/14 的 shell completion examples 也直接调用同一 Command.Run。
- 同一实现 → 这些 test 的 DIRECT_SEMANTIC_USAGE TEST_RELATION 也已保存，可沿反向关系取回实现。
- `Command.run` → seed `checkShellCompleteFlag` 是另一条已解析调用；这里需要明确方向，不能把反向 caller 当作已经进行了正向 expansion。
- 查询到 15 条 pool↔gold 的 resolved/confidence≥0.8 一跳边，其中还包含已召回 help.go 的边与正反重复证据，**不是 15 个新增候选**。

这是值得一个小型 expansion 实验的具体资产，但只证明候选可达，未证明扩展后一定能排入 Top8。

## REAL-011

Query（与生产 BuildQuery 一致）：

> HTTP/2 server Recv hangs when initial HEADERS has END_STREAM A client can send an initial HTTP 2 HEADERS frame with END STREAM and no data frames The server side streaming handler should observe stream.Recv timed out after initial HEADERS END_STREAM without a DATA frame

PrimaryFiles：`internal/transport/http2_server.go`。

| BM25 rank | Structural rank in Top16 | Path | Symbol | Base score | Boost / reason | Final score |
|---:|---:|---|---|---:|---|---:|
| 1 | 1 | test/end2end_test.go | testClientInitialHeaderEndStream | 54.667591 | none | 54.667591 |
| 2 | 2 | test/end2end_test.go | testClientSendDataAfterCloseSend | 50.227955 | none | 50.227955 |
| 3 | 3 | test/end2end_test.go | TestRecvWhileReturningStatus | 45.116677 | none | 45.116677 |
| 4 | 4 | test/gracefulstop_test.go | TestGracefulStopClosesConnAfterLastStream | 45.007603 | none | 45.007603 |
| 5 | 5 | stats/stats.go | InPayload | 44.511558 | RELATED_TEST_DISCOVERY=+0.3 | 44.811558 |
| 6 | 6 | test/servertester.go | greet | 42.889408 | RELATED_TEST_DISCOVERY=+0.3 | 43.189408 |
| 7 | 7 | internal/transport/transport_test.go | TestClientSendsRSTStream_ReadUnreadData | 42.655478 | none | 42.655478 |
| 8 | 8 | stats/stats.go | OutPayload | 41.079616 | RELATED_TEST_DISCOVERY=+0.3 | 41.379616 |
| 9 | 9 | internal/transport/transport_test.go | TestServerSendsResetStreamOnEarlyTrailer | 40.493739 | none | 40.493739 |
| 10 | 10 | internal/transport/transport_test.go | TestClientSendsRSTStream_InTrailers | 39.823420 | none | 39.823420 |
| 11 | 11 | internal/channelz/socket.go | SocketMetrics | 39.118012 | RELATED_TEST_DISCOVERY=+0.3 | 39.418012 |
| 12 | 13 | internal/transport/transport_test.go | TestClientSendsRSTStream_InHeaders | 38.349992 | none | 38.349992 |
| 13 | 12 | stream.go | ClientStream | 38.296520 | RELATED_TEST_DISCOVERY=+0.3 | 38.596520 |
| 14 | 14 | test/end2end_test.go | testClientRequestBodyErrorUnexpectedEOF | 37.970559 | none | 37.970559 |
| 15 | 15 | internal/mem/buffer_pool.go | sizedBufferPool | 37.333968 | RELATED_TEST_DISCOVERY=+0.3 | 37.633968 |
| 16 | 16 | internal/transport/transport_test.go | TestClientTransport_Handle1xxHeaders | 37.220664 | none | 37.220664 |

**结论：CANDIDATE_GENERATION_PROBLEM。**

- `internal/transport/http2_server.go` 在 Top16、Top32、Top50 均不存在；完整正分 BM25 查询首次出现 **rank 101**。
- 该文件有 **37** 个 index documents，说明这是候选截断/竞争问题，不是文件缺失于 corpus。
- 对 gold 没有 pool 内 Structural signal 或 final rank；原始 Top8 Recall/RR 均为 0，任何只重排当前 Top16 的方法都无法命中。
- 现有 pool 中 rank 1 的 testClientInitialHeaderEndStream 与 issue 词面很近，但代码不会沿 test 关系或 call/reference 引入实现；它只给已有候选加分。
- pool 中六个非空 related-test 候选都加 0.3；只把 ClientStream 从 13 提到 12，未影响 Top8。

**一跳覆盖边界：**

只发现 1 条 SEMANTIC/confidence=1 的 pool↔gold 边：
`http2_server.go:NewServerTransport` → `internal/channelz/socket.go:SocketMetrics`，REFERENCE / SEMANTIC_TYPE_OR_SYMBOL_REF。
SocketMetrics 是 base rank 11 的 seed，此边需要反向走；它指向 transport 构造函数，不是“初始 HEADERS END_STREAM”的准确故障定位证据。
高 resolution confidence 表示引用被解析，不等于它对 issue 高相关。
因此不能承诺最小 call/test expansion 能救 REAL-011，也不应为了救它把 generic type/reference 反向 fan-out 全打开。

## Latency Root Cause

原 paired run 包含冷 artifact 加载：BM25 p50/p95=52.488180/563.035758 ms；
Structural=518.672797/2705.312281 ms。下表取最终带 store 与 SQL logger 的分析工具的一次完整诊断性 Engine 测量（工具调试时有前置测量），
不是替换上述 frozen latency，也不是受控性能 benchmark。

每个 case 实际 pool 都为 16，SymbolKeyHash 均非空。真实 wrapper 观察 **16 次 ListRelatedTests**；
Gorm Trace 独立记录 **16 条 SQL**：即一候选一 SELECT，10 个 case 共 **160 次调用/160 条查询**。
store 时间包含 SQL 与 relation materialization；它不是仅 DB 内部执行时间。

| Case | Candidates | Store calls / SQL | Related-test 命中候选 | Store 总耗时 ms | Engine ms | Store 占比 | ≥200ms SQL 数 |
|---|---:|---:|---:|---:|---:|---:|---:|
| REAL-004 | 16 | 16 / 16 | 7 | 420.581 | 420.981 | 99.90% | 0 |
| REAL-005 | 16 | 16 / 16 | 5 | 18.520 | 18.683 | 99.13% | 0 |
| REAL-006 | 16 | 16 / 16 | 14 | 653.321 | 653.706 | 99.94% | 0 |
| REAL-007 | 16 | 16 / 16 | 5 | 597.745 | 598.045 | 99.95% | 0 |
| REAL-008 | 16 | 16 / 16 | 13 | 367.198 | 367.622 | 99.88% | 0 |
| REAL-009 | 16 | 16 / 16 | 12 | 360.460 | 360.889 | 99.88% | 0 |
| REAL-010 | 16 | 16 / 16 | 12 | 11.240 | 11.322 | 99.27% | 0 |
| REAL-011 | 16 | 16 / 16 | 6 | 1383.447 | 1388.034 | 99.67% | 0 |
| REAL-012 | 16 | 16 / 16 | 11 | 2939.958 | 2941.456 | 99.95% | 1 |
| REAL-013 | 16 | 16 / 16 | 13 | 1938.774 | 1940.345 | 99.92% | 1 |

- 最主要的额外成本是逐 candidate 的 CodeIntel store 查询与关系对象读取；本次其占 Engine 时间约 99.1%–99.95%。不是只看总 latency 的猜测。
- 本次真实 SQL 有两条超过默认 SLOW SQL 阈值 200ms，位于 REAL-012、REAL-013；机器/缓存条件变化会改变日志是否出现。
- REAL-011 共有 717,493 条 relations（其中 TEST_RELATION 625,002）；EXPLAIN QUERY PLAN 显示使用 `ix_rel_build(code_index_build_id)`，然后过滤 relation_type/from_symbol_key_hash。它提供扫描成本的解释，不表示本轮已经更改 index。
- REAL-007 的 TEST_RELATION 为 318,974 条；“有相关测试”会返回很多关系，但 boost 仍只取存在性。
- 本轮没有 batch query、cache、索引或 SQL 优化；质量价值成立之前不做该成本投入。

## Decision

```text
EXPANSION_V2
```

**工程决定：值得再做一次有边界、有退出标准的最小一跳候选扩展实验。**
不继续当前 flat boost rerank 的权重调参，不将 Structural 晋升 production。

## Why

1. 两个未完全召回 case 的缺失 gold 均在 Top16 外（38、101），只改 rerank 的路线在这两例上理论上无解。
2. REAL-007 的已有 Top16 seeds 已有 SEMANTIC/confidence=1 的直接调用和 direct-test 关系连接到缺失 Command.Run；机会来自真实既有 CodeIntel，不依赖新增技术栈。
3. 当前 35/160 个 pool 候选及 3 个 Top8 case 确有排名变化，却没有 gold rank 或 Recall/MRR 改善；继续 flat boost 调参缺少价值证据。
4. 98/160 的 related-test existence boost 区分度有限，同时承担 160 次数据库查询；先证明新候选的质量价值，再考虑性能工作。
5. REAL-011 只有 generic reference 的反向可达性，不能当作可救回证明。这限制下一轮 scope，而不是为它增加图深度或复杂图算法。
6. BM25 已完整覆盖 8/10 case。有限 expansion 的收益上限、回归和成本应明确；这 10 例不够证明泛化或 production promotion。

## Next Scope

下一轮仅允许 **BM25 seed → 已有一跳 resolved、高置信 call/direct-test 关系 → 小量新增候选 → deterministic rerank**。
保留 BM25 baseline、build pinning、同 Snapshot corpus、BuildQuery 和 TopK=8。

- 优先限定已解析 semantic direct call / direct-test 边，明确 caller/callee 或 test/implementation 方向；按 SymbolKey 去重并限定 fan-out/总额外候选预算。
- 不把 confidence 当 query relevance，不直接采用泛化 type/reference 的反向邻居；不绕过排名把“可达 gold”当作命中。
- 先在 dev 任务证明新增召回且无原有 gold rank 回归，再对未见任务作独立对照。此报告已经审阅这 10 个 case 的 gold，它们只适合 diagnostic/dev sanity，不能再冒充未见 promotion held-out。
- 如果受限 expansion 没有新增召回、只靠已观察 gold 调参，或成本与回归不值得，退出并保留 BM25；不升级图深度或技术栈来追逐这两个 case。
- 本轮不实现 expansion，不调整 boost/depth，不做 PageRank、Graph DB、Embedding、Vector DB、RRF、RepoMap 等，不进行 production promotion。
