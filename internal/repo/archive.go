package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pap/internal/config"
)

// ArchiveBaseURL is the root of the Arch Linux Archive. Overridden in tests
// to point at an httptest server that mimics
// <base>/repos/YYYY/MM/DD/<repo>/os/<arch>/<repo>.db.
var ArchiveBaseURL = "https://archive.archlinux.org"

// archiveIdxCache memoizes loaded archive snapshots per date+arch so a
// single install resolves every soname against one snapshot.
var (
	archiveMu  sync.Mutex
	archiveIdx = map[string]*Index{}
)

// ArchiveServerFor returns the server template serving the dated ALA snapshot.
// $repo/$arch are expanded later by substVars, matching the layout
// repos/YYYY/MM/DD/<repo>/os/<arch>/<repo>.db with packages co-located.
func ArchiveServerFor(date time.Time) string {
	d := date.UTC()
	return fmt.Sprintf("%s/repos/%04d/%02d/%02d/$repo/os/$arch",
		ArchiveBaseURL, d.Year(), int(d.Month()), d.Day())
}

// ParseArchiveDate parses YYYY-MM-DD (the --archive-date flag format).
func ParseArchiveDate(s string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid archive date %q (want YYYY-MM-DD): %w", s, err)
	}
	return d.UTC(), nil
}

// ArchiveDateFromPath derives the snapshot date from an ELF's mtime — the
// closest on-disk proxy for "the version this binary was built against".
// Missing files fall back to now (i.e. latest snapshot).
func ArchiveDateFromPath(path string) time.Time {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime().UTC()
	}
	return time.Now().UTC()
}

// ArchiveDateFromRoots picks the snapshot date for an install: the mtime of
// the main root ELF (roots[0]), which best approximates the app's build date.
// Later-discovered transitive deps reuse the same snapshot so the whole
// closure stays era-consistent.
func ArchiveDateFromRoots(roots []string) time.Time {
	for _, r := range roots {
		if _, err := os.Stat(r); err == nil {
			return ArchiveDateFromPath(r)
		}
	}
	return time.Now().UTC()
}

// NewArchiveIndex builds an index over the ALA snapshot for date. Only the
// official repos are archived (no chaotic-aur). Each date gets its own repo
// cache subdir so dated .db files never clobber the live index.
func NewArchiveIndex(date time.Time, cacheDir, pkgCacheDir string) *Index {
	d := date.UTC().Format("2006-01-02")
	sub := filepath.Join(cacheDir, "archive", d)
	server := ArchiveServerFor(date.UTC())
	repos := []repoDef{
		{name: "core", servers: []string{server}},
		{name: "extra", servers: []string{server}},
		{name: "multilib", servers: []string{server}},
	}
	return NewWithRepos(repos, sub, pkgCacheDir)
}

// ArchiveIndex returns the cached (loading on first use) index for date,
// walking back up to backtrackDays for the nearest earlier snapshot when the
// exact day has no core.db (ALA days can be missing). Returns the index plus
// the snapshot date actually used (YYYY-MM-DD).
func ArchiveIndex(date time.Time) (*Index, string, error) {
	const backtrackDays = 14
	base := date.UTC()
	var lastErr error
	for i := 0; i < backtrackDays; i++ {
		d := base.AddDate(0, 0, -i)
		key := d.Format("2006-01-02") + "|" + config.Arch
		archiveMu.Lock()
		cached, ok := archiveIdx[key]
		archiveMu.Unlock()
		if ok {
			return cached, d.Format("2006-01-02"), nil
		}
		idx := NewArchiveIndex(d, config.RepoCacheDir, config.PkgCacheDir)
		if err := idx.Load(); err != nil {
			lastErr = err
			continue
		}
		archiveMu.Lock()
		archiveIdx[key] = idx
		archiveMu.Unlock()
		if i > 0 {
			fmt.Fprintf(os.Stderr, "  note: archive snapshot %s missing, using %s\n",
				base.Format("2006-01-02"), d.Format("2006-01-02"))
		}
		return idx, d.Format("2006-01-02"), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no snapshot found")
	}
	return nil, "", fmt.Errorf("archive snapshot for %s unavailable (tried %d prior days): %w",
		base.Format("2006-01-02"), backtrackDays, lastErr)
}

// ArchiveProviders resolves soname against the dated snapshot, for use when
// the live index has no provider (soname bump / removed package). Callers
// download from the returned index so packages come from the same snapshot.
func ArchiveProviders(soname string, date time.Time) ([]PkgInfo, *Index, string, error) {
	idx, used, err := ArchiveIndex(date)
	if err != nil {
		return nil, nil, "", err
	}
	return idx.Providers(soname), idx, used, nil
}

// ResetArchiveCache drops memoized archive indexes (tests only).
func ResetArchiveCache() {
	archiveMu.Lock()
	defer archiveMu.Unlock()
	archiveIdx = map[string]*Index{}
}
