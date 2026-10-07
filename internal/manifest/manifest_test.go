package manifest

import (
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "demo")
	m := &Manifest{Name: "demo", Binary: "demo", Libs: []string{"liba.so.1"}, Packages: map[string]string{"core/a": "1-1"}, Source: "/bin/ls", SourceSHA256: "abc", PlacedBinary: true}
	if err := Save(app, m); err != nil {
		t.Fatal(err)
	}
	got, err := Load(app)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "demo" || got.SourceSHA256 != "abc" || !got.PlacedBinary || got.Packages["core/a"] != "1-1" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestListSkipsBadDirs(t *testing.T) {
	dir := t.TempDir()
	if err := Save(filepath.Join(dir, "ok"), &Manifest{Name: "ok"}); err != nil {
		t.Fatal(err)
	}
	// No manifest here: List must skip, not fail.
	if ms, err := List(dir); err != nil || len(ms) != 1 || ms[0].Name != "ok" {
		t.Fatalf("List = %v,%v", ms, err)
	}
}
