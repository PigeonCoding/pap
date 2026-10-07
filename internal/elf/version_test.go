package elf

import (
	"os"
	"testing"
)

func hostELF(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/curl", "/bin/ls", "/usr/bin/ls"} {
		if IsELF(p) {
			return p
		}
	}
	if exe, err := os.Executable(); err == nil && IsELF(exe) {
		return exe
	}
	t.Skip("no host ELF available for verneed spot-check")
	return ""
}

func TestParseVerneedHost(t *testing.T) {
	p := hostELF(t)
	vn, err := ParseVerneed(p)
	if err != nil {
		t.Fatal(err)
	}
	// Spot-check shape only: every entry must have a file name; version
	// names (if any) must be non-empty. No hardcoded GLIBC version.
	for _, v := range vn {
		if v.File == "" {
			t.Error("verneed entry with empty file")
		}
		for _, ver := range v.Versions {
			if ver.Name == "" {
				t.Error("version requirement with empty name")
			}
		}
	}
}

func TestParseVerdefHost(t *testing.T) {
	for _, p := range []string{"/usr/lib/libc.so.6", "/lib/libc.so.6", "/lib64/libc.so.6", "/usr/lib64/libc.so.6"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		vd, err := ParseVerdef(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(vd) == 0 {
			t.Fatalf("%s: expected version definitions, got none", p)
		}
		found := false
		for k := range vd {
			if k != "" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s: version defs all empty", p)
		}
		return
	}
	t.Skip("no host libc.so.6 found")
}
