package pkg_b

// Worker struct in pkg_b with same name as pkg_a.Worker.
type Worker struct {
	ID string
}

// Process method on Worker in pkg_b with same name as pkg_a.Worker.Process.
func (w *Worker) Process(task string) string {
	return "pkg_b:" + task
}

// GlobalInit function in pkg_b with same name as pkg_a.GlobalInit.
func GlobalInit() bool {
	return false
}
