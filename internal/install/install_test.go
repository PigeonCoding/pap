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
	"strings"
	"testing"
	"time"

	"pap/internal/config"
	"pap/internal/elf"
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

// copyTree must carry mtimes across: ArchiveDateFromRoots reads them to pick
// the ALA snapshot era for dependency resolution, and a copy that reset them
// would resolve every payload against today's repos.
func TestCopyTreePreservesMtimes(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "bin.elf"), []byte("data"), 0755); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2025, 9, 30, 22, 34, 14, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "bin.elf"), mt, mt); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dst, "bin.elf"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(mt) {
		t.Errorf("copied mtime = %v, want %v", st.ModTime(), mt)
	}
	if got := repo.ArchiveDateFromRoots([]string{filepath.Join(dst, "bin.elf")}); !got.Equal(mt) {
		t.Errorf("ArchiveDateFromRoots = %v, want %v", got, mt)
	}
}

func TestUninstallWithoutManifestLeavesBin(t *testing.T) {
	home := isolate(t)
	// Uninstall without manifest must NOT touch bin.
	if err := os.MkdirAll(filepath.Join(home, "apps", "ghost"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0755); err != nil {
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

func TestRepoAppDryRunMakesNoChanges(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc unavailable")
	}
	if _, err := exec.LookPath("bsdtar"); err != nil {
		t.Skip("bsdtar unavailable")
	}
	home := isolate(t)
	work := t.TempDir()

	mux, _, _ := buildRepoE2E(t, work, "e2eapp", "1.0-1", "1.0-1")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	idx := repo.NewTestRepo("core", srv.URL, filepath.Join(home, "cache", "repo"), filepath.Join(home, "pkgcache"))
	repo.OverrideDefault(idx)

	if err := InstallRepoApp("e2eapp", "", "e2edry", InstallOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "apps", "e2edry")); !os.IsNotExist(err) {
		t.Fatal("dry-run created app dir")
	}
	if _, err := os.Stat(filepath.Join(home, "exe", "e2edry")); !os.IsNotExist(err) {
		t.Fatal("dry-run created exe dir")
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

	if err := installDir(folder, "folderapp", InstallOptions{}); err != nil {
		t.Fatalf("installDir folder: %v", err)
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

// TestInstallDirRepoFallbackE2E covers the mixed-closure case: the folder
// bundles libfb.so.1 WITHOUT the FB_1.0 symbol version, while the repo
// build has it and the app's requirer needs it. The install must heal by
// re-staging the soname from the repo instead of failing compat.
func TestInstallDirRepoFallbackE2E(t *testing.T) {
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

	os.WriteFile(filepath.Join(work, "fb.c"), []byte("int fb_fn(void){return 7;}\n"), 0644)
	os.WriteFile(filepath.Join(work, "fb.map"), []byte("FB_1.0 {\n  global: fb_fn;\n  local: *;\n};\n"), 0644)
	// Repo build: versioned symbols.
	repoLib := filepath.Join(work, "libfb.so.1.0")
	cmd := exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libfb.so.1",
		"-Wl,--version-script="+filepath.Join(work, "fb.map"), "-o", repoLib, filepath.Join(work, "fb.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc versioned lib: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(work, "app.c"), []byte("extern int fb_fn(void); int main(void){return fb_fn();}\n"), 0644)
	folder := filepath.Join(work, "fbapp")
	os.MkdirAll(folder, 0755)
	cmd = exec.Command("gcc", "-o", filepath.Join(folder, "fbapp"), filepath.Join(work, "app.c"),
		"-L"+work, "-l:libfb.so.1.0", "-Wl,-rpath,"+work)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc app: %v %s", err, out)
	}
	// Bundled copy: same ABI, no symbol versions.
	localLib := filepath.Join(folder, "libfb.so.1.0")
	cmd = exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libfb.so.1", "-o", localLib, filepath.Join(work, "fb.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc unversioned lib: %v %s", err, out)
	}

	pkgStaging := filepath.Join(work, "pkgroot")
	os.MkdirAll(filepath.Join(pkgStaging, "usr", "lib"), 0755)
	libBytes, _ := os.ReadFile(repoLib)
	os.WriteFile(filepath.Join(pkgStaging, "usr", "lib", "libfb.so.1"), libBytes, 0755)
	pkginfo := "pkgname = e2efb\npkgver = 1.0-1\narch = " + config.Arch + "\n"
	os.WriteFile(filepath.Join(pkgStaging, ".PKGINFO"), []byte(pkginfo), 0644)
	pkgFile := filepath.Join(work, "e2efb-1.0-1-"+config.Arch+".pkg.tar")
	if out, err := exec.Command("bsdtar", "-cf", pkgFile, "-C", pkgStaging, "usr/lib/libfb.so.1", ".PKGINFO").CombinedOutput(); err != nil {
		t.Fatalf("bsdtar pkg: %v %s", err, out)
	}
	pkgBytes, _ := os.ReadFile(pkgFile)
	h := sha256.Sum256(pkgBytes)
	sha := hex.EncodeToString(h[:])

	desc := "%FILENAME%\ne2efb-1.0-1-" + config.Arch + ".pkg.tar\n\n%NAME%\ne2efb\n\n%VERSION%\n1.0-1\n\n%ARCH%\n" + config.Arch + "\n\n%SHA256SUM%\n" + sha + "\n\n%PROVIDES%\nlibfb.so=1-64\n"
	dbBytes := gzipTar([]tarEntry{{"e2efb-1.0-1/desc", desc}})

	mux := http.NewServeMux()
	mux.HandleFunc("/core.db", func(w http.ResponseWriter, r *http.Request) { w.Write(dbBytes) })
	mux.HandleFunc("/e2efb-1.0-1-"+config.Arch+".pkg.tar", func(w http.ResponseWriter, r *http.Request) { w.Write(pkgBytes) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	idx := repo.NewTestRepo("core", srv.URL, filepath.Join(home, "cache", "repo"), filepath.Join(home, "pkgcache"))
	repo.OverrideDefault(idx)

	var out bytes.Buffer
	if err := installDir(folder, "fbapp", InstallOptions{Out: &out}); err != nil {
		t.Fatalf("installDir folder fallback: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "[repo-fallback] libfb.so.1") {
		t.Fatalf("no repo fallback in output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "apps", "fbapp", "libs", "libfb.so.1")); err != nil {
		t.Fatalf("vendored lib missing: %v", err)
	}
	// The staged copy must be the versioned repo build.
	defs, err := elf.ParseVerdef(filepath.Join(home, "apps", "fbapp", "libs", "libfb.so.1"))
	if err != nil {
		t.Fatalf("ParseVerdef staged lib: %v", err)
	}
	if !defs["FB_1.0"] {
		t.Fatalf("staged lib lacks FB_1.0: %v", defs)
	}
	link := filepath.Join(home, "bin", "fbapp")
	if err := exec.Command(link).Run(); err == nil {
		t.Fatal("expected exit code 7, got 0")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("symlink run = %v, want exit 7", err)
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
