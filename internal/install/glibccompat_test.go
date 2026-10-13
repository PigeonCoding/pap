package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func errHostGlibcSample() error {
	return fmt.Errorf("curl: libc.so.6 requires GLIBC_2.43 not provided by libc.so.6\nhost glibc is older")
}

func errMissingDepSample() error {
	return fmt.Errorf("app: missing dependency libfoo.so.1")
}

func TestParseGlibcVersion(t *testing.T) {
	for _, tc := range []struct {
		in       string
		maj, min int
		ok       bool
	}{
		{"GLIBC_2.43", 2, 43, true},
		{"GLIBC_2.17", 2, 17, true},
		{"GLIBC_2.2.5", 0, 0, false}, // only major.minor shape counts
		{"GLIBC_PRIVATE", 0, 0, false},
		{"GCC_14.0", 0, 0, false},
		{"", 0, 0, false},
	} {
		maj, min, ok := parseGlibcVersion(tc.in)
		if maj != tc.maj || min != tc.min || ok != tc.ok {
			t.Errorf("parseGlibcVersion(%q) = (%d,%d,%v), want (%d,%d,%v)",
				tc.in, maj, min, ok, tc.maj, tc.min, tc.ok)
		}
	}
}

func TestPkgVerCmp(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"8.11.1-3", "8.11.1-3", 0},
		{"8.11.1-3", "8.12.1-1", -1},
		{"8.12.1-1", "8.11.1-3", 1},
		{"1.0-2", "1.0-10", -1},   // numeric pkgrel, not lexical
		{"2.41-1", "2.43-1", -1},  // glibc-style
		{"1:1.0-1", "2.0-1", 1},   // epoch wins
		{"1:1.0-1", "1:1.0-1", 0}, // same epoch
		{"1.0a", "1.0b", -1},
		{"1.0", "1.0-1", -1},
	} {
		if got := pkgVerCmp(tc.a, tc.b); got != tc.want {
			t.Errorf("pkgVerCmp(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := pkgVerCmp(tc.b, tc.a); got != -tc.want {
			t.Errorf("pkgVerCmp(%q,%q) = %d, want %d (antisymmetry)", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestHostGlibcSane(t *testing.T) {
	maj, min, p, ok := HostGlibc()
	if !ok {
		t.Skip("no host libc with version definitions found")
	}
	if maj < 2 || min < 0 {
		t.Fatalf("HostGlibc = %d.%d, implausible", maj, min)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("HostGlibc path %q not statable: %v", p, err)
	}
}

func TestRequiredGlibcWithinHost(t *testing.T) {
	host := hostELFPath(t)
	reqMaj, reqMin, reqOK := RequiredGlibc(host)
	if !reqOK {
		t.Skip("host binary has no GLIBC requirements to compare")
	}
	hostMaj, hostMin, _, hostOK := HostGlibc()
	if !hostOK {
		t.Skip("unknown host glibc")
	}
	if !glibcLE(hostMaj, hostMin, reqMaj, reqMin) {
		t.Fatalf("host binary requires GLIBC_%s but host provides GLIBC_%s",
			formatGlibc(reqMaj, reqMin), formatGlibc(hostMaj, hostMin))
	}
}

func TestPayloadHostCompatibleWithHostBinary(t *testing.T) {
	host := hostELFPath(t)
	dir := t.TempDir()
	data, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "appbin"), data, 0755); err != nil {
		t.Fatal(err)
	}
	ok, _, _, _ := PayloadHostCompatible(dir)
	if !ok {
		t.Fatal("payload built from the host's own binary reported incompatible with the host")
	}
}

func TestIsHostGlibcError(t *testing.T) {
	if !IsHostGlibcError(errHostGlibcSample()) {
		t.Error("IsHostGlibcError = false for host-glibc failure, want true")
	}
	if IsHostGlibcError(errMissingDepSample()) {
		t.Error("IsHostGlibcError = true for missing dep, want false")
	}
}

func TestVersionsFromListing(t *testing.T) {
	html := `<html><body>
<a href="curl-8.11.1-3-x86_64.pkg.tar.zst">curl-8.11.1-3-x86_64.pkg.tar.zst</a>
<a href="curl-8.12.1-1-x86_64.pkg.tar.zst">curl-8.12.1-1-x86_64.pkg.tar.zst</a>
<a href="curl-8.12.1-1-x86_64.pkg.tar.zst.sig">sig</a>
<a href="curl-8.10.1-2-aarch64.pkg.tar.zst">other arch</a>
<a href="curl-8.9.1-1-any.pkg.tar.zst">any arch</a>
<a href="curl-1%3A9.0-1-x86_64.pkg.tar.zst">epoch</a>
<a href="other-1.0-1-x86_64.pkg.tar.zst">other pkg</a>
</body></html>`
	got := versionsFromListing(html, "curl")
	// Newest first; exact head depends on host arch (x86_64 here keeps the
	// epoch build out of the way below: epoch 1 beats everything).
	if len(got) == 0 {
		t.Fatal("no versions parsed")
	}
	for _, v := range got {
		if strings.HasSuffix(v, ".sig") || v == "" {
			t.Fatalf("bad entry %q", v)
		}
	}
	// Epoch version must sort above all non-epoch ones.
	if got[0] != "1:9.0-1" {
		t.Fatalf("versions = %v, want epoch 1:9.0-1 first", got)
	}
	// Non-epoch order must be descending.
	var plain []string
	for _, v := range got[1:] {
		plain = append(plain, v)
	}
	want := []string{"8.12.1-1", "8.11.1-3", "8.9.1-1"}
	if len(plain) != len(want) {
		t.Fatalf("versions = %v, want %v after epoch", got, append([]string{"1:9.0-1"}, want...))
	}
	for i := range want {
		if plain[i] != want[i] {
			t.Fatalf("versions = %v, want %v after epoch", got, append([]string{"1:9.0-1"}, want...))
		}
	}
}
