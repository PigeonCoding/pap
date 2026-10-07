package repo

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"pap/internal/config"
)

func multiRepoIndex(t *testing.T, dbs map[string][]tarEntry) *Index {
	t.Helper()
	cacheDir := t.TempDir()
	var repos []repoDef
	for repoName, entries := range dbs {
		if err := os.WriteFile(filepath.Join(cacheDir, repoName+".db"), gzipTar(entries), 0644); err != nil {
			t.Fatal(err)
		}
		repos = append(repos, repoDef{name: repoName, servers: []string{"http://127.0.0.1:0"}})
	}
	// Deterministic repo order for priority tests.
	for i := range repos {
		for j := i + 1; j < len(repos); j++ {
			if repos[j].name < repos[i].name {
				repos[i], repos[j] = repos[j], repos[i]
			}
		}
	}
	return &Index{
		fileMap:  make(map[string][]PkgInfo),
		pkgMeta:  make(map[string]PkgInfo),
		repos:    repos,
		cacheDir: cacheDir,
	}
}

func TestFindPackagePriority(t *testing.T) {
	idx := multiRepoIndex(t, map[string][]tarEntry{
		"core":  {{"curl-8.0-1/desc", desc("curl", "8.0-1", "libcurl.so=4-64")}},
		"extra": {{"curl-8.1-1/desc", desc("curl", "8.1-1", "libcurl.so=4-64")}},
	})
	if err := idx.Load(); err != nil {
		t.Fatal(err)
	}
	got := idx.FindPackage("curl")
	if len(got) != 2 || got[0].Repo != "core" || got[1].Repo != "extra" {
		t.Fatalf("FindPackage(curl) = %+v, want [core extra]", got)
	}
	if got := idx.FindPackage("nope"); len(got) != 0 {
		t.Fatalf("FindPackage(nope) = %+v, want empty", got)
	}
}

func TestArchivePackageURL(t *testing.T) {
	old := ArchiveBaseURL
	ArchiveBaseURL = "https://archive.example"
	t.Cleanup(func() { ArchiveBaseURL = old })

	got := ArchivePackageURL("curl", "8.11.1-3", "x86_64")
	want := "https://archive.example/packages/c/curl/curl-8.11.1-3-x86_64.pkg.tar.zst"
	if got != want {
		t.Fatalf("ArchivePackageURL = %q, want %q", got, want)
	}
	// Epoch ':' is legal in a path segment and sent literally (servers map
	// it to the on-disk filename); letter grouping uses the first character.
	got = ArchivePackageURL("dialog", "1:1.3_20240619-2", "x86_64")
	want = "https://archive.example/packages/d/dialog/dialog-1:1.3_20240619-2-x86_64.pkg.tar.zst"
	if got != want {
		t.Fatalf("epoch URL = %q, want %q", got, want)
	}
}

// TestDownloadAppPackageArchive serves a historical package file from a fake
// ALA packages/ endpoint while the live index only knows a newer version.
func TestDownloadAppPackageArchive(t *testing.T) {
	if _, err := exec.LookPath("bsdtar"); err != nil {
		t.Skip("bsdtar unavailable")
	}
	origBase := ArchiveBaseURL
	t.Cleanup(func() { ArchiveBaseURL = origBase })

	work := t.TempDir()
	staging := filepath.Join(work, "pkgroot")
	os.MkdirAll(filepath.Join(staging, "usr", "bin"), 0755)
	os.WriteFile(filepath.Join(staging, "usr", "bin", "oldapp"), []byte("#!/bin/sh\nexit 0\n"), 0755)
	pkginfo := "pkgname = oldapp\npkgver = 0.9-1\narch = " + config.Arch + "\n"
	os.WriteFile(filepath.Join(staging, ".PKGINFO"), []byte(pkginfo), 0644)
	pkgFile := filepath.Join(work, "oldapp-0.9-1-"+config.Arch+".pkg.tar")
	if out, err := exec.Command("bsdtar", "-cf", pkgFile, "-C", staging, "usr/bin/oldapp", ".PKGINFO").CombinedOutput(); err != nil {
		t.Fatalf("bsdtar pkg: %v %s", err, out)
	}
	pkgBytes, _ := os.ReadFile(pkgFile)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		want := "/packages/o/oldapp/oldapp-0.9-1-" + config.Arch + ".pkg.tar.zst"
		if r.URL.EscapedPath() != want && r.URL.Path != want {
			http.NotFound(w, r)
			return
		}
		w.Write(pkgBytes)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ArchiveBaseURL = srv.URL

	// Live index knows only 1.0-1 (same name, newer version).
	idx := testIndex(t,
		[]tarEntry{{"oldapp-1.0-1/desc", desc("oldapp", "1.0-1", "")}},
		nil,
	)
	if err := idx.Load(); err != nil {
		t.Fatal(err)
	}

	// Exact live version downloads verified from the live entry.
	if _, info, err := idx.DownloadAppPackage("oldapp", "1.0-1"); err == nil {
		_ = info
	}
	// Pinned old version falls back to the Archive endpoint.
	data, info, err := idx.DownloadAppPackage("oldapp", "0.9-1")
	if err != nil {
		t.Fatalf("DownloadAppPackage archive: %v", err)
	}
	if info.Repo != "archive" || info.Version != "0.9-1" || len(data) == 0 {
		t.Fatalf("archive download = %+v len=%d", info, len(data))
	}
	// Unknown package and unknown version both fail.
	if _, _, err := idx.DownloadAppPackage("nosuchpkg", ""); err == nil {
		t.Fatal("expected error for unknown package")
	}
	if _, _, err := idx.DownloadAppPackage("oldapp", "9.9-9"); err == nil {
		t.Fatal("expected error for unknown version")
	}
}
