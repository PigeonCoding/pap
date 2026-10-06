package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var StoreDir = "/tmp/opencode/probe-test/store"

func Init() error {
	return os.MkdirAll(StoreDir, 0755)
}

func Path(name string, data []byte) string {
	h := sha256.Sum256(data)
	return filepath.Join(StoreDir, fmt.Sprintf("%x-%s", h, name))
}

func Store(name string, data []byte) (string, error) {
	p := Path(name, data)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if err := os.WriteFile(p, data, 0755); err != nil {
		return "", err
	}
	return p, nil
}

func Exists(name string, data []byte) bool {
	_, err := os.Stat(Path(name, data))
	return err == nil
}

func Symlink(storePath, linkPath string) error {
	os.Remove(linkPath)
	return os.Symlink(storePath, linkPath)
}

func GC(used map[string]bool) (int, error) {
	entries, err := os.ReadDir(StoreDir)
	if err != nil {
		return 0, err
	}

	var removed int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !used[name] {
			os.Remove(filepath.Join(StoreDir, name))
			removed++
		}
	}
	return removed, nil
}

func ScanUsed() map[string]bool {
	used := map[string]bool{}
	appsDir := filepath.Join(filepath.Dir(StoreDir), "apps")
	entries, _ := os.ReadDir(appsDir)
	for _, app := range entries {
		if !app.IsDir() {
			continue
		}
		libsDir := filepath.Join(appsDir, app.Name(), "libs")
		links, _ := os.ReadDir(libsDir)
		for _, link := range links {
			target, err := os.Readlink(filepath.Join(libsDir, link.Name()))
			if err != nil {
				continue
			}
			base := filepath.Base(target)
			if strings.Contains(base, "-") {
				used[base] = true
			}
		}
	}
	return used
}
