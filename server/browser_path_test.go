package flaresolverr

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeExecutable creates an executable file, making parent dirs as needed.
func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeMarker(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("[App]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLooksLikeGeckoInstall(t *testing.T) {
	t.Run("application.ini beside the binary", func(t *testing.T) {
		dir := t.TempDir()
		bin := writeExecutable(t, filepath.Join(dir, "firefox"))
		writeMarker(t, dir, "application.ini")
		if !looksLikeGeckoInstall(bin) {
			t.Fatal("want true for a binary with application.ini beside it")
		}
	})

	t.Run("platform.ini also counts", func(t *testing.T) {
		dir := t.TempDir()
		bin := writeExecutable(t, filepath.Join(dir, "camoufox"))
		writeMarker(t, dir, "platform.ini")
		if !looksLikeGeckoInstall(bin) {
			t.Fatal("want true for a binary with platform.ini beside it")
		}
	})

	t.Run("macOS bundle keeps the ini in Resources", func(t *testing.T) {
		root := t.TempDir()
		bin := writeExecutable(t, filepath.Join(root, "Contents", "MacOS", "firefox"))
		writeMarker(t, filepath.Join(root, "Contents", "Resources"), "application.ini")
		if !looksLikeGeckoInstall(bin) {
			t.Fatal("want true for the macOS Contents/MacOS + Contents/Resources layout")
		}
	})

	t.Run("bare wrapper is rejected", func(t *testing.T) {
		// What /usr/bin/firefox is on a snap or flatpak install: a shell script
		// with no Gecko install around it. geckodriver answers "binary is not a
		// Firefox executable" for exactly this.
		bin := writeExecutable(t, filepath.Join(t.TempDir(), "firefox"))
		if looksLikeGeckoInstall(bin) {
			t.Fatal("want false for a lone wrapper script")
		}
	})

	t.Run("symlink resolves to the real install", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need elevation on Windows")
		}
		root := t.TempDir()
		realDir := filepath.Join(root, "lib", "firefox")
		bin := writeExecutable(t, filepath.Join(realDir, "firefox"))
		writeMarker(t, realDir, "application.ini")

		link := filepath.Join(root, "firefox")
		if err := os.Symlink(bin, link); err != nil {
			t.Fatal(err)
		}
		if !looksLikeGeckoInstall(link) {
			t.Fatal("want true: the symlink target is a real install")
		}
	})
}

// pinEnvironment isolates binary discovery from the machine running the test.
func pinEnvironment(t *testing.T, pathDir, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("discovery keys off $HOME and colon-separated PATH")
	}
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", home)

	saved := wellKnownFirefoxPaths
	wellKnownFirefoxPaths = nil
	t.Cleanup(func() { wellKnownFirefoxPaths = saved })
}

func TestFindFirefoxBinaryPrefersCamoufoxOverFirefoxOnPath(t *testing.T) {
	// The regression: camoufox unpacked in ~/.cache lost to a stock firefox on
	// PATH, because PATH was searched for both names before ~/.cache was looked
	// at at all. On a machine whose /usr/bin/firefox is a wrapper script that
	// meant geckodriver refused to start at all, with camoufox sitting right
	// there unused.
	root := t.TempDir()
	pathDir := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")

	writeExecutable(t, filepath.Join(pathDir, "firefox"))
	camoufoxDir := filepath.Join(home, ".cache", "camoufox")
	camoufox := writeExecutable(t, filepath.Join(camoufoxDir, "camoufox"))
	writeMarker(t, camoufoxDir, "application.ini")

	pinEnvironment(t, pathDir, home)

	if got := findFirefoxBinary(); got != camoufox {
		t.Fatalf("findFirefoxBinary() = %q, want the camoufox at %q", got, camoufox)
	}
}

func TestFindFirefoxBinarySkipsWrapperForRealInstall(t *testing.T) {
	root := t.TempDir()
	pathDir := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")

	writeExecutable(t, filepath.Join(pathDir, "firefox")) // wrapper, no markers
	realDir := filepath.Join(root, "usr", "lib", "firefox")
	real := writeExecutable(t, filepath.Join(realDir, "firefox"))
	writeMarker(t, realDir, "application.ini")

	pinEnvironment(t, pathDir, home)
	wellKnownFirefoxPaths = []string{real}

	if got := findFirefoxBinary(); got != real {
		t.Fatalf("findFirefoxBinary() = %q, want the real install at %q", got, real)
	}
}

func TestFindFirefoxBinaryFallsBackToUnverifiedCandidate(t *testing.T) {
	// Nothing carries a marker: rather than report "no browser", hand back what
	// was found. A path geckodriver rejects is a clearer error than none.
	root := t.TempDir()
	pathDir := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")

	wrapper := writeExecutable(t, filepath.Join(pathDir, "firefox"))
	pinEnvironment(t, pathDir, home)

	if got := findFirefoxBinary(); got != wrapper {
		t.Fatalf("findFirefoxBinary() = %q, want the wrapper at %q", got, wrapper)
	}
}

func TestFindFirefoxBinaryReportsNothingWhenAbsent(t *testing.T) {
	root := t.TempDir()
	pinEnvironment(t, filepath.Join(root, "bin"), filepath.Join(root, "home"))

	if got := findFirefoxBinary(); got != "" {
		t.Fatalf("findFirefoxBinary() = %q, want empty", got)
	}
}
