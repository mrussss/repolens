# ADR 004: Stable Symbol Identity and Canonical Receiver Normalization

## Status
Accepted and Implemented

## Context
In Go, method receivers can be value or pointer types (e.g. `(s Service) A()` vs `(s *Service) B()`), and may include generics (`Stack[T]`). Tracking symbols requires a canonical, deterministic identifier.

## Decision
1. Define the Raw Symbol Key format: `module_path|package_path|receiver_canonical|kind|name`.
2. Compute `symbol_key_hash = SHA256(symbol_key_raw)` and persist both raw and hashed representations.
3. Canonicalize method receivers by stripping outer parentheses, pointers (`*`), and generic type parameters (e.g. `(s *Service)` -> `Service`).

4. Legal duplicate `init()` declarations append `|<snapshot-relative file path>:<physical start line>:<physical start column>` to the ordinary key before hashing. Ordinary symbol identities keep their existing format. Relations use the extracted declaration identity, and persistence rejects duplicate hashes atomically.
5. Persisted symbol ranges, relation coordinates and related-test coordinates always use physical snapshot positions (`PositionFor(pos, false)`); Go `//line` directives never change the snapshot coordinate contract.

The correctness hardening uses analyzer `v2.2.3` and symbol schema `v2.1.2`. Parser, retrieval and tokenizer versions remain unchanged. Production retrieval remains BM25; Structural development is not resumed.
