package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// The production leak: 20 profile directories, 543 MB, in tmpfs, oldest four
// days old, exactly one with a live browser. Close() does remove its own
// directory — but only on a graceful shutdown, and this deployment took 15
// panics and 2 OOM kills in a fortnight.
func TestSweepRemovesDirsWhoseOwnerIsGone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("liveness here reads /proc")
	}
	root := t.TempDir()
	t.Setenv("FLARESOLVERR_TMPDIR", root)

	mk := func(name, owner string) string {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "payload"), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		if owner != "" {
			if err := os.WriteFile(filepath.Join(d, transientOwnerFile), []byte(owner), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}

	// A pid that cannot be running: 2^22 is above the default pid_max.
	deadPid := "4194305"
	live, _ := processStartTime(os.Getpid())

	gone := mk("flaresolverr-go-profile-gone", deadPid)
	mine := mk("flaresolverr-go-profile-mine", strconv.Itoa(os.Getpid())+" "+live)
	noOwner := mk("flaresolverr-go-profile-noowner", "")
	// A live pid recorded with the wrong start time: pid reuse, so stale.
	// It must be a pid other than our own — transientDirIsLive short-circuits
	// on os.Getpid() on purpose, as a last-resort guard against ever deleting
	// the live owner's own profile, and that shortcut would hide this case.
	// pid 1 is always running and its start time is never the "1" written here.
	reused := mk("flaresolverr-go-profile-reused", "1 1")
	// Not ours at all.
	foreign := mk("something-else", deadPid)

	removed, freed := SweepStaleTransientDirs()

	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Error("a directory whose owner is gone survived — this is the production leak")
	}
	if _, err := os.Stat(reused); !os.IsNotExist(err) {
		t.Error("a directory whose pid was reused survived; without the start-time check a recycled pid makes a dead profile look live forever")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Error("the sweep deleted the live owner's own directory")
	}
	if _, err := os.Stat(noOwner); err != nil {
		t.Error("a directory with no owner file was deleted — it may be a CHROME_ARGS user-data-dir or predate the owner file, and deleting what we cannot reason about is worse than leaking it")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("the sweep went outside the flaresolverr-go- prefix")
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
	if freed < 4096 {
		t.Errorf("freed %d bytes, want at least the two payloads", freed)
	}
}

// Every directory the library creates must carry an owner file, or the sweep
// can never collect it — and a directory with no owner is deliberately immune.
func TestCreatedDirsCarryAnOwnerFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLARESOLVERR_TMPDIR", root)
	transientDirRootMu.Lock()
	transientDirRoot = ""
	transientDirRootMu.Unlock()

	dir, err := CreateTransientDir("flaresolverr-go-profile-*")
	if err != nil {
		t.Fatalf("CreateTransientDir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, transientOwnerFile))
	if err != nil {
		t.Fatalf("no owner file in %s: %v", dir, err)
	}
	if !transientDirIsLive(dir) {
		t.Error("a directory this process just created reads as stale")
	}
	if want := strconv.Itoa(os.Getpid()); len(raw) == 0 || string(raw[:len(want)]) != want {
		t.Errorf("owner file = %q, want it to start with our pid %s", raw, want)
	}
}
