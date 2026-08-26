package diagnosis

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

func ComputeRequestHash(repoID, snapshotID, title, description, errorLog string, pinnedBuildIDs ...int64) string {
	var codeIndexBuildID, retrievalBuildID int64
	if len(pinnedBuildIDs) > 0 {
		codeIndexBuildID = pinnedBuildIDs[0]
	}
	if len(pinnedBuildIDs) > 1 {
		retrievalBuildID = pinnedBuildIDs[1]
	}
	raw := fmt.Sprintf("%s|%s|%s|%s|%s|%d|%d",
		strings.TrimSpace(repoID),
		strings.TrimSpace(snapshotID),
		strings.TrimSpace(title),
		strings.TrimSpace(description),
		strings.TrimSpace(errorLog),
		codeIndexBuildID,
		retrievalBuildID,
	)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func IsValidRunTransition(from, to RunStatus) bool {
	switch from {
	case StatusQueued:
		return to == StatusRunning || to == StatusCancelled
	case StatusRunning:
		return to == StatusSucceeded || to == StatusFailed || to == StatusCancelled
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
