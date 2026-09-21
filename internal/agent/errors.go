package agent

import "errors"

const ErrCodeModelOutputTruncated = "MODEL_OUTPUT_TRUNCATED"

var ErrModelOutputTruncated = errors.New(ErrCodeModelOutputTruncated)
