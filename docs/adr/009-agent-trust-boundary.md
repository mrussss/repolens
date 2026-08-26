# ADR 009: Agent Trust Boundary and Read-Only Tool Authorizations

## Status
Accepted and Implemented

## Context
Code repositories, error logs, and issue descriptions contain untrusted user data that could attempt prompt injections, shell execution, or credential exfiltration.

## Decision
1. Establish rigid policy hierarchy: `Server Policy > Tool Authorization > User Goal > Untrusted Repository Data`.
2. Restrict agent execution strictly to 5 read-only tools: `search_code`, `get_symbol`, `find_references`, `find_related_tests`, and `read_file`.
3. Apply secret redaction and size limits to user inputs and tool responses before sending prompts to LLM providers.
