package jobs

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// CategorizedError allows errors to explicitly declare their ErrorClass and error code.
type CategorizedError struct {
	Class   ErrorClass
	Code    string
	Message string
	Cause   error
}

func (e *CategorizedError) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

func (e *CategorizedError) Unwrap() error {
	return e.Cause
}

// NewPermanentError creates an explicitly permanent error.
func NewPermanentError(code, message string, cause error) error {
	return &CategorizedError{
		Class:   ErrorClassPermanent,
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// NewRetryableError creates an explicitly retryable error.
func NewRetryableError(code, message string, cause error) error {
	return &CategorizedError{
		Class:   ErrorClassRetryable,
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// ClassifyError inspects an error and returns its ErrorClass and an error code.
func ClassifyError(err error) (ErrorClass, string) {
	if err == nil {
		return "", ""
	}

	if errors.Is(err, ErrOwnershipLost) {
		return ErrorClassOwnershipLost, "OWNERSHIP_LOST"
	}
	if errors.Is(err, ErrCancellationRequested) {
		return ErrorClassCancelled, "CANCELLATION_REQUESTED"
	}

	if errors.Is(err, context.Canceled) {
		return ErrorClassCancelled, "CONTEXT_CANCELED"
	}

	var catErr *CategorizedError
	if errors.As(err, &catErr) {
		return catErr.Class, catErr.Code
	}

	errStr := strings.ToLower(err.Error())

	// Cancellation checks
	if strings.Contains(errStr, "cancel") {
		return ErrorClassCancelled, "OPERATION_CANCELLED"
	}

	// Permanent checks
	if strings.Contains(errStr, "invalid") ||
		strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "unsupported") ||
		strings.Contains(errStr, "malformed") ||
		strings.Contains(errStr, "ssrf") ||
		strings.Contains(errStr, "forbidden") {
		return ErrorClassPermanent, "PERMANENT_VALIDATION_ERROR"
	}

	// Retryable checks (HTTP 429, 5xx, timeouts, connection resets)
	if strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "reset by peer") ||
		strings.Contains(errStr, "429") ||
		strings.Contains(errStr, "500") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "504") ||
		strings.Contains(errStr, "rate limit") ||
		strings.Contains(errStr, "temporarily unavailable") {
		return ErrorClassRetryable, "TRANSIENT_NETWORK_ERROR"
	}

	// Default fallback to retryable
	return ErrorClassRetryable, "UNKNOWN_RETRYABLE_ERROR"
}

// HTTPStatusToErrorClass maps HTTP status codes from external services to ErrorClass.
func HTTPStatusToErrorClass(statusCode int) (ErrorClass, string) {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return ErrorClassRetryable, "HTTP_429_RATE_LIMITED"
	case statusCode >= 500 && statusCode <= 599:
		return ErrorClassRetryable, "HTTP_5XX_SERVER_ERROR"
	case statusCode == http.StatusBadRequest:
		return ErrorClassPermanent, "HTTP_400_BAD_REQUEST"
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return ErrorClassPermanent, "HTTP_AUTH_FAILURE"
	case statusCode == http.StatusNotFound:
		return ErrorClassPermanent, "HTTP_404_NOT_FOUND"
	default:
		return ErrorClassPermanent, "HTTP_CLIENT_ERROR"
	}
}

// CalculateBackoff computes bounded exponential backoff with random jitter.
func CalculateBackoff(attempt int, baseDuration, maxDuration time.Duration) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	if baseDuration <= 0 {
		baseDuration = time.Second
	}
	if maxDuration <= 0 {
		maxDuration = time.Minute
	}

	// 2^(attempt-1)
	factor := 1 << uint(attempt-1)
	if factor <= 0 || factor > 64 {
		factor = 64
	}

	backoff := baseDuration * time.Duration(factor)
	if backoff > maxDuration {
		backoff = maxDuration
	}

	// Add 10-25% jitter
	jitterMs := rand.Int63n(int64(backoff.Milliseconds()/4 + 1))
	total := backoff + time.Duration(jitterMs)*time.Millisecond
	if total > maxDuration {
		total = maxDuration
	}
	return total
}
