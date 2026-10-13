package install

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"pap/internal/config"
	"pap/internal/elf"
	"pap/internal/repo"
	"pap/internal/resolver"
)

// This file implements glibc-aware version selection. Arch repos target the
// newest glibc, so a live-latest Arch build can require symbol versions
// (e.g. GLIBC_2.43) that a Debian/stable host does not provide. By default
// pap installs the newest Arch build the host glibc can actually run,
// walking back through the Arch Linux Archive; --no-glibc-compat opts out
// and takes live latest regardless.

var glibcVerRe = regexp.MustCompile(`^GLIBC_(\d+)\.(\d+)$`)

func parseGlibcVersion(s string) (maj, min int, ok bool) {
	m := glibcVerRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(m[1])
	min, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

func formatGlibc(maj, min int) string { return fmt.Sprintf("%d.%d", maj, min) }

// HostGlibc returns the newest GLIBC_X.Y the host libc provides.
func HostGlibc() (maj, min int, libPath string, ok bool) {
	p := resolver.FindHostLib("libc.so.6")
	if p == "" {
		return 0, 0, "", false
	}
	defs, err := elf.ParseVerdef(p)
	if err != nil || len(defs) == 0 {
		return 0, 0, p, false
	}
	bestMaj, bestMin := -1, -1
	for name := range defs {
		maj, min, ok := parseGlibcVersion(name)
		if !ok {
			continue
		}
		if maj > bestMaj || (maj == bestMaj && min > bestMin) {
			bestMaj, bestMin = maj, min
		}
	}
	if bestMaj < 0 {
		return 0, 0, p, false
	}
	return bestMaj, bestMin, p, true
}

// RequiredGlibc returns the newest GLIBC_X.Y an ELF requires (weak refs
// excluded, matching the compat checker). ok is false when the file needs
// no versioned glibc symbols at all.
func RequiredGlibc(elfPath string) (maj, min int, ok bool) {
	needs, err := elf.ParseVerneed(elfPath)
	if err != nil || len(needs) == 0 {
		return 0, 0, false
	}
	bestMaj, bestMin := -1, -1
	for _, vn := range needs {
		for _, v := range vn.Versions {
			if v.Weak || v.Name == "" {
				continue
			}
			maj, min, ok := parseGlibcVersion(v.Name)
			if !ok {
				continue
			}
			if maj > bestMaj || (maj == bestMaj && min > bestMin) {
				bestMaj, bestMin = maj, min
			}
		}
	}
	if bestMaj < 0 {
		return 0, 0, false
	}
	return bestMaj, bestMin, true
}

func glibcLE(hostMaj, hostMin, reqMaj, reqMin int) bool {
	return hostMaj > reqMaj || (hostMaj == reqMaj && hostMin >= reqMin)
}

// elfHostCompatible reports whether one ELF's glibc requirements are met by
// the host. Unknown hosts and requirement-free files count as compatible.
func elfHostCompatible(path string) bool {
	reqMaj, reqMin, ok := RequiredGlibc(path)
	if !ok {
		return true
	}
	hostMaj, hostMin, _, hostOK := HostGlibc()
	if !hostOK {
		return true
	}
	return glibcLE(hostMaj, hostMin, reqMaj, reqMin)
}

// PayloadHostCompatible reports whether every ELF under an extracted package
// payload can run against the host glibc. It also returns the payload's max
// requirement for messages (reqOK false when nothing requires glibc).
func PayloadHostCompatible(payloadDir string) (compatible bool, reqMaj, reqMin int, reqOK bool) {
	hostMaj, hostMin, _, hostOK := HostGlibc()
	if !hostOK {
		return true, 0, 0, false
	}
	bestMaj, bestMin := -1, -1
	for _, p := range listFolderELFs(payloadDir) {
		maj, min, ok := RequiredGlibc(p)
		if !ok {
			continue
		}
		if maj > bestMaj || (maj == bestMaj && min > bestMin) {
			bestMaj, bestMin = maj, min
		}
	}
	if bestMaj < 0 {
		return true, 0, 0, false
	}
	return glibcLE(hostMaj, hostMin, bestMaj, bestMin), bestMaj, bestMin, true
}

// IsHostGlibcError reports whether err is a dependency-compatibility failure
// caused by the host glibc being too old (as opposed to ordinary missing deps).
func IsHostGlibcError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "host glibc is older")
}

// pkgVerCmp compares Arch package versions (epoch:pkgver-pkgrel) pacman-style:
// numeric epoch first, then rpm-style segment comparison. Returns -1, 0, +1.
func pkgVerCmp(a, b string) int {
	ae, ar := splitEpoch(a)
	be, br := splitEpoch(b)
	if ae != be {
		if ae < be {
			return -1
		}
		return 1
	}
	return rpmVerCmp(ar, br)
}

func splitEpoch(v string) (uint64, string) {
	if i := strings.Index(v, ":"); i >= 0 {
		if n, err := strconv.ParseUint(v[:i], 10, 64); err == nil {
			return n, v[i+1:]
		}
		return 0, v
	}
	return 0, v
}

