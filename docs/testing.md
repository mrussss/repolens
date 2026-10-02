# Testing and Validation

## Authoritative validation

Development validation proceeds through focused validation, targeted
regression tests, the Release Gate, independent audit, and freeze.

Run the complete release validation from the repository root:

```bash
./scripts/release_gate.sh
```

The gate runs formatting and `go vet`, Go unit and race suites, component
integration, required real-MySQL integration, the real-MySQL Golden Path,
Web build and tests, eval, Compose configuration and image build, and a
Compose-backed product smoke. Real MySQL is required in this gate; integration
tests run in required mode with zero skips.

Focused checks and targeted regression tests are useful during development.
They do not replace the release gate.

## Continuous integration

`.github/workflows/ci.yml` runs on pushes and pull requests. It covers Web
tests/build, Go formatting/vet/tests/race tests, component integration,
required real-MySQL integration, the required Golden Path, offline eval, and
Compose configuration/image build. The workflow does not run the product
service smoke; the release gate does.

## RealBench

The CLI usage guide is in [`realbench/README.md`](realbench/README.md). Frozen
benchmark results and audit records are archived under
[`history/benchmarks/`](history/benchmarks/).
