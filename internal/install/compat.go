package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"probe/internal/elf"
)

type compatChecker struct {
	libsDir     string
	verdefCache map[string]map[string]bool
	problems    []string
}

func checkCompat(paths []string, libsDir string) error {
	c := &compatChecker{
		libsDir:     libsDir,
		verdefCache: map[string]map[string]bool{},
	}
	for _, p := range paths {
		c.check(p)
	}
	if len(c.problems) == 0 {
		return nil
	}
	return fmt.Errorf("symbol version compatibility:\n  %s", strings.Join(c.problems, "\n  "))
}

func (c *compatChecker) check(path string) {
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
			c.problems = append(c.problems,
				fmt.Sprintf("%s: no library found for %s", filepath.Base(path), vn.File))
			continue
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
					fmt.Sprintf("%s: %s requires %s not provided by %s",
						filepath.Base(path), vn.File, v.Name, prov))
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
