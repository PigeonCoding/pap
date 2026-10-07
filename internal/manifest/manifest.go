package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Manifest struct {
	Name         string            `json:"name"`
	Binary       string            `json:"binary"`
	Libs         []string          `json:"libs"`
	Packages     map[string]string `json:"packages"`
	Package      string            `json:"package,omitempty"`
	Source       string            `json:"source,omitempty"`
	SourceSHA256 string            `json:"sourceSha256,omitempty"`
	PlacedBinary bool              `json:"placedBinary,omitempty"`
	// Launcher is set when the installed ~/.local/bin/<name> is a launcher
	// script exec'ing the real binary at apps/<name>/bin/<name>. Used for
	// binaries that locate resources relative to their own path
	// (e.g. kitty's ../lib/kitty); the real binary must live inside the
	// app dir for those lookups to resolve.
	// Deprecated: new installs use ExeRel + a ~/.local/bin symlink instead.
	Launcher bool `json:"launcher,omitempty"`
	// ExeRel is the real binary's path relative to ~/.pap/exe/<name>.
	// ~/.local/bin/<name> is a symlink to it, so /proc/self/exe resolves
	// inside the exe payload and both absolute-RPATH lib lookup and
	// exe-relative resource lookup keep working.
	ExeRel string `json:"exeRel,omitempty"`
}

func Path(appDir string) string {
	return filepath.Join(appDir, "manifest.json")
}

func Load(appDir string) (*Manifest, error) {
	data, err := os.ReadFile(Path(appDir))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func Save(appDir string, m *Manifest) error {
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(appDir), data, 0644)
}

func List(appsDir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return nil, err
	}
	var manifests []*Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := Load(filepath.Join(appsDir, e.Name()))
		if err == nil {
			manifests = append(manifests, m)
		}
	}
	return manifests, nil
}
