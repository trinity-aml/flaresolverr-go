package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startHolder launches a process whose command line carries the given
// --user-data-dir, the way Chrome's does.
func startHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 300", "--user-data-dir="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	// Wait for /proc to show the cmdline.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(pidsUsingUserDataDir(dir)) > 0 {
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("holder never appeared in /proc for %s", dir)
	return nil
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// The whole point: a browser the driver left behind must die.
func TestAnOrphanedBrowserIsKilledByItsProfileDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the scan reads /proc")
	}
	dir := filepath.Join(t.TempDir(), transientDirPrefix+"profile-test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := startHolder(t, dir)

	if n := KillProcessesUsingUserDataDir(dir); n != 1 {
		t.Fatalf("killed %d processes, want 1", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && alive(cmd.Process.Pid) {
		time.Sleep(50 * time.Millisecond)
	}
	// Reap it so the liveness check is not fooled by a zombie.
	_, _ = cmd.Process.Wait()
	if len(pidsUsingUserDataDir(dir)) != 0 {
		t.Error("a process still holds the profile directory after the reap")
	}
}

// A directory the operator supplied through CHROME_ARGS is not ours, and must
// never be used to pick kill targets — the same rule cleanupUserDataDir
// follows through keepUserDataDir.
func TestADirectoryThatIsNotOursPicksNoTargets(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the scan reads /proc")
	}
	dir := filepath.Join(t.TempDir(), "my-own-chrome-profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := startHolder(t, dir)

	if n := KillProcessesUsingUserDataDir(dir); n != 0 {
		t.Errorf("killed %d processes for a directory that is not ours, want 0", n)
	}
	if !alive(cmd.Process.Pid) {
		t.Error("a process holding an operator-supplied profile was killed")
	}
	if n := KillProcessesUsingUserDataDir(""); n != 0 {
		t.Errorf("an empty directory picked %d targets, want 0", n)
	}
}

// The match is on the whole argv element. A profile whose path is a prefix of
// another's would otherwise take its neighbour down with it, and these paths
// differ only by a random suffix.
func TestAPrefixOfAnotherProfileIsNotAMatch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the scan reads /proc")
	}
	base := t.TempDir()
	short := filepath.Join(base, transientDirPrefix+"profile-1")
	long := filepath.Join(base, transientDirPrefix+"profile-12345")
	for _, d := range []string{short, long} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := startHolder(t, long)

	if pids := pidsUsingUserDataDir(short); len(pids) != 0 {
		t.Errorf("the shorter path matched %d processes holding the longer one", len(pids))
	}
	if !alive(cmd.Process.Pid) {
		t.Error("the process holding the longer path was killed by the shorter one")
	}
	if pids := pidsUsingUserDataDir(long); len(pids) != 1 {
		t.Errorf("the exact path matched %d processes, want 1", len(pids))
	}
}

// And the teardown has to call it, after the driver is stopped — before that,
// the driver may still own the browser.
func TestTheWebdriverTeardownReapsOrphans(t *testing.T) {
	src, err := os.ReadFile("webdriver/browser.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (b *webDriverBrowser) Close() error {")
	if start < 0 {
		t.Fatal("Close not found")
	}
	fn := body[start:]
	if end := strings.Index(fn[1:], "\nfunc "); end >= 0 {
		fn = fn[:end]
	}
	// Comments are stripped first: the explanation above the call names the
	// function, so a scan that kept them passed with the call itself deleted.
	var code []string
	for _, line := range strings.Split(fn, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	fn = strings.Join(code, "\n")
	call := strings.Index(fn, "KillProcessesUsingUserDataDir")
	if call < 0 {
		t.Fatal("Close no longer reaps orphaned browsers, so a driver that could not delete " +
			"its session leaks a whole Chrome again")
	}
	stop := strings.Index(fn, "b.driver.Stop()")
	if stop < 0 || stop > call {
		t.Error("the reap runs before driver.Stop(), where the driver may still own the browser")
	}
	cleanup := strings.Index(fn, "b.cleanupUserDataDir()")
	if cleanup >= 0 && cleanup < call {
		t.Error("the profile directory is removed before the reap, which throws away the only " +
			"handle on the processes still using it")
	}
	if !strings.Contains(fn, "keepUserDataDir") {
		t.Error("the reap is not gated on keepUserDataDir, so an operator-supplied profile " +
			"could have its processes killed")
	}
}
