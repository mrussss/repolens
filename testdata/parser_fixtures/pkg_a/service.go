package pkg_a

// Worker struct in pkg_a.
type Worker struct {
	ID string
}

// Process method on Worker in pkg_a.
func (w *Worker) Process(task string) string {
	return "pkg_a:" + task
}

// GlobalInit function in pkg_a.
func GlobalInit() bool {
	return true
}
