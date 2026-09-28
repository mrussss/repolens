package analysispipeline

import "errors"

var (
	ErrRevisionNotReady = errors.New("analysis revision is not ready")
	ErrBuildNotReady    = errors.New("diagnosis build is not ready")
)
