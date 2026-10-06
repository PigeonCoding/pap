package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Manifest struct {
	Name     string            `json:"name"`
	Binary   string            `json:"binary"`
	Libs     []string          `json:"libs"`
	Packages map[string]string `json:"packages"`
	Package  string            `json:"package,omitempty"`
}

func Path(appsDir, name string) string {
	return filepath.Join(appsDir, name, "manifest.json")
}

func Load(appsDir, name string) (*Manifest, error) {
	data, err := os.ReadFile(Path(appsDir, name))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func Save(appsDir, name string, m *Manifest) error {
	if err := os.MkdirAll(filepath.Join(appsDir, name), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(appsDir, name), data, 0644)
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
		m, err := Load(appsDir, e.Name())
		if err == nil {
			manifests = append(manifests, m)
		}
	}
	return manifests, nil
}
