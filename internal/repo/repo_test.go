package repo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"probe/internal/config"
)

type tarEntry struct{ name, body string }

func gzipTar(entries []tarEntry) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0644, Size: int64(len(e.body))})
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func desc(name, version, provides string) string {
	out := "%FILENAME%\n" + name + "-" + version + "-x86_64.pkg.tar.zst\n\n" +
		"%NAME%\n" + name + "\n\n" +
		"%VERSION%\n" + version + "\n\n" +
		"%ARCH%\nx86_64\n\n" +
		"%SHA256SUM%\nsum-" + name + "\n"
	if provides != "" {
		out += "\n%PROVIDES%\n" + provides + "\n"
	}
	return out
}

// testIndex builds an index whose core.db and core.files are already cached,
// so nothing is fetched: both refreshes short-circuit on the fresh cache.
func testIndex(t *testing.T, dbEntries, fileEntries []tarEntry) *Index {
	t.Helper()
	cacheDir := t.TempDir()
	for file, entries := range map[string][]tarEntry{"core.db": dbEntries, "core.files": fileEntries} {
		if err := os.WriteFile(filepath.Join(cacheDir, file), gzipTar(entries), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return &Index{
		fileMap:  make(map[string][]PkgInfo),
		pkgMeta:  make(map[string]PkgInfo),
		repos:    []repoDef{{name: "core", servers: []string{"http://127.0.0.1:0"}}},
		cacheDir: cacheDir,
	}
}

func TestProvidersDeclaredSonameSkipsFileIndex(t *testing.T) {
	idx := testIndex(t,
		[]tarEntry{{"zlib-1.3.2-3/desc", desc("zlib", "1.3.2-3", "libz.so=1-64")}},
		[]tarEntry{{"zlib-1.3.2-3/files", "%FILES%\nusr/lib/\nusr/lib/libz.so.1\n"}},
	)
	if err := idx.Load(); err != nil {
		t.Fatal(err)
	}

	providers := idx.Providers("libz.so.1")
	if len(providers) != 1 || providers[0].Name != "zlib" {
		t.Fatalf("Providers(libz.so.1) = %+v, want [zlib]", providers)
	}
	if idx.filesLoaded {
		t.Fatal("file index loaded for a soname covered by %PROVIDES%")
	}
}

func TestProvidersFallsBackToFileIndex(t *testing.T) {
	idx := testIndex(t,
		[]tarEntry{
			// python ships libpython3.14.so.1.0 but declares no soname provides.
			{"python-3.14.7-1/desc", desc("python", "3.14.7-1", "python3")},
			{"zlib-1.3.2-3/desc", desc("zlib", "1.3.2-3", "libz.so=1-64")},
		},
		[]tarEntry{
			// The file list is from an older snapshot than the package index.
			{"python-3.14.6-1/desc", desc("python", "3.14.6-1", "python3")},
			{"python-3.14.6-1/files", "%FILES%\nusr/\nusr/bin/\nusr/bin/python\n" +
				"usr/lib/\nusr/lib/libpython3.14.so\nusr/lib/libpython3.14.so.1.0\n"},
			{"zlib-1.3.2-3/desc", desc("zlib", "1.3.2-3", "libz.so=1-64")},
			{"zlib-1.3.2-3/files", "%FILES%\nusr/lib/\nusr/lib/libz.so.1\n"},
		},
	)
	if err := idx.Load(); err != nil {
		t.Fatal(err)
	}

	providers := idx.Providers("libpython3.14.so.1.0")
	if len(providers) != 1 {
		t.Fatalf("Providers(libpython3.14.so.1.0) = %+v, want exactly one provider", providers)
	}
	want := PkgInfo{
		Repo:     "core",
		Name:     "python",
		Version:  "3.14.7-1",
		Arch:     "x86_64",
		Filename: "python-3.14.7-1-x86_64.pkg.tar.zst",
		SHA256:   "sum-python",
	}
	if got := providers[0]; got != want {
		t.Errorf("Providers(libpython3.14.so.1.0) = %+v, want %+v", got, want)
	}
	if !idx.filesLoaded {
		t.Error("file index not loaded for an undeclared soname")
	}

	// The declared provider must stay ahead of the fallback and must not be
	// listed twice once the file index names it as well.
	if providers = idx.Providers("libz.so.1"); len(providers) != 1 {
		t.Errorf("Providers(libz.so.1) = %+v, want a single declared provider", providers)
	}
	if p, err := idx.Resolve("libpython3.14.so.1.0"); err != nil || p.Name != "python" {
		t.Errorf("Resolve(libpython3.14.so.1.0) = %+v, %v; want python", p, err)
	}
	if _, err := idx.Resolve("libnothing.so.9"); err == nil {
		t.Error("Resolve(libnothing.so.9) = nil error, want failure")
	}
}

func TestLoadReposPrefersCustomMirrorlist(t *testing.T) {
	want := []string{
		"https://fastly.example/$repo/os/$arch",
		"https://geo.example/$repo/os/$arch",
	}
	file := filepath.Join(t.TempDir(), "mirrorlist")
	body := "#Server = https://commented-out/\n"
	for _, s := range want {
		body += "Server = " + s + "\n"
	}
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	orig := config.Mirrorlist
	config.Mirrorlist = file
	t.Cleanup(func() { config.Mirrorlist = orig })

	byName := map[string][]string{}
	for _, d := range loadRepos() {
		byName[d.name] = d.servers
	}
	for _, name := range []string{"core", "extra", "multilib"} {
		if !slices.Equal(byName[name], want) {
			t.Errorf("%s servers = %v, want %v", name, byName[name], want)
		}
	}
	if slices.Contains(byName["chaotic-aur"], want[0]) {
		t.Errorf("chaotic-aur servers = %v, must not use the Arch mirrors", byName["chaotic-aur"])
	}
}

func TestOrderedServersDropsStalePin(t *testing.T) {
	idx := &Index{cacheDir: t.TempDir()}
	def := repoDef{name: "core", servers: []string{"https://new.example/$repo/os/$arch"}}

	if err := os.WriteFile(idx.serverFile("core"), []byte("https://old.example/$repo/os/$arch"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := idx.orderedServers(def); !slices.Equal(got, def.servers) {
		t.Errorf("stale pin: orderedServers = %v, want %v", got, def.servers)
	}

	if err := os.WriteFile(idx.serverFile("core"), []byte(def.servers[0]), 0644); err != nil {
		t.Fatal(err)
	}
	if got := idx.orderedServers(def); !slices.Equal(got, def.servers) {
		t.Errorf("current pin: orderedServers = %v, want %v", got, def.servers)
	}
}
