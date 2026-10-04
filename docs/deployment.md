# Local Deployment

RepoLens is a local, single-user developer tool. The supported bundled setup
uses Docker Compose to run MySQL 8, the API, and the Worker:

```bash
cp .env.example .env
docker compose up --build
```

Open <http://127.0.0.1:8080>. Compose binds the API and MySQL ports to
loopback. The API serves the Web UI. The Worker claims database-backed jobs
and shares repository snapshots and Provider secret storage with the API.

Compose persists MySQL data, snapshots, and Provider secrets in named volumes.
`docker compose down` stops and removes the containers while preserving those
volumes. Removing stored data requires an explicit destructive reset; see the
`make clean-data` target and its warning in the root README.

Configure an OpenAI-compatible Provider from the Setup page. The API key is
stored in the shared local secret volume and is not returned to the browser.
Without a configured key, the deterministic FakeProvider supports the demo.

RepoLens treats repository contents as untrusted input. It does not execute a
repository's build, tests, generators, or package installation. The local
single-user deployment is not intended to be exposed directly to the public
Internet.

The production image includes the same Go toolchain and standard-library sources
used to compile the worker. It does not copy the builder's module or build cache.
The offline importer only admits canonical standard-library paths or packages in
the snapshot's root module; third-party dependencies remain unresolved.
After building the Compose images, verify the actual worker runtime without
network access:

```bash
./scripts/verify_codeintel_runtime.sh
```
