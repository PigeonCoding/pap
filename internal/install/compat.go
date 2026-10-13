package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pap/internal/elf"
	"pap/internal/resolver"
)

type compatChecker struct {
	libsDir     string
	verdefCache map[string]map[string]bool
	visited     map[string]bool
	problems    []compatProblem
	skip        func(string) bool // host-provided sonames (ForceLibc-aware)
}

// compatProblem is one unresolved dependency. Needed names the DT_NEEDED
// soname; Provider is the resolved file ("" when nothing provides it).
type compatProblem struct {
	Requirer string // basename of the ELF with the requirement
	Needed   string // soname it asks for ("" for unparseable files)
	Version  string // required symbol version ("" for missing/unparseable)
	Provider string // resolved provider path ("" when missing)
}

func (p compatProblem) Error() string {
	switch {
	case p.Needed == "":
		return fmt.Sprintf("%s: %s", p.Requirer, p.Version)
	case p.Provider == "":
		return fmt.Sprintf("%s: missing dependency %s", p.Requirer, p.Needed)
	default:
		// Full provider path, not just the basename: when the host
		// resolution picks an unexpected file (stale ldconfig entry,
		// foreign-arch copy), the path is what diagnoses it.
		return fmt.Sprintf("%s: %s requires %s not provided by %s",
			p.Requirer, p.Needed, p.Version, p.Provider)
	}
}

// hostTooOldHint explains a host-glibc failure accurately for the mode in
// use: with --no-glibc-compat the user forced live-latest (retry without
// it); in default auto mode pap already picked the newest build compatible
// with the app binary, so only an older manual pin can go further back.
func hostTooOldHint(noGlibcCompat bool) string {
	if noGlibcCompat {
		return "host glibc is older than this Arch build requires; " +
			"you passed --no-glibc-compat (live latest forced) — " +
			"retry without it to auto-select the newest compatible build, " +
			"or pin one manually with --pkg-version <older-ver> <pkg>"
	}
	return "host glibc is older than this Arch build requires; " +
		"pap already selected the newest build compatible with the app binary, " +
		"but its dependency closure still needs newer glibc — " +
		"try --pkg-version <older-ver> <pkg> for an older build"
}

// hostTooOld reports whether any problem is a host-provided lib (outside
// libsDir, e.g. libc.so.6) missing a required symbol version. That means the
// Arch package was built against a newer glibc than the host runs — common
// on Debian/stable hosts — and no vendoring can fix it.
func hostTooOld(problems []compatProblem, libsDir string) bool {
	for _, p := range problems {
		if p.Version == "" || p.Provider == "" || p.Needed == "" {
			continue
		}
		if strings.HasPrefix(p.Provider, libsDir+string(os.PathSeparator)) {
			continue
		}
		if strings.HasPrefix(p.Version, "GLIBC_") || strings.HasPrefix(p.Version, "GCC_") {
			return true
		}
	}
	return false
}

// checkCompatDetailed is the multi-root, structured variant of checkCompat:
// callers use the problems to heal (e.g. re-stage an incompatible bundled
// lib from the repo) instead of just failing. skip reports host-provided
// sonames; pass skipLib(opts, ·) so --force-libc checks the vendored glibc
// instead of the host's.
func checkCompatDetailed(roots []string, libsDir string, skip func(string) bool) []compatProblem {
	c := &compatChecker{
		libsDir:     libsDir,
		verdefCache: map[string]map[string]bool{},
		visited:     map[string]bool{},
		skip:        skip,
	}
	for _, r := range roots {
		c.check(r)
	}
	return c.problems
}

func (c *compatChecker) check(path string) {
	if c.visited[path] {
		return
	}
	c.visited[path] = true

	info, err := elf.Parse(path)
	if err != nil {
		c.problems = append(c.problems, compatProblem{Requirer: filepath.Base(path), Version: err.Error()})
		return
	}
	base := filepath.Base(path)

	for _, n := range info.NEEDED {
		if c.skip(n) {
			continue
		}
		prov := c.provider(n)
		if prov == "" {
			c.problems = append(c.problems,
				compatProblem{Requirer: base, Needed: n})
			continue
		}
		if strings.HasPrefix(prov, c.libsDir+string(os.PathSeparator)) {
			c.check(prov)
		}
	}

	needs, err := elf.ParseVerneed(path)
	if err != nil {
		return
	}
	for _, vn := range needs {
		if vn.File == "" {
			continue
		}
		prov := c.provider(vn.File)
		if prov == "" {
			continue // already reported as a missing dependency above
		}
		defs := c.verdef(prov)
		if defs == nil {
			continue
		}
		for _, v := range vn.Versions {
			if v.Weak || v.Name == "" {
				continue
			}
			if !defs[v.Name] {
				c.problems = append(c.problems,
					compatProblem{Requirer: base, Needed: vn.File, Version: v.Name, Provider: prov})
			}
		}
	}
}

func (c *compatChecker) provider(file string) string {
	if strings.Contains(file, "/") {
		if _, err := os.Stat(file); err == nil {
			return file
		}
		return ""
	}
	if cand := filepath.Join(c.libsDir, file); statExists(cand) {
		return cand
	}
	// Host lookup must cover Debian multiarch (/usr/lib/x86_64-linux-gnu,
	// ...) via ldconfig, not just Arch's /usr/lib: otherwise VERNEED
	// checks against libc are silently skipped and installs succeed with
	// a binary that fails at runtime (GLIBC_X.XX not found).
	if p := resolver.FindHostLib(file); p != "" {
		return p
	}
	return ""
}

func (c *compatChecker) verdef(path string) map[string]bool {
	if defs, ok := c.verdefCache[path]; ok {
		return defs
	}
	defs, err := elf.ParseVerdef(path)
	if err != nil {
		defs = nil
	}
	c.verdefCache[path] = defs
	return defs
}

func statExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
