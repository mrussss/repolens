package agent

import "errors"

const ErrCodeModelOutputTruncated = "MODEL_OUTPUT_TRUNCATED"
const ErrCodeInvalidStructuredReport = "INVALID_STRUCTURED_REPORT"

var ErrModelOutputTruncated = errors.New(ErrCodeModelOutputTruncated)
var ErrInvalidStructuredReport = errors.New(ErrCodeInvalidStructuredReport)
