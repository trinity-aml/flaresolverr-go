package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A platform-only call must sit behind a build tag, not behind a
// `runtime.GOOS` check — a runtime guard still has to compile.
//
// The Windows CI build broke on exactly this:
//
//	server/browser/orphanbrowser.go:65:15: undefined: syscall.Kill
//	server/browser/orphanbrowser.go:77:15: undefined: syscall.Kill
//
// The orphan reaper reads /proc and signals pids, and was written with
// `if runtime.GOOS != "linux" { return 0 }` at the top. That is correct at
// run time and useless at compile time: `syscall.Kill` does not exist on
// Windows, so the package stopped building for a platform nobody runs the
// browser on but everybody cross-compiles for. darwin was fine, which is why
// a local `go build ./...` said nothing.
//
// Checked here rather than left to CI, because CI finds it after the push.
func TestPlatformOnlyCallsSitBehindBuildTags(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(src)
		if !strings.Contains(body, `"syscall"`) {
			continue
		}
		// Either form counts, because the toolchain honours both: an explicit
		// //go:build line, or a platform suffix in the file name
		// (`foo_linux.go` is Linux-only with no comment at all). Requiring
		// only the comment would fail a file that is already constrained
		// correctly.
		head := body
		if i := strings.Index(body, "\npackage "); i >= 0 {
			head = body[:i]
		}
		if strings.Contains(head, "//go:build") || hasPlatformSuffix(name) {
			continue
		}
		t.Errorf("%s imports syscall with no build constraint — neither a //go:build line nor a "+
			"platform suffix in the file name. It will fail to compile on a platform that lacks "+
			"the call, whatever runtime.GOOS check it carries", name)
	}
}

// The reaper must keep a stub for every other platform, or the package stops
// building once the Linux file is tagged.
func TestTheReaperHasANonLinuxStub(t *testing.T) {
	for _, f := range []string{"orphanbrowser_linux.go", "orphanbrowser_other.go"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s is missing: the reaper needs one implementation per platform class, "+
				"or either Linux or everything else stops compiling", f)
		}
	}
	other, err := os.ReadFile("orphanbrowser_other.go")
	if err == nil && !strings.Contains(string(other), "//go:build !linux") {
		t.Error("orphanbrowser_other.go lost its !linux constraint, so it collides with the Linux file")
	}
}

// hasPlatformSuffix reports whether the file name itself constrains the build,
// which the toolchain treats exactly like a //go:build line.
func hasPlatformSuffix(name string) bool {
	base := strings.TrimSuffix(name, ".go")
	base = strings.TrimSuffix(base, "_test")
	for _, goos := range []string{
		"linux", "windows", "darwin", "freebsd", "openbsd", "netbsd",
		"dragonfly", "solaris", "aix", "android", "ios", "js", "plan9", "wasip1", "unix",
	} {
		if strings.HasSuffix(base, "_"+goos) {
			return true
		}
	}
	return false
}
