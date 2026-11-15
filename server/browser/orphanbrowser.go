package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A browser whose WebDriver session could not be deleted is orphaned, and
// nothing ever reaps it.
//
// `Close()` asks chromedriver to delete the session with a 5 s budget and
// **discards the error**, then stops chromedriver. Deleting the session is the
// only thing that makes chromedriver kill its Chrome; with chromedriver gone,
// Chrome is reparented to init and runs forever — holding its renderers, its
// GPU process and its profile.
//
// Measured on the production box 2026-11-15, fourteen hours into one run:
//
//	pid=245996 ppid=1 rss=198M started 10:27  profile-3216520039
//	pid=253640 ppid=1 rss=198M started 11:27  profile-124023389
//	pid=269139 ppid=1 rss=199M started 13:30  profile-262237998
//	pid=314741 ppid=314735(chromedriver)      ← the only live one
//
// 3918 MB of Chrome in the cgroup against 1395 MB after they were reaped, and
// their profiles still on tmpfs — which is how you can tell `Close()` never
// reached `cleanupUserDataDir()` for them. One chromedriver was alive for four
// browsers. The operator's symptom was that only restarting the service helped,
// and that is why: systemd kills the whole cgroup and takes the orphans with it.
//
// So own the process rather than trusting the driver to. Every browser gets a
// unique `--user-data-dir`, which makes it an exact, non-guessable handle on
// the processes belonging to *this* browser and no other.
//
// A live browser is always a child of its driver, so this never races the
// healthy path: by the time it runs, the driver has been stopped and anything
// still holding the directory is by definition unreachable.
func KillProcessesUsingUserDataDir(dir string) int {
	if runtime.GOOS != "linux" {
		// The scan reads /proc. Elsewhere, leave it: killing by a guessed
		// process name is far worse than leaking one browser.
		return 0
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return 0
	}
	// Only ever our own transient profiles. A directory supplied through
	// CHROME_ARGS belongs to the operator — the same reason cleanupUserDataDir
	// honours keepUserDataDir — and must never be used to pick kill targets.
	if filepath.Base(dir) == "" || !strings.HasPrefix(filepath.Base(dir), transientDirPrefix) {
		return 0
	}

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
