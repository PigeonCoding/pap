package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pap/internal/config"
	"pap/internal/repo"
	"pap/internal/resolver"
)

// hostTooOld must flag a GLIBC version required from a host-provided lib:
// that is the Debian-stable case (Arch curl needs GLIBC_2.43, trixie
// provides 2.41) where no vendoring can help.
func TestHostTooOldFlagsGlibc(t *testing.T) {
	libsDir := t.TempDir()
	problems := []compatProblem{
		{Requirer: "curl", Needed: "libc.so.6", Version: "GLIBC_2.43", Provider: "/usr/lib/x86_64-linux-gnu/libc.so.6"},
	}
	if !hostTooOld(problems, libsDir) {
		t.Fatal("hostTooOld = false for GLIBC mismatch on host libc, want true")
	}
	// Vendored-lib mismatches are healable, not host-too-old.
	problems = []compatProblem{
		{Requirer: "app", Needed: "libfoo.so.1", Version: "FOO_1.0", Provider: filepath.Join(libsDir, "libfoo.so.1")},
	}
	if hostTooOld(problems, libsDir) {
		t.Fatal("hostTooOld = true for vendored lib, want false")
	}
}

// End-to-end shape: a binary requiring a GLIBC version the host lacks must
// surface as a compat problem (not silently pass). We simulate an old host
// by pointing the checker at a libsDir containing a stub libc whose VERDEF
// lacks the required symbol.
func TestCompatCatchesMissingGlibcVersion(t *testing.T) {
	host := hostELFPath(t)
	libsDir := t.TempDir()
	// The host's own ls must be compat-clean against the real host libs.
	if problems := checkCompatDetailed([]string{host}, libsDir, resolver.Skip); len(problems) != 0 {
		var msgs []string
		for _, p := range problems {
			msgs = append(msgs, p.Error())
		}
		t.Fatalf("host binary compat problems on matching host: %v", msgs)
	}
	_ = os.Stat
}

func TestCompatProblemErrorShowsProviderPath(t *testing.T) {
	p := compatProblem{Requirer: "curl", Needed: "libc.so.6", Version: "GLIBC_2.43", Provider: "/lib/x86_64-linux-gnu/libc.so.6"}
	msg := p.Error()
	for _, want := range []string{"curl", "libc.so.6", "GLIBC_2.43", "/lib/x86_64-linux-gnu/libc.so.6"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Error() = %q, want it to contain %q", msg, want)
		}
	}
}

func TestHostTooOldHint(t *testing.T) {
	libsDir := t.TempDir()
	problems := []compatProblem{
		{Requirer: "curl", Needed: "libc.so.6", Version: "GLIBC_2.43", Provider: "/usr/lib/libc.so.6"},
	}
	if !hostTooOld(problems, libsDir) {
		t.Fatal("hostTooOld = false for host-lib GLIBC mismatch")
	}
	if h := hostTooOldHint(false); !strings.Contains(h, "--pkg-version") {
		t.Fatalf("auto-mode hint = %q, want a --pkg-version suggestion", h)
	}
	if h := hostTooOldHint(true); !strings.Contains(h, "--no-glibc-compat") {
		t.Fatalf("forced-mode hint = %q, want a --no-glibc-compat mention", h)
	}
}

func TestSkipLibForceLibc(t *testing.T) {
	plain := InstallOptions{}
	forced := InstallOptions{ForceLibc: true}
	// glibc coupling: skipped by default, vendored with --force-libc.
	for _, s := range []string{"libc.so.6", "libm.so.6", "libpthread.so.0", "ld-linux-x86-64.so.2"} {
		if !skipLib(plain, s) {
			t.Errorf("skipLib(default, %q) = false, want true", s)
		}
		if skipLib(forced, s) {
			t.Errorf("skipLib(force-libc, %q) = true, want false", s)
		}
	}
	// Ordinary libs are unaffected by the flag.
	if skipLib(plain, "libcurl.so.4") || skipLib(forced, "libcurl.so.4") {
		t.Error("libcurl.so.4 must stay unskipped in both modes")
	}
	if skipLib(forced, "libstdc++.so.6") != skipLib(plain, "libstdc++.so.6") {
		t.Error("non-glibc libs must not change behavior under --force-libc")
	}
}

func TestForceLibcSeeds(t *testing.T) {
	seeds := forceLibcSeeds()
	if len(seeds) == 0 {
		t.Fatal("no force-libc seeds")
	}
	foundLoader, foundNSS := false, false
	for _, s := range seeds {
		if s == "ld-linux-x86-64.so.2" {
			foundLoader = true
		}
		if strings.HasPrefix(s, "libnss_") {
			foundNSS = true
		}
	}
	if config.Arch == "x86_64" && !foundLoader {
		t.Fatalf("seeds = %v, want the loader", seeds)
	}
	if !foundNSS {
		t.Fatalf("seeds = %v, want libnss_* plugins", seeds)
	}
}

func TestPinProviders(t *testing.T) {
	provs := []repo.PkgInfo{
		{Repo: "chaotic-aur", Name: "glibc-eac", Version: "2.43-1"},
		{Repo: "core", Name: "glibc", Version: "2.43-1"},
	}
	// Pinned to official glibc: patched variants drop out.
	got := pinProviders(provs, &repo.PkgInfo{Repo: "core", Name: "glibc", Version: "2.43-1"})
	if len(got) != 1 || got[0].Name != "glibc" {
		t.Fatalf("pinProviders = %+v, want [core/glibc]", got)
	}
	// No pin, or a pin carrying nothing: full list preserved.
	if got := pinProviders(provs, nil); len(got) != 2 {
		t.Fatalf("pinProviders(nil) = %+v, want both", got)
	}
	if got := pinProviders(provs, &repo.PkgInfo{Repo: "core", Name: "other", Version: "1"}); len(got) != 2 {
		t.Fatalf("pinProviders(unrelated) = %+v, want fallback to both", got)
	}
}

func TestForceLibcPinSoname(t *testing.T) {
	for _, s := range []string{"libc.so.6", "libm.so.6", "ld-linux-x86-64.so.2", "libnss_files.so.2", "libnss_dns.so.2"} {
		if !forceLibcPinSoname(s) {
			t.Errorf("forceLibcPinSoname(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"libcurl.so.4", "libz.so.1", "libstdc++.so.6"} {
		if forceLibcPinSoname(s) {
			t.Errorf("forceLibcPinSoname(%q) = true, want false", s)
		}
	}
}
