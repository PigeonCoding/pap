package install

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pap/internal/config"
	"pap/internal/elf"
	"pap/internal/manifest"
	"pap/internal/repo"
	"pap/internal/resolver"
)

// buildRepoE2E stages a fake app package (usr/bin/<app> linked against
// libe2e.so.1) and a dep package (usr/lib/libe2e.so.1), served over httptest
// with a live core.db describing both. builtVer is the version actually
// packaged; advertisedVer is what the live db claims (differs to simulate a
// mirror that moved past the pinned version; its bytes are not served).
func buildRepoE2E(t *testing.T, work, appName, builtVer, advertisedVer string) (mux *http.ServeMux, appFile, libFile string) {
	t.Helper()
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
	libBytes, _ := os.ReadFile(lib)
	appBytes, _ := os.ReadFile(appBin)

	mkpkg := func(pkgname, pkgver, libPath, libDst, binDst string) (file string, desc string) {
		staging := filepath.Join(work, "pkgroot-"+pkgname)
		os.MkdirAll(filepath.Join(staging, "usr", "lib"), 0755)
		os.MkdirAll(filepath.Join(staging, "usr", "bin"), 0755)
		if libPath != "" {
			os.WriteFile(filepath.Join(staging, libDst), libBytes, 0755)
		}
		if binDst != "" {
			os.WriteFile(filepath.Join(staging, binDst), appBytes, 0755)
		}
		os.WriteFile(filepath.Join(staging, ".PKGINFO"),
			[]byte("pkgname = "+pkgname+"\npkgver = "+pkgver+"\narch = "+config.Arch+"\n"), 0644)
		var members []string
		if libPath != "" {
			members = append(members, libDst)
		}
		if binDst != "" {
			members = append(members, binDst)
		}
		members = append(members, ".PKGINFO")
		pkgFile := filepath.Join(work, pkgname+"-"+pkgver+"-"+config.Arch+".pkg.tar")
		args := append([]string{"-cf", pkgFile, "-C", staging}, members...)
		if out, err := exec.Command("bsdtar", args...).CombinedOutput(); err != nil {
			t.Fatalf("bsdtar pkg %s: %v %s", pkgname, err, out)
		}
		pkgData, _ := os.ReadFile(pkgFile)
		h := sha256.Sum256(pkgData)
		sha := hex.EncodeToString(h[:])
		fn := pkgname + "-" + pkgver + "-" + config.Arch + ".pkg.tar"
		d := "%FILENAME%\n" + fn + "\n\n%NAME%\n" + pkgname + "\n\n%VERSION%\n" + pkgver +
			"\n\n%ARCH%\n" + config.Arch + "\n\n%SHA256SUM%\n" + sha + "\n"
		return pkgFile, d
	};

	appPkg, _ := mkpkg(appName, builtVer, "", "", "usr/bin/"+appName)
	libPkg, libDesc := mkpkg("e2elib", "1.0-1", lib, "usr/lib/libe2e.so.1", "")
	libDesc += "\n%PROVIDES%\nlibe2e.so=1-64\n"

	// The live db advertises advertisedVer (bytes only exist for builtVer).
	appData, _ := os.ReadFile(appPkg)
	advSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if advertisedVer == builtVer {
		h := sha256.Sum256(appData)
		advSHA = hex.EncodeToString(h[:])
	}
	advFn := appName + "-" + advertisedVer + "-" + config.Arch + ".pkg.tar"
	advDesc := "%FILENAME%\n" + advFn + "\n\n%NAME%\n" + appName + "\n\n%VERSION%\n" + advertisedVer +
		"\n\n%ARCH%\n" + config.Arch + "\n\n%SHA256SUM%\n" + advSHA + "\n"

	libData, _ := os.ReadFile(libPkg)
	dbBytes := gzipTar([]tarEntry{
		{appName + "-" + advertisedVer + "/desc", advDesc},
		{"e2elib-1.0-1/desc", libDesc},
	})

	mux = http.NewServeMux()
	mux.HandleFunc("/core.db", func(w http.ResponseWriter, r *http.Request) { w.Write(dbBytes) })
	mux.HandleFunc("/"+filepath.Base(appPkg), func(w http.ResponseWriter, r *http.Request) { w.Write(appData) })
	mux.HandleFunc("/"+filepath.Base(libPkg), func(w http.ResponseWriter, r *http.Request) { w.Write(libData) })
	return mux, appPkg, libPkg
}

