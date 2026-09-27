package jobs

// StopCause identifies why a claimed handler stopped. Keeping these causes
// distinct prevents infrastructure cancellation from becoming a user-visible
// business cancellation.
type StopCause string

func (cause StopCause) Error() string {
	switch cause {
	case StopUserCancel:
		return "user cancellation requested"
	case StopOwnershipLost:
		return "job ownership lost: claim token or lease is no longer valid"
	case StopLeaseFailure:
		return "lease renewal failed"
	case StopCancelPollError:
		return "cancel poll failed"
	case StopShutdown:
		return "worker shutdown"
	default:
		return string(cause)
	}
}

const (
	StopUserCancel      StopCause = "USER_CANCEL"
	StopOwnershipLost   StopCause = "OWNERSHIP_LOST"
	StopLeaseFailure    StopCause = "LEASE_FAILURE"
	StopCancelPollError StopCause = "CANCEL_POLL_ERROR"
	StopShutdown        StopCause = "WORKER_SHUTDOWN"
)

var (
	ErrUserCancellation = error(StopUserCancel)
	ErrLeaseRenewFailed = error(StopLeaseFailure)
	ErrCancelPollFailed = error(StopCancelPollError)
	ErrWorkerShutdown   = error(StopShutdown)
)
