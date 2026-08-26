# ADR 005: Offline Go Types Importer Boundary and Synthetic Fallback

## Status
Accepted and Implemented

## Context
Standard `go/types` type checking requires downloading external module dependencies from the internet (`go.mod`/GOPROXY). In an offline, secure local developer environment, missing external packages cause type checkers to halt.

## Decision
1. Implement `OfflineImporter` (`internal/codeintel/importer/`):
   - Fully parses and type checks internal repository packages from in-memory ASTs.
   - For external unresolvable packages, generates synthetic empty `types.Package` placeholders to allow type-checking to continue without network access.
