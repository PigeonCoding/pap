package repo

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type PkgInfo struct {
	Repo    string
	Name    string
	Version string
	Arch    string
}

type Index struct {
	mu       sync.RWMutex
	fileMap  map[string][]PkgInfo
	mirrors  []string
	cacheDir string
	loaded   bool
}

var (
	defaultIndex *Index
	once         sync.Once
)

func Default() *Index {
	once.Do(func() {
		defaultIndex = &Index{
			fileMap:  make(map[string][]PkgInfo),
			mirrors:  loadMirrors(),
			cacheDir: filepath.Join(os.TempDir(), "arch-repo-cache"),
		}
	})
	return defaultIndex
}

func loadMirrors() []string {
	data, err := os.ReadFile("/etc/pacman.d/mirrorlist")
	if err != nil {
		return []string{"https://mirror.krfoss.org/archlinux"}
	}
	var mirrors []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Server = ") {
			mirrors = append(mirrors, strings.TrimPrefix(line, "Server = "))
		}
	}
	if len(mirrors) == 0 {
		mirrors = append(mirrors, "https://mirror.krfoss.org/archlinux")
	}
	return mirrors
}

func (idx *Index) dbURL(repoName string) string {
	u := idx.mirrors[0]
	u = strings.ReplaceAll(u, "$repo", repoName)
	u = strings.ReplaceAll(u, "$arch", "x86_64")
	return strings.TrimSuffix(u, "/") + "/" + repoName + ".db"
}

func (idx *Index) Load(repos ...string) error {
	idx.mu.RLock()
	done := idx.loaded
	idx.mu.RUnlock()
	if done {
		return nil
	}
	if len(repos) == 0 {
		repos = []string{"core", "extra", "multilib"}
	}

	os.MkdirAll(idx.cacheDir, 0755)

	var wg sync.WaitGroup
	errCh := make(chan error, len(repos))

	for _, r := range repos {
		wg.Add(1)
		go func(repo string) {
			defer wg.Done()
			if err := idx.loadRepo(repo); err != nil {
				errCh <- fmt.Errorf("repo %s: %w", repo, err)
			}
		}(r)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		fmt.Fprintf(os.Stderr, "repo load error: %v\n", err)
	}

	idx.mu.Lock()
	idx.loaded = true
	idx.mu.Unlock()

	return nil
}

func (idx *Index) loadRepo(repoName string) error {
	dbFile := filepath.Join(idx.cacheDir, repoName+".db")

	if !validGzip(dbFile) {
		os.Remove(dbFile)
		var lastErr error
		for _, m := range idx.mirrors {
			u := strings.ReplaceAll(strings.ReplaceAll(m, "$repo", repoName), "$arch", "x86_64")
			u = strings.TrimSuffix(u, "/") + "/" + repoName + ".db"
			if err := fetch(u, dbFile); err != nil {
				lastErr = err
				continue
			}
			if validGzip(dbFile) {
				lastErr = nil
				break
			}
			os.Remove(dbFile)
			lastErr = fmt.Errorf("invalid db from %s", u)
		}
		if lastErr != nil {
			return lastErr
		}
	}

	f, err := os.Open(dbFile)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if !strings.HasSuffix(hdr.Name, "/desc") {
			continue
		}

		descData, err := io.ReadAll(tr)
		if err != nil {
			continue
		}
		desc := string(descData)

		name := descField(desc, "NAME")
		version := descField(desc, "VERSION")
		arch := descField(desc, "ARCH")
		if name == "" || version == "" {
			continue
		}

		info := PkgInfo{
			Repo:    repoName,
			Name:    name,
			Version: version,
			Arch:    arch,
		}

		for _, soname := range parseProvides(desc) {
			idx.mu.Lock()
			idx.fileMap[soname] = append(idx.fileMap[soname], info)
			idx.mu.Unlock()
		}
	}

	return nil
}

func descField(desc, key string) string {
	marker := "%" + key + "%"
	inField := false
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "%") {
			if inField {
				return ""
			}
			inField = line == marker
			continue
		}
		if inField && line != "" {
			return line
		}
	}
	return ""
}

func parseProvides(desc string) []string {
	var provides []string
	inProvides := false
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "%") {
			inProvides = line == "%PROVIDES%"
			continue
		}
		if !inProvides || line == "" {
			continue
		}
		if soname, ok := provideToSoname(line); ok {
			provides = append(provides, soname)
		}
	}
	return provides
}

func provideToSoname(provide string) (string, bool) {
	base, ver, found := strings.Cut(provide, "=")
	if !found || !strings.HasSuffix(base, ".so") {
		return "", false
	}
	abi, _, _ := strings.Cut(ver, "-")
	return base + "." + abi, true
}

func (idx *Index) Resolve(soname string) (PkgInfo, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	entries, ok := idx.fileMap[soname]
	if !ok || len(entries) == 0 {
		return PkgInfo{}, fmt.Errorf("no package provides %s", soname)
	}
	for _, e := range entries {
		if e.Repo != "multilib" {
			return e, nil
		}
	}
	return entries[0], nil
}

func (idx *Index) Mirrors() []string {
	return idx.mirrors
}

func fetch(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func validGzip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [2]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	return hdr[0] == 0x1f && hdr[1] == 0x8b
}

func (idx *Index) Download(pkg PkgInfo) ([]byte, error) {
	filename := fmt.Sprintf("%s-%s-%s.pkg.tar.zst", pkg.Name, pkg.Version, pkg.Arch)
	var lastErr error
	for _, m := range idx.mirrors {
		u := strings.ReplaceAll(strings.ReplaceAll(m, "$repo", pkg.Repo), "$arch", "x86_64")
		u = strings.TrimSuffix(u, "/") + "/" + filename
		resp, err := http.Get(u)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d for %s", resp.StatusCode, u)
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return data, nil
	}
	return nil, lastErr
}