// rpmVerCmp compares version strings segment-wise: runs of digits compare
// numerically, runs of letters ASCII-betically, digits sort newer than
// letters, "~" sorts older than everything (including empty). Separators
// (., -, _, +) only delimit segments.
func rpmVerCmp(a, b string) int {
	sa := verSegments(a)
	sb := verSegments(b)
	for i := 0; i < len(sa) || i < len(sb); i++ {
		var x, y verSeg
		if i < len(sa) {
			x = sa[i]
		}
		if i < len(sb) {
			y = sb[i]
		}
		if c := cmpVerSeg(x, y); c != 0 {
			return c
		}
	}
	return 0
}

type verSeg struct {
	tilde bool   // segment started with "~" (sorts oldest)
	num   bool   // numeric run (sorts newer than alpha)
	val   string // digits without leading zeros, or letters
}

func verSegments(s string) []verSeg {
	var out []verSeg
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '.' || c == '-' || c == '_' || c == '+' {
			i++
			continue
		}
		if c == '~' {
			j := i + 1
			for j < len(s) && (s[j] == '~') {
				j++
			}
			out = append(out, verSeg{tilde: true})
			i = j
			continue
		}
		if c >= '0' && c <= '9' {
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			out = append(out, verSeg{num: true, val: strings.TrimLeft(s[i:j], "0")})
			i = j
			continue
		}
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			j := i
			for j < len(s) && (s[j] >= 'A' && s[j] <= 'Z' || s[j] >= 'a' && s[j] <= 'z') {
				j++
			}
			out = append(out, verSeg{val: s[i:j]})
			i = j
			continue
		}
		// Any other byte is a separator as well.
		i++
	}
	return out
}

func cmpVerSeg(x, y verSeg) int {
	// Missing segment (zero value) vs present: present is newer, unless the
	// present side is a tilde (which is older than empty).
	xMissing := !x.tilde && !x.num && x.val == ""
	yMissing := !y.tilde && !y.num && y.val == ""
	if xMissing && yMissing {
		return 0
	}
	if xMissing {
		if y.tilde {
			return 1
		}
		return -1
	}
	if yMissing {
		if x.tilde {
			return -1
		}
		return 1
	}
	if x.tilde || y.tilde {
		if x.tilde && !y.tilde {
			return -1
		}
		if !x.tilde && y.tilde {
			return 1
		}
		return 0
	}
	if x.num && y.num {
		if len(x.val) != len(y.val) {
			if len(x.val) < len(y.val) {
				return -1
			}
			return 1
		}
		return strings.Compare(x.val, y.val)
	}
	if x.num {
		return 1 // digits sort newer than letters
	}
	if y.num {
		return -1
	}
	return strings.Compare(x.val, y.val)
}

var archiveListClient = &http.Client{Timeout: 60 * time.Second}

// listArchiveVersions returns every published version of pkgName (host arch
// plus "any"), newest first, scraped from the Arch Linux Archive packages/
// listing. No .db downloads: one small HTML fetch.
func listArchiveVersions(pkgName string) ([]string, error) {
	letter := "x"
	if pkgName != "" {
		letter = strings.ToLower(string([]rune(pkgName)[0]))
	}
	u := strings.TrimSuffix(repo.ArchiveBaseURL, "/") + "/packages/" + letter + "/" + pkgName + "/"
	resp, err := archiveListClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("list archived versions of %s: %w", pkgName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("list archived versions of %s: HTTP %d for %s", pkgName, resp.StatusCode, u)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("list archived versions of %s: %w", pkgName, err)
	}
	versions := versionsFromListing(string(body), pkgName)
	if len(versions) == 0 {
		return nil, fmt.Errorf("no archived versions of %s at %s", pkgName, u)
	}
	return versions, nil
}

var hrefRe = regexp.MustCompile(`href="([^"]+)"`)

// versionsFromListing extracts candidate versions from an Apache autoindex
// page, keeping files for the host arch (or "any"), deduplicated and sorted
// newest-first. Signature (.sig) files are ignored.
func versionsFromListing(html, pkgName string) []string {
	prefix := pkgName + "-"
	seen := map[string]bool{}
	var out []string
	for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
		raw := m[1]
		if i := strings.LastIndex(raw, "/"); i >= 0 {
			raw = raw[i+1:]
		}
		if strings.HasSuffix(raw, ".sig") {
			continue
		}
		fn, err := url.PathUnescape(raw)
		if err != nil {
			fn = raw
		}
		if !strings.HasPrefix(fn, prefix) {
			continue
		}
		rest := fn[len(prefix):]
		ver := ""
		for _, arch := range []string{config.Arch, "any"} {
			marker := "-" + arch + ".pkg.tar"
			idx := strings.LastIndex(rest, marker)
			if idx <= 0 {
				continue
			}
			tail := rest[idx+len(marker):]
			if tail != "" && !strings.HasPrefix(tail, ".") {
				continue
			}
			ver = rest[:idx]
			break
		}
		if ver == "" || strings.Contains(ver, "/") {
			continue
		}
		if !seen[ver] {
			seen[ver] = true
			out = append(out, ver)
		}
	}
	sort.Slice(out, func(i, j int) bool { return pkgVerCmp(out[i], out[j]) > 0 })
	return out
}

