package diagnosis

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

func ComputeRequestHash(repoID, snapshotID, title, description, errorLog string) string {
	raw := fmt.Sprintf("%s|%s|%s|%s|%s",
		strings.TrimSpace(repoID),
		strings.TrimSpace(snapshotID),
		strings.TrimSpace(title),
		strings.TrimSpace(description),
		strings.TrimSpace(errorLog),
	)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func IsValidRunTransition(from, to RunStatus) bool {
	switch from {
	case StatusQueued:
		return to == StatusRunning || to == StatusCancelRequested || to == StatusCancelled
	case StatusRunning:
		return to == StatusSucceeded || to == StatusRetryWait || to == StatusFailed || to == StatusCancelRequested || to == StatusCancelled
	case StatusRetryWait:
		return to == StatusRunning || to == StatusCancelRequested || to == StatusCancelled || to == StatusFailed
	case StatusCancelRequested:
		return to == StatusCancelled || to == StatusFailed
	default:
		return false
	}
}

func IsValidAttemptTransition(from, to AttemptStatus) bool {
	if from != AttemptStatusRunning {
		return false
	}
	return to == AttemptStatusSucceeded ||
		to == AttemptStatusFailedRetryable ||
		to == AttemptStatusFailedTerminal ||
		to == AttemptStatusCancelled ||
		to == AttemptStatusAbandoned
}
