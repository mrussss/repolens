package logger

import (
	"context"
	"log/slog"
	"os"
)

type ctxKey string

const (
	RequestIDKey    ctxKey = "request_id"
	UserIDKey       ctxKey = "user_id"
	RepositoryIDKey ctxKey = "repository_id"
	SnapshotIDKey   ctxKey = "snapshot_id"
	DiagnosisIDKey  ctxKey = "diagnosis_id"
	AttemptIDKey    ctxKey = "attempt_id"
)

var defaultLogger *slog.Logger

func Init(env string) {
	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	if env == "development" {
		opts.Level = slog.LevelDebug
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	defaultLogger = slog.New(handler)
	slog.SetDefault(defaultLogger)
}

func L(ctx context.Context) *slog.Logger {
	l := defaultLogger
	if l == nil {
		l = slog.Default()
	}
	if ctx == nil {
		return l
	}

	var attrs []any
	if v := ctx.Value(RequestIDKey); v != nil {
		attrs = append(attrs, slog.Any("request_id", v))
	}
	if v := ctx.Value(UserIDKey); v != nil {
		attrs = append(attrs, slog.Any("user_id", v))
	}
	if v := ctx.Value(RepositoryIDKey); v != nil {
		attrs = append(attrs, slog.Any("repository_id", v))
	}
	if v := ctx.Value(SnapshotIDKey); v != nil {
		attrs = append(attrs, slog.Any("snapshot_id", v))
	}
	if v := ctx.Value(DiagnosisIDKey); v != nil {
		attrs = append(attrs, slog.Any("diagnosis_id", v))
	}
	if v := ctx.Value(AttemptIDKey); v != nil {
		attrs = append(attrs, slog.Any("attempt_id", v))
	}

	if len(attrs) > 0 {
		return l.With(attrs...)
	}
	return l
}
