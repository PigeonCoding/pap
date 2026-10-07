package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"pap/internal/config"
	"pap/internal/manifest"
	"pap/internal/repo"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PAP_HOME", dir)
	t.Setenv("PAP_BIN_DIR", filepath.Join(dir, "bin"))
	config.Refresh()
	repo.OverrideDefault(nil)
	t.Cleanup(func() { repo.OverrideDefault(nil) })
	return dir
}

func TestValidName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "/abs"} {
		if err := validName(bad); err == nil {
			t.Errorf("validName(%q) = nil, want error", bad)
		}
	}
	for _, ok := range []string{"curl", "my-app_1", "a"} {
		if err := validName(ok); err != nil {
			t.Errorf("validName(%q) = %v, want nil", ok, err)
		}
	}
}

func TestDetectResourceDirs(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	resDir := filepath.Join(dir, "lib", "foo")
	if err := os.MkdirAll(resDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resDir, "data.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "myapp")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	// String reference to ../lib/foo must be found.
	if err := os.WriteFile(exe, []byte("\x7fELF....../lib/foo/bar\x00more"), 0755); err != nil {
		t.Fatal(err)
	}
	got := detectResourceDirs(exe, "other")
	if len(got) != 1 || got[0].host != resDir || got[0].rel() != filepath.Join("lib", "foo") {
		t.Fatalf("string-ref detect = %+v, want [{host:%s rel:lib/foo}]", got, resDir)
	}
	// Name-based fallback: no string ref, but ../lib/<name> exists.
	if err := os.WriteFile(exe, []byte("\x7fELF no refs here"), 0755); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(dir, "lib", "myapp")
	if err := os.MkdirAll(named, 0755); err != nil {
		t.Fatal(err)
	}
	got = detectResourceDirs(exe, "myapp")
	found := false
	for _, rd := range got {
		if rd.host == named {
			found = true
		}
	}
	if !found {
		t.Fatalf("name-based detect = %+v, want %s included", got, named)
	}
	// Nothing present: no resources.
	got = detectResourceDirs(exe, "nosuchapp")
	for _, rd := range got {
		if rd.host == filepath.Join(dir, "lib", "nosuchapp") {
			t.Fatalf("false positive: %+v", got)
		}
	}
	// Siblings composed at runtime (`%s/%s/<sib>`): a sibling dir whose name
	// the binary mentions is vendored too; unmentioned ones are not.
	sib := filepath.Join(dir, "lib", "kitty-extensions")
	if err := os.MkdirAll(sib, 0755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "lib", "unrelated")
	if err := os.MkdirAll(plain, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("\x7fELF....../lib/foo/bar\x00%s/%s/kitty-extensions\x00"), 0755); err != nil {
		t.Fatal(err)
	}
	direct := detectResourceDirs(exe, "other")
	if len(direct) != 2 { // lib/foo (string ref) + lib/myapp (exe basename)
		t.Fatalf("direct detect = %+v", direct)
	}
	got, _ = appendSiblingResources(direct, exe)
	// lib/myapp matches the exe basename (name-based fallback).
	want := map[string]bool{resDir: true, sib: true, filepath.Join(dir, "lib", "myapp"): true}
	if len(got) != len(want) {
		t.Fatalf("sibling detect = %+v, want %v", got, want)
	}
	for _, rd := range got {
		if !want[rd.host] {
			t.Fatalf("unexpected resource %+v", rd)
		}
	}
}

func TestMentionsName(t *testing.T) {
	if !mentionsName("a/kitten\x00", "kitten") {
		t.Error("bounded occurrence missed")
	}
	if mentionsName("a/kittens/b", "kitten") {
		t.Error("substring false positive")
	}
	if mentionsName("xxkitten", "kitten") {
		t.Error("prefix false positive")
	}
	if !mentionsName("%s/%s/kitty-extensions", "kitty-extensions") {
		t.Error("format-string occurrence missed")
	}
}

func TestDetectBinSiblings(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0755)
	hostELF := hostELFPath(t)
	copyFile := func(dst string) {
		t.Helper()
		data, err := os.ReadFile(hostELF)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0755); err != nil {
			t.Fatal(err)
		}
	}
	main := filepath.Join(bin, "mainapp")
	copyFile(main)
	// Sibling companion: mentioned in corpus, executable ELF, no .so.
	companion := filepath.Join(bin, "kitten")
	copyFile(companion)
	// Unmentioned sibling: must be skipped.
	copyFile(filepath.Join(bin, "othertool"))
	// .so files are never companions (local-libs path handles those).
	os.WriteFile(filepath.Join(bin, "libx.so.1"), []byte("\x7fELF"), 0755)
	corpus := "run kitten\x00 now"
	got := detectBinSiblings(main, corpus)
	if len(got) != 1 || got[0] != companion {
		t.Fatalf("bin siblings = %v, want [%s]", got, companion)
	}
}

