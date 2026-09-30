//go:build cgo

package analysispipeline

import (
	"errors"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func isSQLiteWriterContention(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrBusy {
		return true
	}
	return isSQLiteBusyMessage(err)
}

func isSQLiteBusyMessage(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy")
}
