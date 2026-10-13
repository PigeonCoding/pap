package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"pap/internal/config"
	"pap/internal/repo"
)

// Private subdir libs co-shipped with their parent (libpulse's
// pulseaudio/libpulsecommon) must be aliased to top-level on extraction,
// so the follow-up NEEDED resolves from disk instead of failing with
// "no package provides".
func TestExtractLibsAliasesSubdirPrivateLib(t *testing.T) {
	for _, bin := range []string{"gcc", "bsdtar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s unavailable", bin)
		}
	}
	home := isolate(t)
	work := t.TempDir()

	os.WriteFile(filepath.Join(work, "bar.c"), []byte("int bar_fn(void){return 1;}\n"), 0644)
	cmd := exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libbar.so.1",
		"-o", filepath.Join(work, "libbar.so.1.0"), filepath.Join(work, "bar.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc bar: %v %s", err, out)
	}
	// Parent links the private lib and carries a RUNPATH like libpulse does.
	os.WriteFile(filepath.Join(work, "foo.c"), []byte("extern int bar_fn(void); int foo_fn(void){return bar_fn();}\n"), 0644)
	cmd = exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libfoo.so.0",
		"-o", filepath.Join(work, "libfoo.so.0.1"), filepath.Join(work, "foo.c"),
		"-L"+work, "-l:libbar.so.1.0", "-Wl,-rpath,/usr/lib/fooaudio")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gcc foo: %v %s", err, out)
	}
	barBytes, _ := os.ReadFile(filepath.Join(work, "libbar.so.1.0"))
	fooBytes, _ := os.ReadFile(filepath.Join(work, "libfoo.so.0.1"))

	staging := filepath.Join(work, "pkgroot")
	for _, d := range []string{"usr/lib/fooaudio"} {
		os.MkdirAll(filepath.Join(staging, d), 0755)
	}
	os.WriteFile(filepath.Join(staging, "usr/lib/libfoo.so.0"), fooBytes, 0755)
	os.WriteFile(filepath.Join(staging, "usr/lib/fooaudio/libbar.so.1"), barBytes, 0755)
	os.WriteFile(filepath.Join(staging, ".PKGINFO"), []byte("pkgname = e2epriv\npkgver = 1.0-1\narch = "+config.Arch+"\n"), 0644)
	pkgFile := filepath.Join(work, "e2epriv-1.0-1.pkg.tar")
	args := []string{"-cf", pkgFile, "-C", staging, "usr/lib/libfoo.so.0", "usr/lib/fooaudio/libbar.so.1", ".PKGINFO"}
	if out, err := exec.Command("bsdtar", args...).CombinedOutput(); err != nil {
		t.Fatalf("bsdtar pkg: %v %s", err, out)
	}

	libsDir := filepath.Join(home, "libs")
	os.MkdirAll(libsDir, 0755)
	pkg := repo.PkgInfo{Repo: "core", Name: "e2epriv", Version: "1.0-1", Arch: config.Arch}
	names, _, err := extractLibsFromFile(pkg, pkgFile, "libfoo.so.0", libsDir)
	if err != nil {
		t.Fatalf("extractLibsFromFile: %v", err)
	}
	// Parent staged at top-level plus the subdir copy.
	foundTop, foundSub := false, false
	for _, n := range names {
		if n == "libfoo.so.0" {
			foundTop = true
		}
		if n == filepath.Join("fooaudio", "libbar.so.1") {
			foundSub = true
		}
	}
	if !foundTop || !foundSub {
		t.Fatalf("names = %v, want libfoo.so.0 + fooaudio/libbar.so.1", names)
	}
	// The alias is what the loader ($ORIGIN) and the next BFS frontier find.
	if _, err := os.Stat(filepath.Join(libsDir, "libbar.so.1")); err != nil {
		t.Fatalf("top-level alias for co-shipped private lib missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(libsDir, "fooaudio", "libbar.so.1")); err != nil {
		t.Fatalf("subdir copy missing: %v", err)
	}

	// Already-staged short-circuit: resolving the co-shipped soname with
	// no provider must succeed from disk instead of failing.
	_, _, _, _, err = resolveAndStage(nil, []string{"libbar.so.1"}, libsDir, map[string]string{}, InstallOptions{})
	if err != nil {
		t.Fatalf("resolveAndStage already-staged = %v, want nil", err)
	}
}