func TestSiblingScanAllowed(t *testing.T) {
	for _, sys := range []string{"/usr/lib", "/usr/lib64", "/lib", "/lib64", "/usr/share", "/usr/local/lib"} {
		if siblingScanAllowed(sys) {
			t.Errorf("siblingScanAllowed(%q) = true, want false", sys)
		}
	}
	for _, ok := range []string{"/opt/myapp/lib", "/home/u/.pap/exe/x/lib", "/tmp/bundle/lib"} {
		if !siblingScanAllowed(ok) {
			t.Errorf("siblingScanAllowed(%q) = false, want true", ok)
		}
	}
}

func TestCopyTreePreservesSymlinks(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("hi"), 0644)
	os.MkdirAll(filepath.Join(src, "sub"), 0755)
	os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("yo"), 0755)
	if err := os.Symlink("a.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "sub", "b.txt")); string(data) != "yo" {
		t.Fatalf("nested file = %q", data)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link.txt")); err != nil || target != "a.txt" {
		t.Fatalf("symlink = %q,%v", target, err)
	}
}

func TestInstallScriptE2E(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(t.TempDir(), "hello.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := InstallElf(src, "hello", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin", "hello")
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("installed bin missing: %v", err)
	}
	if len(data) == 0 || data[0] != '#' {
		t.Fatalf("installed script mangled: %q", data[:min(20, len(data))])
	}
	// Script entrypoints install a launcher (regular file) so $0 resolves
	// to the payload; accept a symlink too for backwards compatibility.
	if fi, err := os.Lstat(bin); err != nil {
		t.Fatalf("bin entry missing: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 && fi.Mode().Perm()&0111 == 0 {
		t.Fatalf("bin entry is not executable: %v", fi.Mode())
	}
	m, err := manifest.Load(filepath.Join(home, "apps", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if m.SourceSHA256 == "" || !m.PlacedBinary {
		t.Fatalf("manifest missing hash/placed: %+v", m)
	}
	// Safer uninstall removes the binary we placed and runs GC.
	if err := Uninstall("hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Fatal("bin not removed on uninstall")
	}
	// Uninstall without manifest must NOT touch bin.
	if err := os.MkdirAll(filepath.Join(home, "apps", "ghost"), 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(home, "bin", "ghost")
	if err := os.WriteFile(sentinel, []byte("user file"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall("ghost"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("uninstall without manifest deleted a user file")
	}
}

func TestDryRunMakesNoChanges(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(t.TempDir(), "x.sh")
	os.WriteFile(src, []byte("#!/bin/sh\n"), 0755)
	if err := InstallElf(src, "x", InstallOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "apps", "x")); !os.IsNotExist(err) {
		t.Fatal("dry-run created app dir")
	}
}

// TestInstallElfE2E builds a tiny binary needing libe2e.so.1, serves a
// synthetic repo db + package over httptest, and runs the full install.
func TestInstallElfE2E(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc unavailable")
	}
	if _, err := exec.LookPath("bsdtar"); err != nil {
		t.Skip("bsdtar unavailable")
	}
	if _, err := exec.LookPath("patchelf"); err != nil {
		t.Skip("patchelf unavailable")
	}
	home := isolate(t)
	work := t.TempDir()

	// Fake shared lib with SONAME libe2e.so.1.
	libSrc := "int e2e_fn(void){return 42;}\n"
	os.WriteFile(filepath.Join(work, "e2e.c"), []byte(libSrc), 0644)
	lib := filepath.Join(work, "libe2e.so.1.0")
	cmd := exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libe2e.so.1", "-o", lib, filepath.Join(work, "e2e.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc lib: %v %s", err, out)
	}
	appSrc := "extern int e2e_fn(void); int main(void){return e2e_fn();}\n"
	os.WriteFile(filepath.Join(work, "app.c"), []byte(appSrc), 0644)
	appBin := filepath.Join(work, "appbin")
	cmd = exec.Command("gcc", "-o", appBin, filepath.Join(work, "app.c"), "-L"+work, "-l:libe2e.so.1.0", "-Wl,-rpath,"+work)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc app: %v %s", err, out)
	}

	// Fake Arch package containing usr/lib/libe2e.so.1.
	pkgStaging := filepath.Join(work, "pkgroot")
	os.MkdirAll(filepath.Join(pkgStaging, "usr", "lib"), 0755)
	libBytes, _ := os.ReadFile(lib)
	os.WriteFile(filepath.Join(pkgStaging, "usr", "lib", "libe2e.so.1"), libBytes, 0755)
	pkginfo := "pkgname = e2elib\npkgver = 1.0-1\narch = x86_64\n"
	os.WriteFile(filepath.Join(pkgStaging, ".PKGINFO"), []byte(pkginfo), 0644)
	pkgFile := filepath.Join(work, "e2elib-1.0-1-x86_64.pkg.tar")
	if out, err := exec.Command("bsdtar", "-cf", pkgFile, "-C", pkgStaging, "usr/lib/libe2e.so.1", ".PKGINFO").CombinedOutput(); err != nil {
		t.Fatalf("bsdtar pkg: %v %s", err, out)
	}
	pkgBytes, _ := os.ReadFile(pkgFile)
	h := sha256.Sum256(pkgBytes)
	sha := hex.EncodeToString(h[:])

	desc := "%FILENAME%\ne2elib-1.0-1-x86_64.pkg.tar\n\n%NAME%\ne2elib\n\n%VERSION%\n1.0-1\n\n%ARCH%\nx86_64\n\n%SHA256SUM%\n" + sha + "\n\n%PROVIDES%\nlibe2e.so=1-64\n"
	dbBytes := gzipTar([]tarEntry{{"e2elib-1.0-1/desc", desc}})

	mux := http.NewServeMux()
	mux.HandleFunc("/core.db", func(w http.ResponseWriter, r *http.Request) { w.Write(dbBytes) })
	mux.HandleFunc("/e2elib-1.0-1-x86_64.pkg.tar", func(w http.ResponseWriter, r *http.Request) { w.Write(pkgBytes) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cacheDir := filepath.Join(home, "cache", "repo")
	pkgCache := filepath.Join(home, "pkgcache")
	idx := repo.NewTestRepo("core", srv.URL, cacheDir, pkgCache)
	repo.OverrideDefault(idx)

	if err := InstallElf(appBin, "e2eapp", InstallOptions{}); err != nil {
		t.Fatalf("InstallElf e2e: %v", err)
	}
	link := filepath.Join(home, "bin", "e2eapp")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("bin entry is not a symlink: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(home, "exe", "e2eapp", "bin", "e2eapp")); err != nil {
		t.Fatalf("real binary missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "apps", "e2eapp", "libs", "libe2e.so.1")); err != nil {
		t.Fatalf("vendored lib missing: %v", err)
	}
	// Run through the symlink: exit code 42 comes from e2e_fn().
	if err := exec.Command(link).Run(); err == nil {
		t.Fatal("expected exit code 42, got 0")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 42 {
		t.Fatalf("symlink run = %v, want exit 42", err)
	}
}

// TestInstallElfE2EWithResources mirrors the kitty failure: the binary
// embeds a ../lib/<app> reference and ships a host resource tree. Install
// must vendor the tree and leave a working symlink in the bin dir.
func TestInstallElfE2EWithResources(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc unavailable")
	}
	if _, err := exec.LookPath("bsdtar"); err != nil {
		t.Skip("bsdtar unavailable")
	}
	if _, err := exec.LookPath("patchelf"); err != nil {
		t.Skip("patchelf unavailable")
	}
	home := isolate(t)
	work := t.TempDir()

	libSrc := "int e2e_fn(void){return 42;}\n"
	os.WriteFile(filepath.Join(work, "e2e.c"), []byte(libSrc), 0644)
	lib := filepath.Join(work, "libe2e.so.1.0")
	cmd := exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libe2e.so.1", "-o", lib, filepath.Join(work, "e2e.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc lib: %v %s", err, out)
	}
	// The binary references its resources via an exe-relative path, like kitty.
	appSrc := "extern int e2e_fn(void);\nconst char *e2e_res = \"../lib/e2eres/data.txt\";\nint main(int argc, char **argv){ (void)argc; (void)argv; (void)e2e_res; return e2e_fn(); }\n"
	os.WriteFile(filepath.Join(work, "app.c"), []byte(appSrc), 0644)
	binDir := filepath.Join(work, "bin")
	os.MkdirAll(binDir, 0755)
	appBin := filepath.Join(binDir, "appbin")
	cmd = exec.Command("gcc", "-o", appBin, filepath.Join(work, "app.c"), "-L"+work, "-l:libe2e.so.1.0", "-Wl,-rpath,"+work)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc app: %v %s", err, out)
	}
	// Host resource tree at <exedir>/../lib/e2eres.
	resDir := filepath.Join(work, "lib", "e2eres")
	os.MkdirAll(resDir, 0755)
	if err := os.WriteFile(filepath.Join(resDir, "data.txt"), []byte("resource-bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	pkgStaging := filepath.Join(work, "pkgroot")
	os.MkdirAll(filepath.Join(pkgStaging, "usr", "lib"), 0755)
	libBytes, _ := os.ReadFile(lib)
	os.WriteFile(filepath.Join(pkgStaging, "usr", "lib", "libe2e.so.1"), libBytes, 0755)
	pkginfo := "pkgname = e2elib\npkgver = 1.0-1\narch = x86_64\n"
	os.WriteFile(filepath.Join(pkgStaging, ".PKGINFO"), []byte(pkginfo), 0644)
	pkgFile := filepath.Join(work, "e2elib-1.0-1-x86_64.pkg.tar")
	if out, err := exec.Command("bsdtar", "-cf", pkgFile, "-C", pkgStaging, "usr/lib/libe2e.so.1", ".PKGINFO").CombinedOutput(); err != nil {
		t.Fatalf("bsdtar pkg: %v %s", err, out)
	}
	pkgBytes, _ := os.ReadFile(pkgFile)
	h := sha256.Sum256(pkgBytes)
	sha := hex.EncodeToString(h[:])

	desc := "%FILENAME%\ne2elib-1.0-1-x86_64.pkg.tar\n\n%NAME%\ne2elib\n\n%VERSION%\n1.0-1\n\n%ARCH%\nx86_64\n\n%SHA256SUM%\n" + sha + "\n\n%PROVIDES%\nlibe2e.so=1-64\n"
	dbBytes := gzipTar([]tarEntry{{"e2elib-1.0-1/desc", desc}})

	mux := http.NewServeMux()
	mux.HandleFunc("/core.db", func(w http.ResponseWriter, r *http.Request) { w.Write(dbBytes) })
	mux.HandleFunc("/e2elib-1.0-1-x86_64.pkg.tar", func(w http.ResponseWriter, r *http.Request) { w.Write(pkgBytes) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	idx := repo.NewTestRepo("core", srv.URL, filepath.Join(home, "cache", "repo"), filepath.Join(home, "pkgcache"))
	repo.OverrideDefault(idx)

	if err := InstallElf(appBin, "e2eres", InstallOptions{}); err != nil {
		t.Fatalf("InstallElf e2e resources: %v", err)
	}
	m, err := manifest.Load(filepath.Join(home, "apps", "e2eres"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(m.ExeRel) && m.ExeRel != filepath.Join("bin", "e2eres") {
		t.Fatalf("manifest ExeRel = %q, want bin/e2eres", m.ExeRel)
	}
	link := filepath.Join(home, "bin", "e2eres")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("bin entry missing: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("bin entry is not a symlink: %v", fi.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	wantTarget := filepath.Join(home, "exe", "e2eres", "bin", "e2eres")
	if target != wantTarget {
		t.Fatalf("symlink -> %q, want %q", target, wantTarget)
	}
	if _, err := os.Stat(filepath.Join(home, "exe", "e2eres", "bin", "e2eres")); err != nil {
		t.Fatalf("real binary missing: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(home, "exe", "e2eres", "lib", "e2eres", "data.txt")); err != nil || string(data) != "resource-bytes" {
		t.Fatalf("vendored resource = %q,%v", data, err)
	}
	// Running through the symlink must exec the app: exit code 42 comes from e2e_fn().
	cmd = exec.Command(link)
	// The app needs its lib: RPATH points at the app libs dir, so no env needed.
	if err := cmd.Run(); err == nil {
		t.Fatal("expected exit code 42, got 0")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 42 {
		t.Fatalf("symlink run = %v, want exit 42", err)
	}
	if err := Uninstall("e2eres"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("symlink not removed on uninstall")
	}
	if _, err := os.Stat(filepath.Join(home, "exe", "e2eres")); !os.IsNotExist(err) {
		t.Fatal("exe dir not removed on uninstall")
	}
}

func TestBuildAndStageLocalLib(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc unavailable")
	}
	home := isolate(t)
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "e2e.c"), []byte("int e2e_fn(void){return 1;}\n"), 0644)
	lib := filepath.Join(work, "lib", "libe2e.so.1.0")
	os.MkdirAll(filepath.Join(work, "lib"), 0755)
	cmd := exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libe2e.so.1", "-o", lib, filepath.Join(work, "e2e.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc lib: %v %s", err, out)
	}
	m := buildLocalLibs([]string{work})
	if m["libe2e.so.1"] != lib {
		t.Fatalf("local index = %v, want libe2e.so.1 -> %s", m, lib)
	}
	libsDir := filepath.Join(home, "apps", "x", "libs")
	names, _, err := stageLocalLib("libe2e.so.1", lib, libsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "libe2e.so.1" {
		t.Fatalf("staged = %v", names)
	}
	if _, err := os.Lstat(filepath.Join(libsDir, "libe2e.so.1")); err != nil {
		t.Fatalf("staged link missing: %v", err)
	}
	// Non-ELF / missing files are skipped, never fatal.
	if m := buildLocalLibs([]string{filepath.Join(work, "nosuchdir")}); len(m) != 0 {
		t.Fatalf("missing root index = %v", m)
	}
	os.WriteFile(filepath.Join(work, "lib", "notalib.so.txt"), []byte("nope"), 0644)
	if m := buildLocalLibs([]string{work}); m["notalib.so.txt"] != "" {
		t.Fatalf("non-ELF indexed: %v", m)
	}
}

func TestSplitInstallArgs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "appbin")
	os.WriteFile(file, []byte("x"), 0755)

	// Straight order.
	p, n, sw, err := SplitInstallArgs([]string{file, "myapp"})
	if err != nil || p != file || n != "myapp" || sw {
		t.Fatalf("straight = %q,%q,%v,%v", p, n, sw, err)
	}
	// Swapped: missing first arg, existing second.
	p, n, sw, err = SplitInstallArgs([]string{"myapp", file})
	if err != nil || p != file || n != "myapp" || !sw {
		t.Fatalf("swapped = %q,%q,%v,%v", p, n, sw, err)
	}
	// Swapped with existing folder first (the `install kitty ./kitty/bin/kitty`
	// shape): the second arg can't be a name, so names/paths swap.
	t.Chdir(dir)
	os.MkdirAll("myapp/bin", 0755)
	os.WriteFile(filepath.Join("myapp", "bin", "prog"), []byte("x"), 0755)
	p, n, sw, err = SplitInstallArgs([]string{"myapp", filepath.Join("myapp", "bin", "prog")})
	if err != nil || n != "myapp" || !sw {
		t.Fatalf("name-first swap = %q,%q,%v,%v", p, n, sw, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("swapped path does not exist: %q", p)
	}
	// Missing path.
	if _, _, _, err := SplitInstallArgs([]string{filepath.Join(dir, "nope"), "x"}); err == nil {
		t.Fatal("expected missing-path error")
	}
	// Too many args.
	if _, _, _, err := SplitInstallArgs([]string{"a", "b", "c"}); err == nil {
		t.Fatal("expected too-many-args error")
	}
	// None.
	if _, _, _, err := SplitInstallArgs(nil); err == nil {
		t.Fatal("expected missing-path error")
	}
}

func TestResolveMainExe(t *testing.T) {
	hostELF := hostELFPath(t)
	copyELF := func(dst string) {
		t.Helper()
		data, err := os.ReadFile(hostELF)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0755); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	copyELF(filepath.Join(dir, "onlybin"))
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0644)
	if rel, err := resolveMainExe(dir, "whatever"); err != nil || rel != "onlybin" {
		t.Fatalf("single candidate = %q,%v", rel, err)
	}
	copyELF(filepath.Join(dir, "named"))
	if rel, err := resolveMainExe(dir, "named"); err != nil || rel != "named" {
		t.Fatalf("name select = %q,%v", rel, err)
	}
	if _, err := resolveMainExe(dir, "other"); err == nil {
		t.Fatal("expected ambiguity error")
	}
	// Nested bin/ layout (upstream bundles): <dir>/bin/<name> wins.
	bindir := t.TempDir()
	os.MkdirAll(filepath.Join(bindir, "bin"), 0755)
	copyELF(filepath.Join(bindir, "bin", "nested"))
	if rel, err := resolveMainExe(bindir, "nested"); err != nil || rel != filepath.Join("bin", "nested") {
		t.Fatalf("bin nested = %q,%v", rel, err)
	}
	// Ambiguous bin/ scan without a name match errors.
	copyELF(filepath.Join(bindir, "bin", "second"))
	if _, err := resolveMainExe(bindir, "nosuch"); err == nil {
		t.Fatal("expected bin ambiguity error")
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "x.txt"), []byte("x"), 0644)
	if _, err := resolveMainExe(empty, "x"); err == nil {
		t.Fatal("expected no-executable error")
	}
}

func TestResolveMainExePrefersLauncherScript(t *testing.T) {
	hostELF := hostELFPath(t)
	dir := t.TempDir()
	data, err := os.ReadFile(hostELF)
	if err != nil {
		t.Fatal(err)
	}
	// Bundle layout: backend ELF at top level, launcher script under bin/.
	if err := os.WriteFile(filepath.Join(dir, "codium"), data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "codium"), []byte("#!/bin/sh\nexec\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if rel, err := resolveMainExe(dir, "codium"); err != nil || rel != filepath.Join("bin", "codium") {
		t.Fatalf("launcher script = %q,%v", rel, err)
	}
	// ELF-only folders still resolve the ELF.
	elfOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(elfOnly, "tool"), data, 0755); err != nil {
		t.Fatal(err)
	}
	if rel, err := resolveMainExe(elfOnly, "tool"); err != nil || rel != "tool" {
		t.Fatalf("elf main = %q,%v", rel, err)
	}
}

func TestElectronBundleDir(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "codium")
	os.WriteFile(exe, []byte("\x7fELF"), 0755)
	if _, ok := electronBundleDir(exe); ok {
		t.Fatal("bare dir reported as bundle")
	}
	os.WriteFile(filepath.Join(dir, "icudtl.dat"), []byte("x"), 0644)
	if _, ok := electronBundleDir(exe); ok {
		t.Fatal("half bundle reported as bundle")
	}
	os.WriteFile(filepath.Join(dir, "resources.pak"), []byte("x"), 0644)
	if got, ok := electronBundleDir(exe); !ok || got != dir {
		t.Fatalf("bundle = %q,%v", got, ok)
	}
}

func TestResolveMainExeAcceptsScript(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "myapp"), []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if rel, err := resolveMainExe(dir, "myapp"); err != nil || rel != "myapp" {
		t.Fatalf("script main = %q,%v", rel, err)
	}
	// A lone script with no name match still resolves when unambiguous.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "run.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if rel, err := resolveMainExe(other, "whatever"); err != nil || rel != "run.sh" {
		t.Fatalf("single script = %q,%v", rel, err)
	}
}

func TestListFolderELFs(t *testing.T) {
	dir := t.TempDir()
	elfBytes := []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "a"), append(elfBytes, 1), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "b"), append(elfBytes, 2), 0755)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0644)
	// Symlinks are skipped (the target itself is visited).
	if err := os.Symlink("a", filepath.Join(dir, "link-a")); err != nil {
		t.Fatal(err)
	}
	got := listFolderELFs(dir)
	if len(got) != 2 {
		t.Fatalf("listFolderELFs = %v, want 2 entries", got)
	}
	if got2 := neededFromELFList(nil); len(got2) != 0 {
		t.Fatalf("neededFromELFList(nil) = %v", got2)
	}
}