func TestInstallRepoAppE2E(t *testing.T) {
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

	mux, _, _ := buildRepoE2E(t, work, "e2eapp", "1.0-1", "1.0-1")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	idx := repo.NewTestRepo("core", srv.URL, filepath.Join(home, "cache", "repo"), filepath.Join(home, "pkgcache"))
	repo.OverrideDefault(idx)

	if err := InstallRepoApp("e2eapp", "", "e2eapp", InstallOptions{}); err != nil {
		t.Fatalf("InstallRepoApp: %v", err)
	}
	m, err := manifest.Load(filepath.Join(home, "apps", "e2eapp"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "repo:e2eapp" {
		t.Fatalf("manifest Source = %q, want repo:e2eapp", m.Source)
	}
	if m.Package != "core/e2eapp 1.0-1" {
		t.Fatalf("manifest Package = %q, want core/e2eapp 1.0-1", m.Package)
	}
	if m.ExeRel != filepath.Join("pkg", "usr", "bin", "e2eapp") {
		t.Fatalf("manifest ExeRel = %q", m.ExeRel)
	}
	link := filepath.Join(home, "bin", "e2eapp")
	if err := exec.Command(link).Run(); err == nil {
		t.Fatal("expected exit code 42, got 0")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 42 {
		t.Fatalf("symlink run = %v, want exit 42", err)
	}

	// Reinstall re-fetches the pinned version; upgrade re-resolves latest.
	if err := ReinstallWithOptions("e2eapp", InstallOptions{}); err != nil {
		t.Fatalf("reinstall repo app: %v", err)
	}
	if err := UpgradeWithOptions("e2eapp", InstallOptions{}); err != nil {
		t.Fatalf("upgrade repo app: %v", err)
	}
}

// TestInstallRepoAppPinnedVersion installs an exact older version that the
// live index does not carry, via the Archive packages/ endpoint.
func TestInstallRepoAppPinnedVersion(t *testing.T) {
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

	mux, appPkg, _ := buildRepoE2E(t, work, "e2eapp", "0.9-1", "1.0-1")
	// Live db advertises only 1.0-1 (bytes unavailable → live 404s).
	appData09, _ := os.ReadFile(appPkg)
	mux.HandleFunc("/packages/e/e2eapp/e2eapp-0.9-1-"+config.Arch+".pkg.tar.zst",
		func(w http.ResponseWriter, r *http.Request) { w.Write(appData09) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	origBase := repo.ArchiveBaseURL
	repo.ArchiveBaseURL = srv.URL
	t.Cleanup(func() { repo.ArchiveBaseURL = origBase })

	idx := repo.NewTestRepo("core", srv.URL, filepath.Join(home, "cache", "repo"), filepath.Join(home, "pkgcache"))
	repo.OverrideDefault(idx)

	if err := InstallRepoApp("e2eapp", "0.9-1", "e2eold", InstallOptions{}); err != nil {
		t.Fatalf("InstallRepoApp pinned: %v", err)
	}
	m, err := manifest.Load(filepath.Join(home, "apps", "e2eold"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Package != "archive/e2eapp 0.9-1" {
		t.Fatalf("manifest Package = %q, want archive/e2eapp 0.9-1", m.Package)
	}
	link := filepath.Join(home, "bin", "e2eold")
	if err := exec.Command(link).Run(); err == nil {
		t.Fatal("expected exit code 42, got 0")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 42 {
		t.Fatalf("symlink run = %v, want exit 42", err)
	}
}

func TestFindRepoMainExe(t *testing.T) {
	payload := t.TempDir()
	os.MkdirAll(filepath.Join(payload, "usr", "bin"), 0755)
	os.WriteFile(filepath.Join(payload, "usr", "bin", "myapp"), []byte("#!/bin/sh\n"), 0755)
	os.WriteFile(filepath.Join(payload, "usr", "bin", "helper"), []byte("data"), 0644)
	if rel, err := findRepoMainExe(payload, "mypkg", "myapp"); err != nil || rel != filepath.Join("usr", "bin", "myapp") {
		t.Fatalf("main = %q, %v", rel, err)
	}
	// Falls back to the first executable when names match nothing.
	os.Remove(filepath.Join(payload, "usr", "bin", "myapp"))
	os.WriteFile(filepath.Join(payload, "usr", "bin", "other"), []byte("#!/bin/sh\n"), 0755)
	if rel, err := findRepoMainExe(payload, "mypkg", "myapp"); err != nil || rel != filepath.Join("usr", "bin", "other") {
		t.Fatalf("fallback main = %q, %v", rel, err)
	}
	// Library-only payloads fail with a helpful error.
	os.Remove(filepath.Join(payload, "usr", "bin", "other"))
	if _, err := findRepoMainExe(payload, "mylib", "mylib"); err == nil {
		t.Fatal("expected error for library-only package")
	}
}

// A script shipped beside the real binary (corepack next to node) only wraps
// it and execs a name the host PATH may not have, so the fallback must pick
// the ELF. Scripts remain valid when the package ships no ELF at all.
func TestFindRepoMainExePrefersELFOverScript(t *testing.T) {
	payload := t.TempDir()
	bindir := filepath.Join(payload, "usr", "bin")
	if err := os.MkdirAll(bindir, 0755); err != nil {
		t.Fatal(err)
	}
	elfBytes, err := os.ReadFile(hostELFPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindir, "corepack"), []byte("#!/usr/bin/env node\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindir, "node"), elfBytes, 0755); err != nil {
		t.Fatal(err)
	}
	// corepack sorts before node; the ELF still wins.
	if rel, err := findRepoMainExe(payload, "nodejs", "node_old"); err != nil || rel != filepath.Join("usr", "bin", "node") {
		t.Fatalf("main = %q, %v, want usr/bin/node", rel, err)
	}
	// Script-only packages fall back to the script as before.
	if err := os.Remove(filepath.Join(bindir, "node")); err != nil {
		t.Fatal(err)
	}
	if rel, err := findRepoMainExe(payload, "nodejs", "node_old"); err != nil || rel != filepath.Join("usr", "bin", "corepack") {
		t.Fatalf("script-only main = %q, %v, want usr/bin/corepack", rel, err)
	}
}

func TestSplitPinnedPackage(t *testing.T) {
	name, ver, ok := splitPinnedPackage("core/curl 8.11.1-3")
	if !ok || name != "curl" || ver != "8.11.1-3" {
		t.Fatalf("split = %q %q %v", name, ver, ok)
	}
	if _, _, ok := splitPinnedPackage("garbage"); ok {
		t.Fatal("bad pin accepted")
	}
	if pkg, ok := splitRepoSource("repo:curl"); !ok || pkg != "curl" {
		t.Fatalf("source split = %q %v", pkg, ok)
	}
	if _, ok := splitRepoSource("/some/path"); ok {
		t.Fatal("local path treated as repo source")
	}
}

// TestGlibcCompatClosureRetryE2E covers the case the app-level pre-check
// cannot see: the live app binary runs on the host glibc, but its live
// dependency closure does not. The install must then continue with the next
// older archived build (era-consistent deps from the snapshot) instead of
// failing. The too-new dep is simulated with a library requiring a
// fictitious GLIBC_99.0 from the host libc.
func TestGlibcCompatClosureRetryE2E(t *testing.T) {
	for _, bin := range []string{"gcc", "bsdtar", "patchelf"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s unavailable", bin)
		}
	}
	home := isolate(t)
	work := t.TempDir()

	// badlib defines GLIBC_99.0 under the libc SONAME, so anything linked
	// against it needs GLIBC_99.0 from the host libc — unsatisfiable.
	os.WriteFile(filepath.Join(work, "bad.c"), []byte("int bad_fn(void){return 3;}\n"), 0644)
	os.WriteFile(filepath.Join(work, "bad.map"), []byte("GLIBC_99.0 {\n  global: bad_fn;\n  local: *;\n};\n"), 0644)
	cmd := exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libc.so.6",
		"-Wl,--version-script="+filepath.Join(work, "bad.map"),
		"-o", filepath.Join(work, "libbad.so.0"), filepath.Join(work, "bad.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc versioned libc-soname lib: %v %s", err, out)
	}
	// BAD dep lib: same ABI as good, but linked against badlib, so its
	// VERNEED demands GLIBC_99.0 from the host. e2e_fn returns 43 to prove
	// at runtime that this build never ends up installed.
	os.WriteFile(filepath.Join(work, "e2ebad.c"), []byte("extern int bad_fn(void); int e2e_fn(void){return 40 + bad_fn();}\n"), 0644)
	cmd = exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libe2e.so.1",
		"-o", filepath.Join(work, "libe2e_bad.so"), filepath.Join(work, "e2ebad.c"),
		"-L"+work, "-l:libbad.so.0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gcc bad dep lib: %v %s", err, out)
	}
	if maj, min, ok := RequiredGlibc(filepath.Join(work, "libe2e_bad.so")); !ok || maj != 99 || min != 0 {
		t.Fatalf("fixture broken: bad dep requires GLIBC_%d.%d (ok=%v), want 99.0", maj, min, ok)
	}
	// GOOD dep lib: returns 42, no exotic requirements.
	os.WriteFile(filepath.Join(work, "e2e.c"), []byte("int e2e_fn(void){return 42;}\n"), 0644)
	cmd = exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libe2e.so.1",
		"-o", filepath.Join(work, "libe2e_good.so"), filepath.Join(work, "e2e.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gcc good dep lib: %v %s", err, out)
	}
	// App binary: ordinary, host-compatible on its own.
	os.WriteFile(filepath.Join(work, "app.c"), []byte("extern int e2e_fn(void); int main(void){return e2e_fn();}\n"), 0644)
	cmd = exec.Command("gcc", "-o", filepath.Join(work, "appbin"), filepath.Join(work, "app.c"),
		"-L"+work, "-l:libe2e_good.so", "-Wl,-rpath,"+work)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gcc app: %v %s", err, out)
	}
	appBytes, _ := os.ReadFile(filepath.Join(work, "appbin"))
	badBytes, _ := os.ReadFile(filepath.Join(work, "libe2e_bad.so"))
	goodBytes, _ := os.ReadFile(filepath.Join(work, "libe2e_good.so"))

	mkpkg := func(pkgname, pkgver string, files map[string][]byte) (data []byte, fn string) {
		staging := filepath.Join(work, "pkgroot-"+pkgname+"-"+pkgver)
		for rel, b := range files {
			p := filepath.Join(staging, rel)
			os.MkdirAll(filepath.Dir(p), 0755)
			os.WriteFile(p, b, 0755)
		}
		os.WriteFile(filepath.Join(staging, ".PKGINFO"),
			[]byte("pkgname = "+pkgname+"\npkgver = "+pkgver+"\narch = "+config.Arch+"\n"), 0644)
		var members []string
		for rel := range files {
			members = append(members, rel)
		}
		members = append(members, ".PKGINFO")
		pkgFile := filepath.Join(work, pkgname+"-"+pkgver+"-"+config.Arch+".pkg.tar")
		args := append([]string{"-cf", pkgFile, "-C", staging}, members...)
		if out, err := exec.Command("bsdtar", args...).CombinedOutput(); err != nil {
			t.Fatalf("bsdtar pkg %s %s: %v %s", pkgname, pkgver, err, out)
		}
		data, _ = os.ReadFile(pkgFile)
		return data, pkgname + "-" + pkgver + "-" + config.Arch + ".pkg.tar"
	};
	descFor := func(pkgname, pkgver, fn string, data []byte, provides string) string {
		h := sha256.Sum256(data)
		d := "%FILENAME%\n" + fn + "\n\n%NAME%\n" + pkgname + "\n\n%VERSION%\n" + pkgver +
			"\n\n%ARCH%\n" + config.Arch + "\n\n%SHA256SUM%\n" + hex.EncodeToString(h[:]) + "\n"
		if provides != "" {
			d += "\n%PROVIDES%\n" + provides + "\n"
		}
		return d
	}

	appV2, appV2Fn := mkpkg("e2eapp", "2.0-1", map[string][]byte{"usr/bin/e2eapp": appBytes})
	libBad, libBadFn := mkpkg("e2elib", "1.0-1", map[string][]byte{"usr/lib/libe2e.so.1": badBytes})
	appV1, _ := mkpkg("e2eapp", "1.0-1", map[string][]byte{"usr/bin/e2eapp": appBytes})
	libGood, libGoodFn := mkpkg("e2elib", "1.0-1", map[string][]byte{"usr/lib/libe2e.so.1": goodBytes})

	liveDB := gzipTar([]tarEntry{
		{"e2eapp-2.0-1/desc", descFor("e2eapp", "2.0-1", appV2Fn, appV2, "")},
		{"e2elib-1.0-1/desc", descFor("e2elib", "1.0-1", libBadFn, libBad, "libe2e.so=1-64")},
	})
	// Era snapshot: same soname, GOOD dep build (plus an app entry for shape).
	snapDB := gzipTar([]tarEntry{
		{"e2eapp-1.0-1/desc", descFor("e2eapp", "1.0-1", "e2eapp-1.0-1-"+config.Arch+".pkg.tar", appV1, "")},
		{"e2elib-1.0-1/desc", descFor("e2elib", "1.0-1", libGoodFn, libGood, "libe2e.so=1-64")},
	})
	emptyDB := gzipTar(nil)
	listing := `<html><body>` +
		`<a href="e2eapp-2.0-1-` + config.Arch + `.pkg.tar.zst">v2</a>` +
		`<a href="e2eapp-1.0-1-` + config.Arch + `.pkg.tar.zst">v1</a>` +
		`</body></html>`

	mux := http.NewServeMux()
	mux.HandleFunc("/core.db", func(w http.ResponseWriter, r *http.Request) { w.Write(liveDB) })
	mux.HandleFunc("/"+appV2Fn, func(w http.ResponseWriter, r *http.Request) { w.Write(appV2) })
	mux.HandleFunc("/"+libBadFn, func(w http.ResponseWriter, r *http.Request) { w.Write(libBad) })
	mux.HandleFunc("/packages/e/e2eapp/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".pkg.tar.zst") {
			w.Write(appV1)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(listing))
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "core.db"):
			w.Write(snapDB)
		case strings.HasSuffix(r.URL.Path, "extra.db") || strings.HasSuffix(r.URL.Path, "multilib.db"):
			w.Write(emptyDB)
		default:
			w.Write(libGood)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := repo.ArchiveBaseURL
	repo.ArchiveBaseURL = srv.URL
	t.Cleanup(func() { repo.ArchiveBaseURL = origBase })
	repo.ResetArchiveCache()
	idx := repo.NewTestRepo("core", srv.URL, filepath.Join(home, "cache", "repo"), filepath.Join(home, "pkgcache"))
	repo.OverrideDefault(idx)
	testArchiveAccess = true
	t.Cleanup(func() { testArchiveAccess = false })

	var out strings.Builder
	if err := InstallRepoApp("e2eapp", "", "e2ecompat", InstallOptions{Out: &out}); err != nil {
		t.Fatalf("InstallRepoApp with closure retry: %v\n%s", err, out.String())
	}
	m, err := manifest.Load(filepath.Join(home, "apps", "e2ecompat"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Package != "archive/e2eapp 1.0-1" {
		t.Fatalf("manifest Package = %q, want archive/e2eapp 1.0-1 (older build after closure retry)", m.Package)
	}
	link := filepath.Join(home, "bin", "e2ecompat")
	if err := exec.Command(link).Run(); err == nil {
		t.Fatal("expected exit code 42, got 0")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 42 {
		t.Fatalf("symlink run = %v, want exit 42 (good dep build)", err)
	}
}

// TestForceLibcE2E installs with --force-libc: the glibc/loader coupling is
// vendored from a fixture glibc package (host file copies, declared via
// %PROVIDES%) instead of staying on the host, and PT_INTERP points at the
// vendored loader.
func TestForceLibcE2E(t *testing.T) {
	for _, bin := range []string{"gcc", "bsdtar", "patchelf"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s unavailable", bin)
		}
	}
	home := isolate(t)
	work := t.TempDir()

	// Fixture glibc package: real host files (valid ELFs with proper
	// SONAMEs and version definitions), declared via %PROVIDES%.
	hostLibs := map[string]string{
		"libc.so.6":             "/usr/lib/libc.so.6",
		"libm.so.6":             "/usr/lib/libm.so.6",
		"ld-linux-x86-64.so.2":  "/usr/lib/ld-linux-x86-64.so.2",
		"libnss_files.so.2":     "/usr/lib/libnss_files.so.2",
		"libnss_dns.so.2":       "/usr/lib/libnss_dns.so.2",
		"libnss_compat.so.2":    "/usr/lib/libnss_compat.so.2",
	}
	if config.Arch != "x86_64" {
		t.Skip("fixture loader name is x86_64-specific")
	}
	glibcStaging := filepath.Join(work, "pkgroot-glibc")
	for soname, host := range hostLibs {
		data, err := os.ReadFile(host)
		if err != nil {
			t.Skipf("host lib %s unavailable: %v", host, err)
		}
		p := filepath.Join(glibcStaging, "usr", "lib", soname)
		os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.WriteFile(p, data, 0755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(glibcStaging, ".PKGINFO"),
		[]byte("pkgname = e2eglibc\npkgver = 1.0-1\narch = "+config.Arch+"\n"), 0644)
	glibcPkg := filepath.Join(work, "e2eglibc-1.0-1-"+config.Arch+".pkg.tar")
	if out, err := exec.Command("bsdtar", "-cf", glibcPkg, "-C", glibcStaging,
		"usr/lib/libc.so.6", "usr/lib/libm.so.6", "usr/lib/ld-linux-x86-64.so.2",
		"usr/lib/libnss_files.so.2", "usr/lib/libnss_dns.so.2", "usr/lib/libnss_compat.so.2",
		".PKGINFO").CombinedOutput(); err != nil {
		t.Fatalf("bsdtar glibc pkg: %v %s", err, out)
	}
	glibcData, _ := os.ReadFile(glibcPkg)
	glibcSHA := sha256.Sum256(glibcData)
	glibcFn := "e2eglibc-1.0-1-" + config.Arch + ".pkg.tar"
	glibcDesc := "%FILENAME%\n" + glibcFn + "\n\n%NAME%\ne2eglibc\n\n%VERSION%\n1.0-1" +
		"\n\n%ARCH%\n" + config.Arch + "\n\n%SHA256SUM%\n" + hex.EncodeToString(glibcSHA[:]) +
		"\n\n%PROVIDES%\nlibc.so=6-64\nlibm.so=6-64\nld-linux-x86-64.so.2\n" +
		"libnss_files.so=2-64\nlibnss_dns.so=2-64\nlibnss_compat.so=2-64\n"

	// Ordinary app + dep (exit 42).
	os.WriteFile(filepath.Join(work, "e2e.c"), []byte("int e2e_fn(void){return 42;}\n"), 0644)
	cmd := exec.Command("gcc", "-shared", "-fPIC", "-Wl,-soname,libe2e.so.1",
		"-o", filepath.Join(work, "libe2e.so.1.0"), filepath.Join(work, "e2e.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc lib: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(work, "app.c"), []byte("extern int e2e_fn(void); int main(void){return e2e_fn();}\n"), 0644)
	cmd = exec.Command("gcc", "-o", filepath.Join(work, "appbin"), filepath.Join(work, "app.c"),
		"-L"+work, "-l:libe2e.so.1.0", "-Wl,-rpath,"+work)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gcc app: %v %s", err, out)
	}
	libBytes, _ := os.ReadFile(filepath.Join(work, "libe2e.so.1.0"))
	appBytes, _ := os.ReadFile(filepath.Join(work, "appbin"))

	mkpkg := func(pkgname, pkgver string, files map[string][]byte) (data []byte, fn string) {
		staging := filepath.Join(work, "pkgroot-"+pkgname)
		for rel, b := range files {
			p := filepath.Join(staging, rel)
			os.MkdirAll(filepath.Dir(p), 0755)
			os.WriteFile(p, b, 0755)
		}
		os.WriteFile(filepath.Join(staging, ".PKGINFO"),
			[]byte("pkgname = "+pkgname+"\npkgver = "+pkgver+"\narch = "+config.Arch+"\n"), 0644)
		var members []string
		for rel := range files {
			members = append(members, rel)
		}
		members = append(members, ".PKGINFO")
		pkgFile := filepath.Join(work, pkgname+"-"+pkgver+"-"+config.Arch+".pkg.tar")
		args := append([]string{"-cf", pkgFile, "-C", staging}, members...)
		if out, err := exec.Command("bsdtar", args...).CombinedOutput(); err != nil {
			t.Fatalf("bsdtar pkg %s: %v %s", pkgname, err, out)
		}
		data, _ = os.ReadFile(pkgFile)
		return data, pkgname + "-" + pkgver + "-" + config.Arch + ".pkg.tar"
	};
	descFor := func(pkgname, pkgver, fn string, data []byte, provides string) string {
		h := sha256.Sum256(data)
		d := "%FILENAME%\n" + fn + "\n\n%NAME%\n" + pkgname + "\n\n%VERSION%\n" + pkgver +
			"\n\n%ARCH%\n" + config.Arch + "\n\n%SHA256SUM%\n" + hex.EncodeToString(h[:]) + "\n"
		if provides != "" {
			d += "\n%PROVIDES%\n" + provides + "\n"
		}
		return d
	}
	appData, appFn := mkpkg("e2eapp", "1.0-1", map[string][]byte{"usr/bin/e2eapp": appBytes})
	libData, libFn := mkpkg("e2elib", "1.0-1", map[string][]byte{"usr/lib/libe2e.so.1": libBytes})
	dbBytes := gzipTar([]tarEntry{
		{"e2eapp-1.0-1/desc", descFor("e2eapp", "1.0-1", appFn, appData, "")},
		{"e2elib-1.0-1/desc", descFor("e2elib", "1.0-1", libFn, libData, "libe2e.so=1-64")},
		{"e2eglibc-1.0-1/desc", glibcDesc},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/core.db", func(w http.ResponseWriter, r *http.Request) { w.Write(dbBytes) })
	mux.HandleFunc("/"+appFn, func(w http.ResponseWriter, r *http.Request) { w.Write(appData) })
	mux.HandleFunc("/"+libFn, func(w http.ResponseWriter, r *http.Request) { w.Write(libData) })
	mux.HandleFunc("/"+glibcFn, func(w http.ResponseWriter, r *http.Request) { w.Write(glibcData) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	idx := repo.NewTestRepo("core", srv.URL, filepath.Join(home, "cache", "repo"), filepath.Join(home, "pkgcache"))
	repo.OverrideDefault(idx)

	if err := InstallRepoApp("e2eapp", "", "e2eforcelibc", InstallOptions{ForceLibc: true}); err != nil {
		t.Fatalf("InstallRepoApp --force-libc: %v", err)
	}
	m, err := manifest.Load(filepath.Join(home, "apps", "e2eforcelibc"))
	if err != nil {
		t.Fatal(err)
	}
	if !m.ForceLibc {
		t.Fatal("manifest ForceLibc = false, want true")
	}
	for _, soname := range []string{"libc.so.6", "ld-linux-x86-64.so.2", "libnss_files.so.2"} {
		if _, err := os.Stat(filepath.Join(home, "apps", "e2eforcelibc", "libs", soname)); err != nil {
			t.Fatalf("vendored %s missing: %v", soname, err)
		}
	}
	// PT_INTERP must point at the vendored loader, not the host's.
	loader := resolver.LoaderSonames(config.Arch)[0]
	link := filepath.Join(home, "bin", "e2eforcelibc")
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	info, err := elf.Parse(target)
	if err != nil {
		t.Fatalf("parse installed binary: %v", err)
	}
	wantInterp := filepath.Join(home, "apps", "e2eforcelibc", "libs", loader)
	if info.Interp != wantInterp {
		t.Fatalf("INTERP = %q, want vendored %q", info.Interp, wantInterp)
	}
	if err := exec.Command(link).Run(); err == nil {
		t.Fatal("expected exit code 42, got 0")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 42 {
		t.Fatalf("vendored-libc run = %v, want exit 42", err)
	}
}
