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
		return fmt.Sprintf("%s: %s requires %s not provided by %s",
			p.Requirer, p.Needed, p.Version, filepath.Base(p.Provider))
	}
}

// checkCompat walks DT_NEEDED recursively from root and verifies that every
// non-host dependency exists, then checks symbol version requirements
// (VERNEED) against the provider's VERDEF. Recursion only descends into
// isolated libs; system libs are trusted as the running host's own.
func checkCompat(root string, libsDir string) error {
	if problems := checkCompatDetailed([]string{root}, libsDir); len(problems) > 0 {
		msgs := make([]string, len(problems))
		for i, p := range problems {
			msgs[i] = p.Error()
		}
		return fmt.Errorf("dependency compatibility:\n  %s", strings.Join(msgs, "\n  "))
	}
	return nil
}

// checkCompatDetailed is the multi-root, structured variant of checkCompat:
// callers use the problems to heal (e.g. re-stage an incompatible bundled
// lib from the repo) instead of just failing.
func checkCompatDetailed(roots []string, libsDir string) []compatProblem {
	c := &compatChecker{
		libsDir:     libsDir,
		verdefCache: map[string]map[string]bool{},
		visited:     map[string]bool{},
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
		if resolver.Skip(n) {
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
	candidates := []string{
		filepath.Join(c.libsDir, file),
		"/usr/lib/" + file,
		"/usr/lib64/" + file,
		"/lib/" + file,
		"/lib64/" + file,
	}
	for _, cand := range candidates {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
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