func TestResolveExplicitExe(t *testing.T) {
	hostELF := hostELFPath(t)
	data, err := os.ReadFile(hostELF)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app"), data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "tool"), data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	if rel, err := resolveExplicitExe(dir, "sub/tool"); err != nil || rel != filepath.Join("sub", "tool") {
		t.Fatalf("nested exe = %q,%v", rel, err)
	}
	for _, bad := range []string{
		"/abs/path",     // absolute paths rejected
		"../escape",     // folder escapes rejected
		"sub/../../esc", // normalized escapes rejected
		".",             // self rejected
		"missing",       // nonexistent rejected
		"sub",           // directories rejected
		"notes.txt",     // non-executables rejected
	} {
		if got, err := resolveExplicitExe(dir, bad); err == nil {
			t.Fatalf("resolveExplicitExe(%q) = %q, want error", bad, got)
		}
	}
}

// hostELFPath returns a real host ELF for tests needing parseable binaries.
func hostELFPath(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/ls", "/usr/bin/ls"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	t.Skip("no host ELF available")
	return ""
}

func TestStageResourceDest(t *testing.T) {
	rd := resourceDir{cls: "lib", first: "foo"}
	if got, ok := stageResourceDest(filepath.Join("bin", "app"), rd); !ok || got != filepath.Join("lib", "foo") {
		t.Fatalf("bin layout = %q,%v", got, ok)
	}
	if got, ok := stageResourceDest(filepath.Join("pkg", "myapp"), rd); !ok || got != filepath.Join("lib", "foo") {
		t.Fatalf("pkg root layout = %q,%v", got, ok)
	}
	if got, ok := stageResourceDest(filepath.Join("pkg", "sub", "myapp"), rd); !ok || got != filepath.Join("pkg", "lib", "foo") {
		t.Fatalf("pkg nested layout = %q,%v", got, ok)
	}
	// A reference escaping the payload root is refused.
	rdUp := resourceDir{cls: "..", first: "etc"}
	if _, ok := stageResourceDest(filepath.Join("bin", "app"), rdUp); ok {
		t.Fatal("expected escape refusal")
	}
}

