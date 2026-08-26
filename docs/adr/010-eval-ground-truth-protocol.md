# ADR 010: Evaluation Dataset Ground Truth Protocol and Dataset Split

## Status
Accepted and Implemented

## Context
Validating retrieval, agent reasoning, and citation accuracy requires frozen datasets with deterministic ground-truth line ranges and fault symptoms.

## Decision
1. Freeze standard fault cases split into Dev Set (hyperparameter tuning) and Held-out Test Set (promotion gating).
2. Validate ground truth annotations with automated lint checks: line bounds, syntax correctness, and file existence.
3. Test sets are never modified or fitted against during model tuning.
