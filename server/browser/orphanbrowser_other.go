//go:build !linux

package browser

// Elsewhere the reap is skipped rather than guessed at: finding the processes
// means reading /proc, and the portable substitute — matching on a process
// name — would be a licence to kill any Chrome on the machine.
func killProcessesUsingUserDataDir(string) int { return 0 }
