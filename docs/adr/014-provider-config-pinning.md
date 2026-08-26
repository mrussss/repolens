# ADR 014: Deterministic Provider Configuration Pinning and Endpoint Fingerprints

## Status
Accepted and Implemented

## Context
When configuring third-party OpenAI-compatible LLM endpoints, subtle variations in base URLs (trailing slashes, `/v1` paths) or credentials can cause configuration drift and silent failures.

## Decision
1. Normalize Base URLs strictly: trim trailing slashes, ensure `/v1` endpoint consistency, and prohibit non-HTTPS endpoints in production.
2. Compute deterministic SHA256 fingerprints:
   - `endpoint_fingerprint`: SHA256 of normalized Base URL + Model name.
   - `config_fingerprint`: SHA256 of normalized Base URL + Model + API Key.
3. Expose safe endpoint fingerprints in public health status without leaking API keys.
