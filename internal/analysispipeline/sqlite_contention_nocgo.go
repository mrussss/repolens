//go:build !cgo

package analysispipeline

import "strings"

func isSQLiteWriterContention(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy")
}
