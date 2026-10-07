package repo

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pap/internal/config"
)

// FindPackage returns every index entry for a package name, in repo priority
// order (the order of idx.repos, matching pacman's own precedence). Empty
// version lookups take element 0 (the live latest); pinned versions scan the
// list first so an exact live match downloads verified from a mirror.
func (idx *Index) FindPackage(name string) []PkgInfo {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var out []PkgInfo
	for _, def := range idx.repos {
		if p, ok := idx.pkgMeta[def.name+"/"+name]; ok {
			out = append(out, p)
		}
	}
	return out
}

// ArchivePackageURL addresses a single historical package file on the ALA
// packages/ endpoint, which keeps every published version (unlike the dated
// repo snapshots, which keep one). Layout:
// <base>/packages/<letter>/<pkgname>/<pkgname>-<pkgver>-<arch>.pkg.tar.zst
// The filename segment is URL-escaped (epochs contain ':', e.g. 1:1.3-2).
func ArchivePackageURL(name, version, arch string) string {
	letter := "x"
	if name != "" {
		letter = strings.ToLower(string([]rune(name)[0]))
	}
	fn := fmt.Sprintf("%s-%s-%s.pkg.tar.zst", name, version, arch)
	return strings.TrimSuffix(ArchiveBaseURL, "/") +
		"/packages/" + letter + "/" + name + "/" + url.PathEscape(fn)
}

// DownloadAppPackage fetches the installable app package itself (as opposed
// to a library dependency resolved by soname). version "" means the live
// latest; an exact version first tries the live index (mirror download,
// SHA256-verified) and falls back to the ALA packages/ endpoint, whose files
// carry no index checksum — the payload's .PKGINFO is verified to name the
// requested package version instead.
func (idx *Index) DownloadAppPackage(name, version string) ([]byte, PkgInfo, error) {
	if err := idx.Load(); err != nil {
		return nil, PkgInfo{}, fmt.Errorf("load repo index: %w", err)
	}
	cands := idx.FindPackage(name)
	if len(cands) == 0 {
		return nil, PkgInfo{}, fmt.Errorf("package %s not found in repos (%s)",
			name, strings.Join(idx.repoNames(), ", "))
	}
	if version == "" {
		data, err := idx.Download(cands[0])
		if err != nil {
			return nil, PkgInfo{}, err
		}
		return data, cands[0], nil
	}
	for _, p := range cands {
		if p.Version == version {
			data, err := idx.Download(p)
			if err != nil {
				return nil, PkgInfo{}, err
			}
			return data, p, nil
		}
	}
	return idx.downloadArchivePackage(name, version)
}

func (idx *Index) repoNames() []string {
	var out []string
	for _, d := range idx.repos {
		out = append(out, d.name)
	}
	return out
}

// downloadArchivePackage fetches name-version from ALA packages/, trying the
// host arch then "any" (arch-independent packages). Files are cached under
// the package cache by filename; hits are re-verified against .PKGINFO.
func (idx *Index) downloadArchivePackage(name, version string) ([]byte, PkgInfo, error) {
	pkgCache := idx.pkgCacheDir
	if pkgCache == "" {
		pkgCache = idx.cacheDir
	}
	if err := os.MkdirAll(pkgCache, 0755); err != nil {
		return nil, PkgInfo{}, err
	}
	var tried []string
	for _, arch := range []string{config.Arch, "any"} {
		fn := fmt.Sprintf("%s-%s-%s.pkg.tar.zst", name, version, arch)
		cacheFile := filepath.Join(pkgCache, fn)
		if data, err := os.ReadFile(cacheFile); err == nil {
			if ok, _ := checkPkgInfoBytes(data, name, version, arch); ok {
				return data, PkgInfo{Repo: "archive", Name: name, Version: version, Arch: arch, Filename: fn}, nil
			}
			os.Remove(cacheFile)
		}
		u := ArchivePackageURL(name, version, arch)
		tried = append(tried, u)
		tmpF, err := os.CreateTemp(pkgCache, ".dl-*")
		if err != nil {
			continue
		}
		tmp := tmpF.Name()
		tmpF.Close()
		os.Remove(tmp)
		if err := fetchTo(u, tmp, ""); err != nil {
			os.Remove(tmp)
			continue
		}
		ok, verr := checkPkgInfoFile(tmp, name, version, arch)
		if verr != nil || !ok {
			os.Remove(tmp)
			if verr != nil {
				return nil, PkgInfo{}, fmt.Errorf("archive %s: %w", fn, verr)
			}
			continue
		}
		data, err := os.ReadFile(tmp)
		os.Remove(tmp)
		if err != nil {
			continue
		}
		if err := os.WriteFile(cacheFile, data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pkg cache write: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "  note: %s %s not in live repos; using Arch Linux Archive\n", name, version)
		return data, PkgInfo{Repo: "archive", Name: name, Version: version, Arch: arch, Filename: fn}, nil
	}
	return nil, PkgInfo{}, fmt.Errorf("package %s %s not in live repos and not on the Archive (tried %s)",
		name, version, strings.Join(tried, ", "))
}

// checkPkgInfoBytes stages data to a temp file for .PKGINFO inspection.
func checkPkgInfoBytes(data []byte, name, version, arch string) (bool, error) {
	f, err := os.CreateTemp("", "pap-pkginfo-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, err
	}
	f.Close()
	defer os.Remove(tmp)
	return checkPkgInfoFile(tmp, name, version, arch)
}

// checkPkgInfoFile verifies a package file's .PKGINFO names the requested
// package version (the Archive endpoint serves no checksums, so this is the
// integrity/identity check that pins the download to the request).
func checkPkgInfoFile(pkgFile, name, version, arch string) (bool, error) {
	out, err := exec.Command("bsdtar", "-xOf", pkgFile, ".PKGINFO").Output()
	if err != nil {
		return false, fmt.Errorf("read .PKGINFO: %w", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		got[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	if got["pkgname"] != name {
		return false, fmt.Errorf("pkgname mismatch: want %s, got %s", name, got["pkgname"])
	}
	if got["pkgver"] != version {
		return false, fmt.Errorf("pkgver mismatch: want %s %s, got %s", name, version, got["pkgver"])
	}
	if got["arch"] != arch {
		return false, fmt.Errorf("arch mismatch: want %s, got %s", arch, got["arch"])
	}
	return true, nil
}
