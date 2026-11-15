//go:build linux

package browser

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func killProcessesUsingUserDataDir(dir string) int {
	pids := pidsUsingUserDataDir(dir)
	if len(pids) == 0 {
		return 0
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	// Chrome shuts its children down on SIGTERM; give it a moment before
	// insisting, so the usual case leaves no half-killed renderers behind.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(pidsUsingUserDataDir(dir)) == 0 {
			return len(pids)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pidsUsingUserDataDir(dir) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return len(pids)
}

// pidsUsingUserDataDir lists processes whose command line names exactly this
// user-data-dir. The match is on `--user-data-dir=<dir>` as one argv element,
// so a directory that is a prefix of another cannot be caught by mistake.
func pidsUsingUserDataDir(dir string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	want := "--user-data-dir=" + dir
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		// Chrome rewrites a renderer's cmdline as one NUL-terminated string
		// with spaces inside, so split on both.
		for _, field := range strings.FieldsFunc(string(raw), func(r rune) bool {
			return r == 0 || r == ' '
		}) {
			if field == want {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}
