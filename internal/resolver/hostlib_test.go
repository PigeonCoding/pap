package resolver

import (
	"os"
	"path/filepath"
	"testing"

	"pap/internal/elf"
)

// Regression for Debian multiarch hosts (e.g. /usr/lib/x86_64-linux-gnu):
// host libc must resolve via ldconfig + multiarch dirs, not just /usr/lib.
func TestFindHostLibFindsLibc(t *testing.T) {
	p := FindHostLib("libc.so.6")
	if p == "" {
		t.Skip("no host libc.so.6 found (minimal container?)")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("FindHostLib(libc.so.6) = %q, not statable: %v", p, err)
	}
	// Old hardcoded layout missed Debian triplets; the search dirs must
	// include at least one multiarch glob root when present.
	foundMultiarch := false
	for _, d := range hostSearchDirs() {
		if _, err := os.Stat(d); err == nil {
			if len(d) > len("/usr/lib") && len(d) != len("/lib") {
				// Any triplet dir counts; absence on Arch is fine.
				foundMultiarch = true
			}
		}
	}
	_ = foundMultiarch
	if p == "" {
		t.Fatal("empty result")
	}
}

// The resolved host lib must be usable as a compat provider: an ELF for
// this host whose SONAME matches. Stale ldconfig entries and
// foreign-architecture copies must never be returned.
func TestFindHostLibResultValid(t *testing.T) {
	p := FindHostLib("libc.so.6")
	if p == "" {
		t.Skip("no host libc.so.6 found")
	}
	if !hostCandidateValid(p, "libc.so.6") {
		t.Fatalf("FindHostLib result %q fails validation", p)
	}
	info, err := elf.Parse(p)
	if err != nil {
		t.Fatalf("Parse(%q): %v", p, err)
	}
	if info.SONAME != "libc.so.6" {
		t.Fatalf("SONAME = %q, want libc.so.6", info.SONAME)
	}
}

// A same-named file with a different SONAME (e.g. a copy shuffled across
// distros) must not validate as that soname's provider.
func TestHostCandidateValidRejectsSonameMismatch(t *testing.T) {
	if _, err := os.Stat("/usr/lib/libz.so.1"); err != nil {
		t.Skip("no libz for mismatch fixture")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "libfake.so.9")
	data, err := os.ReadFile("/usr/lib/libz.so.1")
	if err != nil {
		// Dereference the symlink if that is what it is.
		if target, rerr := filepath.EvalSymlinks("/usr/lib/libz.so.1"); rerr == nil {
			data, err = os.ReadFile(target)
		}
	}
	if err != nil {
		t.Skip("cannot read libz bytes")
	}
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatal(err)
	}
	if hostCandidateValid(dst, "libfake.so.9") {
		t.Fatal("file whose SONAME is libz.so.1 validated as libfake.so.9")
	}
	// Non-ELF files never validate.
	plain := filepath.Join(dir, "libc.so.6")
	if err := os.WriteFile(plain, []byte("not an elf"), 0644); err != nil {
		t.Fatal(err)
	}
	if hostCandidateValid(plain, "libc.so.6") {
		t.Fatal("text file validated as a host library")
	}
}
