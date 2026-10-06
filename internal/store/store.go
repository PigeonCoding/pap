package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"probe/internal/config"
)

// gcGrace keeps entries that are younger than this alive, so a GC racing a
// concurrent install can't unlink a file that was just stored but not yet
// symlinked into an app.
const gcGrace = 2 * time.Minute

func Init() error {
	return os.MkdirAll(config.StoreDir, 0755)
}

func Path(data []byte) string {
	h := sha256.Sum256(data)
	return filepath.Join(config.StoreDir, hex.EncodeToString(h[:]))
}

// Store writes content under its sha256 hex digest (content-addressed, shared
// across apps). Writes are atomic: temp file + rename, so a crash can never
// leave a truncated file under a hash that later installs would trust.
func Store(data []byte) (string, error) {
	if err := Init(); err != nil {
		return "", err
	}
	p := Path(data)
	if st, err := os.Stat(p); err == nil && st.Size() == int64(len(data)) {
		return p, nil
	}

	tmp, err := os.CreateTemp(config.StoreDir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	return p, nil
}

func Symlink(storePath, linkPath string) error {
	os.Remove(linkPath)
	return os.Symlink(storePath, linkPath)
}

func GC(used map[string]bool) (int, error) {
	entries, err := os.ReadDir(config.StoreDir)
	if err != nil {
		return 0, err
	}

	var removed int
	for _, e := range entries {
		if e.IsDir() || filepath.Base(e.Name())[0] == '.' {
			continue
		}
		name := e.Name()
		if used[name] {
			continue
		}
		p := filepath.Join(config.StoreDir, name)
		if st, err := e.Info(); err == nil && time.Since(st.ModTime()) < gcGrace {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	return removed, nil
}

// ScanUsed marks every store entry referenced by a symlink anywhere under
// apps/*/libs (including plugin subdirectories).
func ScanUsed() map[string]bool {
	used := map[string]bool{}
	entries, _ := os.ReadDir(config.AppsDir)
	for _, app := range entries {
		if !app.IsDir() {
			continue
		}
		libsDir := filepath.Join(config.AppsDir, app.Name(), "libs")
		filepath.WalkDir(libsDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			target, err := os.Readlink(path)
			if err != nil {
				return nil
			}
			used[filepath.Base(target)] = true
			return nil
		})
	}
	return used
}
