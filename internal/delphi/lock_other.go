//go:build !unix

package delphi

// processAlive cannot be checked portably here; assume the holder is alive
// so a second loop never runs. Delete loop.lock by hand after a crash.
func processAlive(int) bool { return true }
