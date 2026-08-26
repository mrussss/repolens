# ADR 007: Atomic Retrieval Artifact Publishing and Storage Protocol

## Status
Accepted and Implemented

## Context
Writing search index files directly into active production directories can lead to partial reads or corrupted indexes if crashes or cancellations occur during construction.

## Decision
1. Stage all index files in `.tmp/<retrieval_build_id>-<claim_token>/`.
2. Generate index binary, metadata, and `manifest.json`.
3. Compute SHA256 checksums across all written files.
4. Perform atomic directory rename via `os.Rename` to `storage/indexes/<retrieval_build_id>/`.
5. Update `RetrievalBuild` to `READY` within a database transaction.
