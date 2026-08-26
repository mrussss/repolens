# ADR 011: Local Single-User Product Model and Zero-Leakage Secrets

## Status
Accepted and Implemented

## Context
RepoLens is designed as a local developer tool. Multi-tenant SaaS concepts like JWT logins, RBAC roles, and cloud user registration introduce unnecessary barriers to instant developer adoption.

## Decision
1. Eliminate multi-tenant authentication, login walls, and session management.
2. Store API keys locally in secret files with `0600` permissions; never persist keys in frontend storage.
3. Expose single-user local APIs with immediate 1-click Demo Mode out-of-the-box.
