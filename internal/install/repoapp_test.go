package install

import (
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
