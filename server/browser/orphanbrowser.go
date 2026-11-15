package browser

import (
	"path/filepath"
	"strings"
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
//
// The implementation is Linux-only and lives behind a build tag rather than a
// `runtime.GOOS` check, because `syscall.Kill` does not exist on Windows at
// all — a runtime guard still has to compile, and this one did not.
func KillProcessesUsingUserDataDir(dir string) int {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return 0
	}
	// Only ever our own transient profiles. A directory supplied through
	// CHROME_ARGS belongs to the operator — the same reason cleanupUserDataDir
	// honours keepUserDataDir — and must never be used to pick kill targets.
	if !strings.HasPrefix(filepath.Base(dir), transientDirPrefix) {
		return 0
	}
	return killProcessesUsingUserDataDir(dir)
}
