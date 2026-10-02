# RepoLens RealBench v2 Agent E2E Evidence

> This is selected-case evidence from a small external benchmark. It is not a production accuracy or broad real-world generalization claim.

## Fixed configuration and preflight

- Provider: `AIHubMix`
- Model: `coding-glm-5.3-flash`
- Authentication: Bearer
- Temperature: `0.1`
- Provider base URL fingerprint: `da0d2014ca3dc3501c63eef452f5c93acc1eca8a82fbd4cd14d227ca8c230eda`
- Provider timeout: `180s`
- Prompt / Agent version: `v2.1` / `v2.1`
- Agent config hash: `e51fe71196918bc463f196b5015ae50a08014d528ba40b5ac519615086f8e5d3`
- Max tool calls / tool budget: `12` / `12`
- Retrieval / index version: `v2.1.0` / `v2.1.0`
- Reasoning effort: not requested
- Response format: prompt JSON contract

Every selected run recorded `is_configured=true`, `is_demo=false`, and preflight `PASS` for normal completion, tool-calling round trip, minimal Agent loop, and structured report. The API key was read from the local secret file and is not present in Git, docs, artifacts, trace, or command output.

## Selected cases

The selected set is three cases, one run each. This is a first E2E evidence pass, not a repeated-run stability estimate.

| Case | Run ID | E2E status | Root-cause grade | Citation valid / total | Unsupported claims | Tools / rounds | Latency | Tokens | Failure classification |
|---|---|---|---|---:|---:|---|---:|---:|---|
| REAL-004 | `20260909T111515Z-ab7bdba6` | `E2E_COMPLETED` | Correct | 4 / 4 (100%) | 0 | 5 / 4 | 116,359 ms | 14,834 in + 5,714 out = 20,548 | — |
| REAL-006 | `20260909T111814Z-16512aaa` | `E2E_FAILURE` | Not Scorable | 0 / 0 | 0 | 12 / 8 | 104,672 ms | cached/reasoning `NOT_REPORTED` | `REPOLENS_PRODUCT` |
| REAL-013 | `20260909T112054Z-03ffdebd` | `E2E_FAILURE` | Not Scorable | 0 / 0 | 0 | 12 / 8 | 67,263 ms | cached/reasoning `NOT_REPORTED` | `REPOLENS_PRODUCT` |

REAL-004 Provider usage reported cached tokens `7,104` and reasoning tokens `3,702`. The two failed cases did not produce a final Provider response, so input/output totals and cached/reasoning fields are recorded as unavailable rather than as zero. API pricing was not supplied through `REPOLENS_REALBENCH_INPUT_PRICE_PER_MILLION` and `REPOLENS_REALBENCH_OUTPUT_PRICE_PER_MILLION`; each artifact therefore records `cost_status=NOT_CONFIGURED` and no invented dollar amount.

The complete per-case evidence is in each ignored run directory under `artifacts/realbench/`, including `preflight.json`, `e2e_metrics.json`, `citation_result.json`, `agent_trace.json`, and `e2e_attempts.json`.

## Rubrics

Root-cause grading is a documented semi-automatic rubric applied only after the Agent prediction is persisted and evaluator-only Ground Truth is loaded: `Correct` requires substantial expected-cause token overlap and a citation to a primary file; `Partial` means either the correct area or a meaningful part of the mechanism is present; `Incorrect` means neither is supported; `Not Scorable` is used for timeout, tool limit, parse/infra failure, or no final diagnosis. Unsupported claims count findings without citations. Citation validity uses the existing file/line/excerpt validator.

## REAL-002 tool-limit analysis

The frozen v1 `REAL-002` history remains unchanged and still records the earlier bounded 12-tool-call product failure. The final runner now preserves trace evidence for this failure mode and does not increase the limit. The current final-code trace from the equivalent bounded failure in v2 `REAL-006` records 12 executed calls: 7 `search_code`, 4 `read_file`, and 1 `get_symbol`; the attempted next `find_references` call is rejected by the guard before execution. The sequence contains several progressively different searches and repeated file reads, while the v1 Retrieval baseline already places `completions.go` at rank 3 for REAL-002. There is no final answer after the 12th call.

Conclusion for the current evidence: this is an Agent policy/model-budget interaction (`D. Model capability`, with inefficient search planning), not evidence that the frozen Retrieval result is absent. It is retained as `REPOLENS_PRODUCT`; the budget remains 12 and no case-specific prompt or retrieval boost was added.

## Timeout and retry policy

Provider/network/429/5xx/timeout errors are classified as `EXTERNAL_INFRA`; Agent runtime, citation, and RepoLens logic errors are `REPOLENS_PRODUCT`. External errors may retry at most three attempts, with every attempt retained in `e2e_attempts.json`; product failures are not retried. The frozen v1 `REAL-003` timeout remains documented as external infrastructure evidence in [`v1-agent-e2e.md`](v1-agent-e2e.md), not as a Retrieval accuracy failure.