// findCompatibleVersion returns the newest archived version of pkgName whose
// payload runs on the host glibc. liveVersion is skipped (the caller already
// determined it is incompatible). At most maxTries candidates are downloaded.
func findCompatibleVersion(pkgName, liveVersion string, opts InstallOptions) (string, error) {
	return nextCompatibleVersion(pkgName, map[string]bool{liveVersion: true}, opts)
}

// nextCompatibleVersion generalizes findCompatibleVersion with a skip set:
// versions already tried (live latest, failed closures) are excluded, so a
// dependency-closure failure can continue the search with older builds
// instead of giving up.
func nextCompatibleVersion(pkgName string, skip map[string]bool, opts InstallOptions) (string, error) {
	versions, err := listArchiveVersions(pkgName)
	if err != nil {
		return "", err
	}
	hostMaj, hostMin, _, hostOK := HostGlibc()
	hostStr := "unknown"
	if hostOK {
		hostStr = "GLIBC_" + formatGlibc(hostMaj, hostMin)
	}
	const maxTries = 25
	tried := 0
	for _, ver := range versions {
		if skip[ver] {
			continue
		}
		if tried >= maxTries {
			return "", fmt.Errorf("no compatible build of %s in the last %d archived versions tried (host %s)",
				pkgName, maxTries, hostStr)
		}
		tried++
		opts.printf("  [glibc-compat] trying %s %s ...\n", pkgName, ver)
		data, _, derr := repo.Default().DownloadAppPackage(pkgName, ver)
		if derr != nil {
			opts.printf("  [glibc-compat] %s: download failed (%v), trying older\n", ver, derr)
			continue
		}
		work, terr := os.MkdirTemp("", "pap-gcompat-*")
		if terr != nil {
			return "", terr
		}
		payload := filepath.Join(work, "payload")
		compatible := false
		if merr := os.MkdirAll(payload, 0755); merr == nil {
			pkgFile := filepath.Join(work, "app.pkg.tar.zst")
			if werr := os.WriteFile(pkgFile, data, 0644); werr == nil {
				if out, xerr := exec.Command("bsdtar", "-xf", pkgFile, "-C", payload).CombinedOutput(); xerr == nil {
					compatible, _, _, _ = PayloadHostCompatible(payload)
					_ = out
				}
			}
		}
		os.RemoveAll(work)
		if compatible {
			return ver, nil
		}
		opts.printf("  [glibc-compat] %s needs newer glibc, trying older\n", ver)
	}
	return "", fmt.Errorf("no archived build of %s runs on this host's glibc (host %s); try --pkg-version <older> manually or --no-glibc-compat to force live latest",
		pkgName, hostStr)
}

// autoGlibcVersion decides which version to install: "" means the live
// latest is host-compatible (proceed unchanged); otherwise it returns the
// newest archived version the host glibc can run. The second return reports
// whether a downgrade was selected. Lookup failures fall back to ("", false)
// so the caller proceeds with live latest and the compat checker reports
// informatively instead of masking a network error as success.
func autoGlibcVersion(pkgName string, opts InstallOptions) (string, bool) {
	hostMaj, hostMin, _, hostOK := HostGlibc()
	if !hostOK {
		return "", false
	}
	opts.printf("Checking live %s against host glibc (downloading)...\n", pkgName)
	data, info, err := repo.Default().DownloadAppPackage(pkgName, "")
	if err != nil {
		return "", false
	}
	work, err := os.MkdirTemp("", "pap-gcompat-live-*")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(work)
	payload := filepath.Join(work, "payload")
	if err := os.MkdirAll(payload, 0755); err != nil {
		return "", false
	}
	pkgFile := filepath.Join(work, "app.pkg.tar.zst")
	if err := os.WriteFile(pkgFile, data, 0644); err != nil {
		return "", false
	}
	if out, err := exec.Command("bsdtar", "-xf", pkgFile, "-C", payload).CombinedOutput(); err != nil {
		opts.printf("warning: cannot inspect live %s %s (%v: %s); proceeding without glibc check\n",
			pkgName, info.Version, err, strings.TrimSpace(string(out)))
		return "", false
	}
	if ok, _, _, _ := PayloadHostCompatible(payload); ok {
		return "", false
	}
	opts.printf("Live %s %s needs newer glibc than this host provides (host GLIBC_%s); searching the Archive for the newest compatible build...\n",
		pkgName, info.Version, formatGlibc(hostMaj, hostMin))
	ver, err := findCompatibleVersion(pkgName, info.Version, opts)
	if err != nil {
		opts.printf("warning: %v; installing live latest (compat check will report)\n", err)
		return "", false
	}
	opts.printf("Selected %s %s (newest build compatible with host GLIBC_%s)\n",
		pkgName, ver, formatGlibc(hostMaj, hostMin))
	return ver, true
}
