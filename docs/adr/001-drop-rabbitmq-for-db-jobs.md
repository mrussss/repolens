# ADR 001: Drop RabbitMQ & Outbox Relay for Database-Backed Analysis Jobs

## Status
Accepted and Implemented

## Context
RepoLens v1 used an asynchronous architecture with RabbitMQ and an Outbox relay daemon. For a single-tenant local-first repository intelligence and RCA engine, maintaining RabbitMQ containers, outbox table polling loops, exchange/queue bindings, and dead-letter topics created significant operational complexity without adding value.

## Decision
1. Replace RabbitMQ and Outbox Relay with a native Database-backed Job runtime (`internal/jobs/`).
2. Implement concurrent job claiming via `SELECT ... FOR UPDATE SKIP LOCKED` (MySQL 8) and transaction-fenced locks (SQLite3).
3. Enforce deterministic fencing tokens (`execution_generation`), dynamic background lease renewal (`LeaseRenewer`), and stale job reaping (`Reaper`).
4. Support 4 frozen job types: `MATERIALIZE_SNAPSHOT`, `BUILD_CODE_INDEX`, `BUILD_RETRIEVAL`, and `RUN_DIAGNOSIS`.

## Consequences
- Zero external message broker dependency; reduced container count and instant startup.
- Full transactional consistency between business domain entities (Diagnoses/Snapshots/Builds) and their queued execution jobs.
