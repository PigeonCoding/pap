package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pap/internal/config"
)

func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PAP_HOME", dir)
	config.Refresh()
}

func oldTime() time.Time { return time.Now().Add(-time.Hour) }

func TestStoreRoundTripAndGC(t *testing.T) {
	isolate(t)
	data := []byte("fake-so-bytes")
	p, err := Store(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stored file missing: %v", err)
	}
	// Same content returns same path without rewrite.
	p2, err := Store(data)
	if err != nil || p != p2 {
		t.Fatalf("Store twice = %q,%v want %q", p2, err, p)
	}
	// GC keeps referenced entries, removes the rest after grace.
	// Make the file old enough to pass gcGrace.
	old := p
	if err := os.Chtimes(old, oldTime(), oldTime()); err != nil {
		t.Skipf("chtimes: %v", err)
	}
	used := map[string]bool{filepath.Base(p): true}
	if n, err := GC(used); err != nil || n != 0 {
		t.Fatalf("GC used = %d,%v want 0", n, err)
	}
	if n, err := GC(map[string]bool{}); err != nil || n != 1 {
		t.Fatalf("GC unused = %d,%v want 1", n, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("expected stored file removed")
	}
}

func TestStorePreservesMode(t *testing.T) {
	isolate(t)
	data := []byte("mode-bytes-unique-123")
	p, err := StoreWithMode(data, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0644 {
		t.Fatalf("mode = %o, want 644", fi.Mode().Perm())
	}
}

func TestScanUsed(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(config.AppsDir, 0755); err != nil {
		t.Fatal(err)
	}
	data := []byte("scan-used-bytes")
	sp, err := Store(data)
	if err != nil {
		t.Fatal(err)
	}
	libs := filepath.Join(config.AppsDir, "demo", "libs")
	if err := os.MkdirAll(libs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := Symlink(sp, filepath.Join(libs, "libdemo.so.1")); err != nil {
		t.Fatal(err)
	}
	used := ScanUsed()
	if !used[filepath.Base(sp)] {
		t.Fatalf("ScanUsed missing %s: %v", sp, used)
	}
}
