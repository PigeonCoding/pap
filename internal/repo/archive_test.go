package repo

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pap/internal/config"
)

func isolateArchive(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PAP_HOME", dir)
	config.Refresh()
	ResetArchiveCache()
	t.Cleanup(func() {
		ResetArchiveCache()
		ArchiveBaseURL = "https://archive.archlinux.org"
	})
	return dir
}

func TestArchiveServerFor(t *testing.T) {
	d := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	got := ArchiveServerFor(d)
	want := ArchiveBaseURL + "/repos/2025/01/15/$repo/os/$arch"
	if got != want {
		t.Fatalf("ArchiveServerFor = %q, want %q", got, want)
	}
}

func TestParseArchiveDate(t *testing.T) {
	d, err := ParseArchiveDate("2025-01-15")
	if err != nil {
		t.Fatal(err)
	}
	if d.Year() != 2025 || int(d.Month()) != 1 || d.Day() != 15 {
		t.Fatalf("parsed = %v", d)
	}
	if _, err := ParseArchiveDate("15-01-2025"); err == nil {
		t.Fatal("bad date accepted")
	}
	if _, err := ParseArchiveDate(""); err == nil {
		t.Fatal("empty date accepted")
	}
}

func TestArchiveDateFromPath(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fake-elf")
	if err := os.WriteFile(f, []byte("\x7fELF"), 0644); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2024, 6, 10, 8, 0, 0, 0, time.UTC)
	if err := os.Chtimes(f, mt, mt); err != nil {
		t.Fatal(err)
	}
	if got := ArchiveDateFromPath(f); got.Format("2006-01-02") != "2024-06-10" {
		t.Fatalf("ArchiveDateFromPath = %v", got)
	}
	// Missing file falls back to ~now.
	if got := ArchiveDateFromPath(filepath.Join(t.TempDir(), "nope")); time.Since(got) > 5*time.Minute {
		t.Fatalf("missing file date = %v, want ~now", got)
	}
}

// TestArchiveIndexLoadAndBacktrack serves two dated snapshots: the requested
// day (2025-01-15) has no core.db (404), the prior day does. ArchiveIndex
// must walk back and resolve from the earlier snapshot.
func TestArchiveIndexLoadAndBacktrack(t *testing.T) {
	isolateArchive(t)
	dbEntries := []tarEntry{{"oldlib-1.0-1/desc", desc("oldlib", "1.0-1", "libold.so=1-64")}}
	dbBytes := gzipTar(dbEntries)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only 2025-01-14/core has a db; everything else 404s.
		if r.URL.Path == "/repos/2025/01/14/core/os/x86_64/core.db" {
			w.Write(dbBytes)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ArchiveBaseURL = srv.URL

	date, err := ParseArchiveDate("2025-01-15")
	if err != nil {
		t.Fatal(err)
	}
	idx, used, err := ArchiveIndex(date)
	if err != nil {
		t.Fatalf("ArchiveIndex = %v", err)
	}
	if used != "2025-01-14" {
		t.Fatalf("used = %q, want 2025-01-14 (backtrack)", used)
	}
	provs := idx.Providers("libold.so.1")
	if len(provs) != 1 || provs[0].Name != "oldlib" || provs[0].Version != "1.0-1" {
		t.Fatalf("Providers(libold.so.1) = %+v", provs)
	}
	// Cached: second call for the same date must not hit the network again.
	ArchiveBaseURL = "http://127.0.0.1:0"
	idx2, used2, err := ArchiveIndex(date)
	if err != nil || used2 != used || idx2 != idx {
		t.Fatalf("cached ArchiveIndex = %v %q %v, want same", idx2, used2, err)
	}
}

// TestArchiveProvidersMissingSnapshot surfaces an error when no day in the
// backtrack window has a db (all 404).
func TestArchiveProvidersMissingSnapshot(t *testing.T) {
	isolateArchive(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ArchiveBaseURL = srv.URL

	date, err := ParseArchiveDate("2025-01-15")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ArchiveProviders("libold.so.1", date); err == nil {
		t.Fatal("expected error for empty archive window")
	}
}

// TestArchiveCacheIsolation ensures dated .db files land under
// repo/archive/<date>/ and never clobber the live core.db.
func TestArchiveCacheIsolation(t *testing.T) {
	dir := isolateArchive(t)
	dbBytes := gzipTar([]tarEntry{{"oldlib-1.0-1/desc", desc("oldlib", "1.0-1", "libold.so=1-64")}})

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(dbBytes)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ArchiveBaseURL = srv.URL

	date, _ := ParseArchiveDate("2025-01-14")
	if _, _, err := ArchiveIndex(date); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache", "repo", "archive", "2025-01-14", "core.db")); err != nil {
		t.Fatalf("dated db not in archive subdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache", "repo", "core.db")); !os.IsNotExist(err) {
		t.Fatal("archive load clobbered the live core.db")
	}
}
