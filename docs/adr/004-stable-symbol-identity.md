# ADR 004: Stable Symbol Identity and Canonical Receiver Normalization

## Status
Accepted and Implemented

## Context
In Go, method receivers can be value or pointer types (e.g. `(s Service) A()` vs `(s *Service) B()`), and may include generics (`Stack[T]`). Tracking symbols requires a canonical, deterministic identifier.

## Decision
1. Define the Raw Symbol Key format: `module_path|package_path|receiver_canonical|kind|name`.
2. Compute `symbol_key_hash = SHA256(symbol_key_raw)` and persist both raw and hashed representations.
3. Canonicalize method receivers by stripping outer parentheses, pointers (`*`), and generic type parameters (e.g. `(s *Service)` -> `Service`).
