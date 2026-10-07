package repo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProvideToSoname(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantOK  bool
	}{
		{"libcurl.so=4-64", "libcurl.so.4", true},
		{"libz.so=1-64", "libz.so.1", true},
		{"libfoo.so.1", "libfoo.so.1", true},
		{"libfoo.so", "libfoo.so", true},
		{"python3", "", false},
		{"libfoo.so.1=1-64", "", false}, // versioned base must end in .so
		{"foo=1-2", "", false},
		{"", "", false},
		{"  libbar.so=2-64  ", "libbar.so.2", true},
	}
	for _, c := range cases {
		got, ok := provideToSoname(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("provideToSoname(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestDescField(t *testing.T) {
	desc := "%NAME%\ncurl\n\n%VERSION%\n8.22.0-1\n\n%ARCH%\nx86_64\n"
	if got := descField(desc, "NAME"); got != "curl" {
		t.Errorf("NAME = %q, want curl", got)
	}
	if got := descField(desc, "VERSION"); got != "8.22.0-1" {
		t.Errorf("VERSION = %q", got)
	}
	if got := descField(desc, "MISSING"); got != "" {
		t.Errorf("MISSING = %q, want empty", got)
	}
	// Empty field body yields empty.
	if got := descField("%NAME%\n\n%VERSION%\n1\n", "NAME"); got != "" {
		t.Errorf("empty NAME = %q, want empty", got)
	}
}

func TestParseProvides(t *testing.T) {
	desc := "%PROVIDES%\nlibcurl.so=4-64\nlibfoo.so.1\npython3\n\n%OTHER%\nx\n"
	got := parseProvides(desc)
	want := map[string]bool{"libcurl.so.4": true, "libfoo.so.1": true}
	if len(got) != len(want) {
		t.Fatalf("parseProvides = %v, want %v", got, want)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected provide %q", s)
		}
	}
}

func TestPkgURLAndFileName(t *testing.T) {
	idx := &Index{cacheDir: t.TempDir()}
	def := repoDef{name: "core", servers: []string{"https://mirror.example/$repo/os/$arch"}}
	_ = def
	pkg := PkgInfo{Repo: "core", Name: "curl", Version: "8.22.0-1", Arch: "x86_64", Filename: "curl-8.22.0-1-x86_64.pkg.tar.zst"}
	url := idx.pkgURL("https://mirror.example/$repo/os/$arch", pkg)
	want := "https://mirror.example/core/os/x86_64/curl-8.22.0-1-x86_64.pkg.tar.zst"
	if url != want {
		t.Errorf("pkgURL = %q, want %q", url, want)
	}
	// Empty filename falls back to constructed name.
	pkg.Filename = ""
	if fn := idx.pkgFileName(pkg); fn != "curl-8.22.0-1-x86_64.pkg.tar.zst" {
		t.Errorf("pkgFileName fallback = %q", fn)
	}
	// Path traversal rejected.
	pkg.Filename = "../evil.pkg.tar.zst"
	if fn := idx.pkgFileName(pkg); fn != "" {
		t.Errorf("traversal filename = %q, want empty", fn)
	}
}

func TestOrderedServers(t *testing.T) {
	idx := &Index{cacheDir: t.TempDir()}
	def := repoDef{name: "core", servers: []string{"https://a.example/$repo", "https://b.example/$repo"}}
	if got := idx.orderedServers(def); len(got) != 2 || got[0] != def.servers[0] {
		t.Fatalf("no pin: orderedServers = %v", got)
	}
	if err := os.WriteFile(idx.serverFile("core"), []byte(def.servers[1]), 0644); err != nil {
		t.Fatal(err)
	}
	got := idx.orderedServers(def)
	if len(got) != 2 || got[0] != def.servers[1] || got[1] != def.servers[0] {
		t.Fatalf("pinned: orderedServers = %v", got)
	}
	// Stale pin dropped.
	if err := os.WriteFile(idx.serverFile("core"), []byte("https://old.example/$repo"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := idx.orderedServers(def); len(got) != 2 || got[0] != def.servers[0] {
		t.Fatalf("stale pin: orderedServers = %v", got)
	}
	_ = filepath.Separator
}
