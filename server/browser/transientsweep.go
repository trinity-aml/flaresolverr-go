package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Transient browser profiles outlive the process that made them, and on a
// tmpfs root that is RAM.
//
// `CreateTransientDir` puts each profile under the first writable root it
// finds, and on a systemd unit with no XDG_RUNTIME_DIR that is
// `/dev/shm/flaresolverr-go` — tmpfs, i.e. RAM. Every backend's `Close()` does
// remove its own directory, but `Close()` only runs on a *graceful* shutdown.
// A Go panic (exit 2) or a SIGKILL skips it, and the jacred deployment this
// library serves took 15 panics and 2 OOM kills in one fortnight.
//
// Measured on the production box 2026-11-14:
//
//	/dev/shm/flaresolverr-go — 20 profile directories, 543 MB,
//	oldest dated 2026-11-10 (four days), exactly one with a live browser.
//
// So the fix is not a better teardown — no teardown survives SIGKILL — but a
// sweep on the way in: when the library first needs a transient directory, it
// removes the ones whose owning process is gone. Same shape as
// sweepStrayWindows: clean up what is there rather than guarantee a path that
// can always be skipped.

const transientOwnerFile = ".flaresolverr-owner"

var transientSweepOnce sync.Once

// writeTransientOwner records who owns a freshly created transient directory.
// The start time is recorded beside the pid because pids are reused, and a
// reused pid would make a dead directory look live forever.
func writeTransientOwner(dir string) {
	content := strconv.Itoa(os.Getpid())
	if st, ok := processStartTime(os.Getpid()); ok {
		content += " " + st
	}
	_ = os.WriteFile(filepath.Join(dir, transientOwnerFile), []byte(content), 0o600)
}

// SweepStaleTransientDirs removes transient directories whose owner is gone.
// It runs at most once per process; CreateTransientDir calls it, so there is
// no wiring for a caller to forget.
//
// A directory with **no** owner file is left alone. Those are either from a
// build that predates this file or a profile pinned by CHROME_ARGS
// user-data-dir, and deleting a directory we cannot reason about is the one
// outcome worse than leaking it.
func SweepStaleTransientDirs() (removed int, freedBytes int64) {
	if runtime.GOOS != "linux" {
		// Liveness here reads /proc. Everywhere else the sweep is skipped
		// rather than guessed at from mtimes — an idle browser's profile has
		// an old mtime and deleting it would break a live session.
		return 0, 0
	}
	seen := map[string]bool{}
	for _, root := range transientDirRoots() {
		root = strings.TrimSpace(root)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "flaresolverr-go-") {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if transientDirIsLive(dir) {
				continue
			}
			size := dirSize(dir)
			if err := os.RemoveAll(dir); err == nil {
				removed++
				freedBytes += size
			}
		}
	}
	return removed, freedBytes
}

// transientDirIsLive reports whether the directory's recorded owner is still
// running. An unreadable or absent owner file counts as live, deliberately:
// see SweepStaleTransientDirs.
func transientDirIsLive(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, transientOwnerFile))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return true
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return true
	}
	if pid == os.Getpid() {
		// Last-resort guard: never collect the live owner's own profile, even
		// if its recorded start time somehow disagrees.
		return true
	}
	start, ok := processStartTime(pid)
	if !ok {
		// No such process: the owner is gone.
		return false
	}
	if len(fields) > 1 && fields[1] != start {
		// The pid is live but it is a different process — pid reuse.
		return false
	}
	return true
}

// processStartTime returns field 22 of /proc/<pid>/stat, the process start
// time in clock ticks since boot. Parsing has to start after the *last* ')'
// because the comm field can itself contain spaces and parentheses.
func processStartTime(pid int) (string, bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	s := string(raw)
	close := strings.LastIndexByte(s, ')')
	if close < 0 || close+2 >= len(s) {
		return "", false
	}
	fields := strings.Fields(s[close+2:])
	// After comm and state, field 22 overall is index 19 here.
	if len(fields) < 20 {
		return "", false
	}
	return fields[19], true
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
