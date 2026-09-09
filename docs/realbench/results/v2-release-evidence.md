# RepoLens v2.1 Final Evidence Release Gate

## Freeze identity

- Final code-under-test commit: `80f73597787393fe9f358380da5eb9c9979a4a1c`
- Final evidence commit: `TBD` (this document is finalized in a docs-only follow-up)
- Working tree: clean at benchmark and gate execution; the user-requested root development MD is local-only and excluded through `.git/info/exclude`.
- Final tag: `v2.1-final-evidence` is created only after the evidence commit is finalized.

The benchmark code, frozen datasets, and docs are kept distinct in the evidence: the v1 and v2 run metadata record the code-under-test commit, while this document records the later docs-only evidence commit.

## Verification matrix

| Gate | Result | Evidence |
|---|---|---|
| `gofmt -l cmd internal tests` | PASS | No files listed |
| `go vet ./...` | PASS | Completed on final code commit |
| `go test ./...` | PASS | Completed on final code commit |
| `go test -race ./...` | PASS | Completed on final code commit |
| Component integration | PASS | `go test -v ./tests/integration/...` |
| Real MySQL integration | PASS | `REPOLENS_REQUIRE_REAL_INTEGRATION=1 go test -v -race ./tests/integration_real/...`; 3 MySQL 8.0 testcontainers cases, 0 skips |
| Web build | PASS | `npm ci && npm run build`; TypeScript and Vite build succeeded |
| Eval | PASS | `go run ./cmd/eval`; 16 held-out cases loaded, production strategy remains Pure Go BM25 |
| Compose config | PASS | `docker compose config` |
| Docker build | PASS | `docker compose build`; API and worker images built |
| Existing release gate | PASS | `./scripts/release_gate.sh`; health and real Demo smoke succeeded |

## Benchmark evidence

- RealBench v1 validation: PASS; frozen manifest hash `5b63f6e3ce1437c2d9e57dbb410530b54eca1f6a64590a29f4f639379768b9bf`.
- RealBench v1 clean-head retrieval: PASS; run `20260909T111454Z-63cd1604`, 3/3 completed, 0 infra errors, 0 product failures, Hit@5 3/3, Hit@10 3/3, MRR 0.778.
- RealBench v2 validation: PASS; frozen manifest hash `a7fa7d404d37b9a5b94c6f448d8bf3c4ad26ee4fad7b2f2ce4b3b442936af1dc`.
- RealBench v2 clean-head retrieval: PASS; run `20260909T113202Z-0b9cfe0c`, 10/10 completed, 0 infra errors, 0 product failures, Hit@5 7/10, Hit@10 9/10, MRR 0.724.
- Provider preflight: PASS with `is_configured=true`, `is_demo=false`; normal completion, tool calling, Agent loop, and structured report all passed.
- Selected Agent E2E evidence: generated for REAL-004, REAL-006, and REAL-013. One case was Correct with valid citations; two cases remain visible as `REPOLENS_PRODUCT` bounded-tool failures and are not converted into Incorrect accuracy scores.

## Scope and release boundary

`realbench-v1` remains a three-case pilot history, and `realbench-v2` remains a ten-case/eight-repository pilot-scale external benchmark. The recorded numbers are reproducible evidence for this clean code commit, not a production accuracy guarantee. No frozen case, input, Ground Truth, manifest hash, retrieval algorithm, prompt-specific case rule, Vector/Embedding/RRF path, or real API secret was added or changed during evidence freeze.
