package install

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"pap/internal/repo"
)

func makePkgFile(t *testing.T, pkginfo string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data := []byte(pkginfo)
	if err := tw.WriteHeader(&tar.Header{Name: ".PKGINFO", Mode: 0644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	tw.Close()

	f := filepath.Join(t.TempDir(), "pkg.tar")
	if err := os.WriteFile(f, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestVerifyPkgFile(t *testing.T) {
	f := makePkgFile(t, "pkgname = curl\npkgver = 8.22.0-1\narch = x86_64\n")
	want := repo.PkgInfo{Repo: "core", Name: "curl", Version: "8.22.0-1", Arch: "x86_64"}

	if err := verifyPkgFile(f, want); err != nil {
		t.Fatalf("expected match, got: %v", err)
	}

	bad := want
	bad.Version = "9.9.9-1"
	if err := verifyPkgFile(f, bad); err == nil {
		t.Fatal("expected version mismatch error")
	}

	bad = want
	bad.Name = "wget"
	if err := verifyPkgFile(f, bad); err == nil {
		t.Fatal("expected pkgname mismatch error")
	}
}
