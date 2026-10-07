package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pap/internal/config"
	"pap/internal/repo"
)

func gzipTarArchive(entries []struct{ name, body string }) []byte {
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

func archiveDesc(name, version, provides string) string {
	return "%FILENAME%\n" + name + "-" + version + "-x86_64.pkg.tar.zst\n\n" +
		"%NAME%\n" + name + "\n\n" +
		"%VERSION%\n" + version + "\n\n" +
		"%ARCH%\nx86_64\n\n" +
		"%SHA256SUM%\nsum-" + name + "\n\n%PROVIDES%\n" + provides + "\n"
}

func isolateArchiveInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PAP_HOME", dir)
	t.Setenv("PAP_BIN_DIR", filepath.Join(dir, "bin"))
	config.Refresh()
	repo.OverrideDefault(nil)
	repo.ResetArchiveCache()
	t.Cleanup(func() {
		repo.OverrideDefault(nil)
		repo.ResetArchiveCache()
		repo.ArchiveBaseURL = "https://archive.archlinux.org"
	})
	return dir
}

// Live index knows nothing; the dated archive snapshot provides libold.so.1.
func TestArchiveScopeDateSelection(t *testing.T) {
	isolateArchiveInstall(t)

	// Explicit date wins over mtime.
	s := newArchiveScope(nil, InstallOptions{ArchiveDate: "2024-03-05"})
	if !s.enabled || s.date.Format("2006-01-02") != "2024-03-05" {
		t.Fatalf("explicit date scope = %+v", s)
	}
	// Bad date disables.
	s = newArchiveScope(nil, InstallOptions{ArchiveDate: "not-a-date"})
	if s.enabled {
		t.Fatal("bad archive date should disable fallback")
	}
	// NoArchive disables.
	s = newArchiveScope(nil, InstallOptions{NoArchive: true})
	if s.enabled {
		t.Fatal("NoArchive should disable fallback")
	}
	// Auto: mtime of first existing root.
	f := filepath.Join(t.TempDir(), "app")
	os.WriteFile(f, []byte("\x7fELF"), 0644)
	mt := time.Date(2023, 11, 20, 0, 0, 0, 0, time.UTC)
	os.Chtimes(f, mt, mt)
	s = newArchiveScope([]string{f}, InstallOptions{})
	if !s.enabled || s.date.Format("2006-01-02") != "2023-11-20" {
		t.Fatalf("auto date scope = %+v, want 2023-11-20", s)
	}
}

func TestArchiveProvidersForEndToEnd(t *testing.T) {
	home := isolateArchiveInstall(t)

	// Live index: fresh empty db so Providers is empty without network.
	liveCache := filepath.Join(home, "cache", "repo")
	os.MkdirAll(liveCache, 0755)
	if err := os.WriteFile(filepath.Join(liveCache, "core.db"), gzipTarArchive(nil), 0644); err != nil {
		t.Fatal(err)
	}
	liveIdx := repo.NewWithRepos(nil, liveCache, filepath.Join(home, "pkgcache"))
	// Seed it loaded-empty: write was fresh, Load would try network for
	// missing repos — instead override Default with an index whose repos
	// point at an unreachable server but whose cache is fresh.
	_ = liveIdx
	repo.OverrideDefault(repo.NewTestRepo("core", "http://127.0.0.1:0", liveCache, filepath.Join(home, "pkgcache")))
	if err := repo.Default().Load(); err == nil {
		// Empty but valid db may load with zero providers; either way the
		// soname below must be unknown to live.
	}
	if got := repo.Default().Providers("libold.so.1"); len(got) != 0 {
		t.Fatalf("live should not provide libold.so.1: %+v", got)
	}

	// Archive snapshot serves the provider (core only; extra/multilib 404
	// like a minimal mirror — multilib is optional, extra warns and skips).
	dbBytes := gzipTarArchive([]struct{ name, body string }{
		{"oldlib-1.0-1/desc", archiveDesc("oldlib", "1.0-1", "libold.so=1-64")},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/2025/01/14/core/os/x86_64/core.db" {
			w.Write(dbBytes)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	repo.ArchiveBaseURL = srv.URL

	s := newArchiveScope(nil, InstallOptions{ArchiveDate: "2025-01-14"})
	provs, idx, snap := s.providersFor("libold.so.1")
	if len(provs) == 0 || provs[0].Name != "oldlib" || provs[0].Version != "1.0-1" {
		t.Fatalf("archive providers = %+v", provs)
	}
	if idx == nil || snap != "2025-01-14" {
		t.Fatalf("idx=%v snap=%q", idx, snap)
	}
	// NoArchive: no fallback.
	s = newArchiveScope(nil, InstallOptions{NoArchive: true})
	if provs, _, _ := s.providersFor("libold.so.1"); len(provs) != 0 {
		t.Fatalf("NoArchive should yield no providers: %+v", provs)
	}
}