// TestInstallDirE2E installs a folder: layout preserved under exe/<name>/pkg,
// main exe patched in place, symlink on PATH, folder-internal resources intact.
func TestInstallDirE2E(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc unavailable")
	}
	if _, err := exec.LookPath("bsdtar"); err != nil {
		t.Skip("bsdtar unavailable")
	}
	if _, err := exec.LookPath("patchelf"); err != nil {
		t.Skip("patchelf unavailable")
	}
	home := isolate(t)
	work := t.TempDir()

	libSrc := "int e2e_fn(void){return 42;}\n"
	os.WriteFile(filepath.Join(work, "e2e.c"), []byte(libSrc), 0644)
	lib := filepath.Join(work, "libe2e.so.1.0")
	cmd := exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libe2e.so.1", "-o", lib, filepath.Join(work, "e2e.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc lib: %v %s", err, out)
	}
	folder := filepath.Join(work, "folderapp")
	os.MkdirAll(folder, 0755)
	appSrc := "extern int e2e_fn(void); int main(void){return e2e_fn();}\n"
	os.WriteFile(filepath.Join(work, "app.c"), []byte(appSrc), 0644)
	cmd = exec.Command("gcc", "-o", filepath.Join(folder, "folderapp"), filepath.Join(work, "app.c"), "-L"+work, "-l:libe2e.so.1.0", "-Wl,-rpath,"+work)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc app: %v %s", err, out)
	}
	// Folder-internal resources shipped next to the exe.
	os.MkdirAll(filepath.Join(folder, "lib", "folderapp"), 0755)
	os.WriteFile(filepath.Join(folder, "lib", "folderapp", "data.txt"), []byte("folder-bytes"), 0644)

	pkgStaging := filepath.Join(work, "pkgroot")
	os.MkdirAll(filepath.Join(pkgStaging, "usr", "lib"), 0755)
	libBytes, _ := os.ReadFile(lib)
	os.WriteFile(filepath.Join(pkgStaging, "usr", "lib", "libe2e.so.1"), libBytes, 0755)
	pkginfo := "pkgname = e2elib\npkgver = 1.0-1\narch = x86_64\n"
	os.WriteFile(filepath.Join(pkgStaging, ".PKGINFO"), []byte(pkginfo), 0644)
	pkgFile := filepath.Join(work, "e2elib-1.0-1-x86_64.pkg.tar")
	if out, err := exec.Command("bsdtar", "-cf", pkgFile, "-C", pkgStaging, "usr/lib/libe2e.so.1", ".PKGINFO").CombinedOutput(); err != nil {
		t.Fatalf("bsdtar pkg: %v %s", err, out)
	}
	pkgBytes, _ := os.ReadFile(pkgFile)
	h := sha256.Sum256(pkgBytes)
	sha := hex.EncodeToString(h[:])

	desc := "%FILENAME%\ne2elib-1.0-1-x86_64.pkg.tar\n\n%NAME%\ne2elib\n\n%VERSION%\n1.0-1\n\n%ARCH%\nx86_64\n\n%SHA256SUM%\n" + sha + "\n\n%PROVIDES%\nlibe2e.so=1-64\n"
	dbBytes := gzipTar([]tarEntry{{"e2elib-1.0-1/desc", desc}})

	mux := http.NewServeMux()
	mux.HandleFunc("/core.db", func(w http.ResponseWriter, r *http.Request) { w.Write(dbBytes) })
	mux.HandleFunc("/e2elib-1.0-1-x86_64.pkg.tar", func(w http.ResponseWriter, r *http.Request) { w.Write(pkgBytes) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	idx := repo.NewTestRepo("core", srv.URL, filepath.Join(home, "cache", "repo"), filepath.Join(home, "pkgcache"))
	repo.OverrideDefault(idx)

	if err := InstallElf(folder, "", InstallOptions{}); err != nil {
		t.Fatalf("InstallElf folder: %v", err)
	}
	m, err := manifest.Load(filepath.Join(home, "apps", "folderapp"))
	if err != nil {
		t.Fatal(err)
	}
	if m.ExeRel != filepath.Join("pkg", "folderapp") {
		t.Fatalf("manifest ExeRel = %q", m.ExeRel)
	}
	link := filepath.Join(home, "bin", "folderapp")
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(home, "exe", "folderapp", "pkg", "folderapp") {
		t.Fatalf("symlink -> %q,%v", target, err)
	}
	if data, err := os.ReadFile(filepath.Join(home, "exe", "folderapp", "pkg", "lib", "folderapp", "data.txt")); err != nil || string(data) != "folder-bytes" {
		t.Fatalf("folder resource = %q,%v", data, err)
	}
	if err := exec.Command(link).Run(); err == nil {
		t.Fatal("expected exit code 42, got 0")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 42 {
		t.Fatalf("symlink run = %v, want exit 42", err)
	}
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type tarEntry struct{ name, body string }

func gzipTar(entries []tarEntry) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0644, Size: int64(len(e.body))})
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}
