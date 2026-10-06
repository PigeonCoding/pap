package repo

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"probe/internal/config"
)

const dbTTL = 6 * time.Hour

var httpClient = &http.Client{Timeout: 5 * time.Minute}

type PkgInfo struct {
	Repo     string
	Name     string
	Version  string
	Arch     string
	Filename string
	SHA256   string
}

type repoDef struct {
	name    string
	servers []string
}

type Index struct {
	mu          sync.RWMutex
	fileMap     map[string][]PkgInfo
	pkgMeta     map[string]PkgInfo // "repo/name" -> package index metadata
	repos       []repoDef
	cacheDir    string
	pkgCacheDir string
	loaded      bool

	// File indexes (<repo>.files) are fetched on demand: they are large and
	// only needed for sonames no %PROVIDES% entry claims.
	filesMu     sync.Mutex
	filesLoaded bool
}

var (
	defaultIndex *Index
	once         sync.Once
)

// Repositories used for resolution: the official Arch repos plus
// chaotic-aur. Nothing else — third-party repos from pacman.conf (cachyos,
// lizardbyte, ...) are deliberately ignored.
var wantedRepos = []string{"core", "extra", "multilib", "chaotic-aur"}

var chaoticServers = []string{
	"https://geo-mirror.chaotic.cx/$repo/$arch",
	"https://cdn-mirror.chaotic.cx/$repo/$arch",
	"https://us-mirror.chaotic.cx/$repo/$arch",
	"https://de-mirror.chaotic.cx/$repo/$arch",
}

func Default() *Index {
	once.Do(func() {
		defaultIndex = &Index{
			fileMap:     make(map[string][]PkgInfo),
			pkgMeta:     make(map[string]PkgInfo),
			repos:       loadRepos(),
			cacheDir:    filepath.Join(os.TempDir(), "arch-repo-cache"),
			pkgCacheDir: config.PkgCacheDir,
		}
	})
	return defaultIndex
}

// loadRepos builds the fixed repo set. Server lists come from pacman.conf
// sections when present (so the user's mirror preference is respected);
// chaotic-aur falls back to its official mirrors, other repos to
// /etc/pacman.d/mirrorlist.
func loadRepos() []repoDef {
	conf := parsePacmanConf()
	mirrors := loadMirrors()

	var defs []repoDef
	for _, name := range wantedRepos {
		var servers []string
		switch {
		case len(conf[name]) > 0:
			servers = conf[name]
		case name == "chaotic-aur":
			servers = chaoticServers
		default:
			servers = mirrors
		}
		defs = append(defs, repoDef{name: name, servers: servers})
	}
	return defs
}

// parsePacmanConf extracts each repo section's Server and Include lines from
// /etc/pacman.conf, keyed by repo name.
func parsePacmanConf() map[string][]string {
	out := map[string][]string{}

	data, err := os.ReadFile("/etc/pacman.conf")
	if err != nil {
		return out
	}
	cur := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			if cur == "options" {
				cur = ""
			}
			continue
		}
		if cur == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "server":
			out[cur] = append(out[cur], strings.TrimSpace(val))
		case "include":
			out[cur] = append(out[cur], mirrorlistServers(strings.TrimSpace(val))...)
		}
	}
	return out
}

func mirrorlistServers(pattern string) []string {
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(raw)
			if !strings.HasPrefix(line, "Server") {
				continue
			}
			if _, val, ok := strings.Cut(line, "="); ok {
				out = append(out, strings.TrimSpace(val))
			}
		}
	}
	return out
}

func loadMirrors() []string {
	data, err := os.ReadFile("/etc/pacman.d/mirrorlist")
	if err != nil {
		return []string{"https://mirror.krfoss.org/archlinux"}
	}
	var mirrors []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Server = ") {
			mirrors = append(mirrors, strings.TrimPrefix(line, "Server = "))
		}
	}
	if len(mirrors) == 0 {
		mirrors = append(mirrors, "https://mirror.krfoss.org/archlinux")
	}
	return mirrors
}

func substVars(s, repoName string) string {
	for _, kv := range config.ArchVars() {
		s = strings.ReplaceAll(s, kv[0], kv[1])
	}
	return strings.ReplaceAll(s, "$repo", repoName)
}

func indexURL(server, repoName, kind string) string {
	return strings.TrimSuffix(substVars(server, repoName), "/") + "/" + repoName + "." + kind
}

func (idx *Index) Load() error {
	idx.mu.RLock()
	done := idx.loaded
	idx.mu.RUnlock()
	if done {
		return nil
	}

	os.MkdirAll(idx.cacheDir, 0755)
	os.MkdirAll(idx.pkgCacheDir, 0755)

	var wg sync.WaitGroup
	errs := make([]error, len(idx.repos))
	for i := range idx.repos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = idx.fetchIndex(idx.repos[i], "db")
		}(i)
	}
	wg.Wait()

	// Parse sequentially in pacman.conf order so provider priority is
	// deterministic and matches pacman's own repo priority.
	for i, def := range idx.repos {
		if errs[i] != nil {
			fmt.Fprintf(os.Stderr, "repo %s: %v\n", def.name, errs[i])
			continue
		}
		if err := idx.parseDb(def.name); err != nil {
			fmt.Fprintf(os.Stderr, "repo %s: parse: %v\n", def.name, err)
		}
	}

	idx.mu.Lock()
	idx.loaded = true
	idx.mu.Unlock()
	return nil
}

func (idx *Index) serverFile(repoName string) string {
	return filepath.Join(idx.cacheDir, repoName+".server")
}

// indexFile is the local cache path for a repo index: kind is "db" (package
// metadata) or "files" (the files each package ships).
func (idx *Index) indexFile(repoName, kind string) string {
	return filepath.Join(idx.cacheDir, repoName+"."+kind)
}

// fetchIndex refreshes a repo index at most every dbTTL, trying servers in
// order and keeping the stale cache when every one fails. The files index
// starts from the mirror that served the package index so both come from the
// same snapshot.
func (idx *Index) fetchIndex(def repoDef, kind string) error {
	file := idx.indexFile(def.name, kind)

	if validIndex(file) && !mtimeOlder(file, dbTTL) {
		return nil
	}

	servers := def.servers
	if kind == "files" {
		servers = idx.orderedServers(def)
	}

	var lastErr error
	for _, srv := range servers {
		tmp := file + ".tmp"
		os.Remove(tmp)
		if err := fetch(indexURL(srv, def.name, kind), tmp); err != nil {
			lastErr = err
			continue
		}
		if !validIndex(tmp) {
			os.Remove(tmp)
			lastErr = fmt.Errorf("invalid index from %s", indexURL(srv, def.name, kind))
			continue
		}
		os.Rename(tmp, file)
		if kind == "db" {
			os.WriteFile(idx.serverFile(def.name), []byte(srv), 0644)
		}
		return nil
	}

	// Refresh failed: keep serving the stale cache rather than nothing.
	if validIndex(file) {
		if kind == "db" {
			fmt.Fprintf(os.Stderr, "warning: repo %s: refresh failed (%v), using cached index\n", def.name, lastErr)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no servers configured")
	}
	return lastErr
}

func (idx *Index) parseDb(repoName string) error {
	dbFile := idx.indexFile(repoName, "db")

	switch dbKind(dbFile) {
	case "gzip":
		return idx.parseGzipDb(repoName, dbFile)
	case "zstd":
		// e.g. chaotic-aur ships zstd-compressed databases; bsdtar (already
		// required for package extraction) handles any libarchive format.
		return idx.parseZstdDb(repoName, dbFile)
	default:
		return fmt.Errorf("unknown db compression in %s", dbFile)
	}
}

func (idx *Index) parseGzipDb(repoName, dbFile string) error {
	f, err := os.Open(dbFile)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !strings.HasSuffix(hdr.Name, "/desc") {
			continue
		}
		descData, err := io.ReadAll(tr)
		if err != nil {
			continue
		}
		idx.parseDesc(repoName, string(descData))
	}
	return nil
}

func (idx *Index) parseZstdDb(repoName, dbFile string) error {
	tmp, err := extractTo(dbFile, "repodb-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	return filepath.Walk(tmp, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, "/desc") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		idx.parseDesc(repoName, string(data))
		return nil
	})
}

// extractTo unpacks an archive into a fresh temp dir and returns its path.
func extractTo(archive, pattern string) (string, error) {
	tmp, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("bsdtar", "-xf", archive, "-C", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("extract %s: %w: %s", filepath.Base(archive), err, out)
	}
	return tmp, nil
}

func (idx *Index) parseDesc(repoName, desc string) {
	info, ok := pkgInfoFromDesc(repoName, desc)
	if !ok {
		return
	}
	idx.pkgMeta[repoName+"/"+info.Name] = info

	for _, soname := range parseProvides(desc) {
		idx.fileMap[soname] = append(idx.fileMap[soname], info)
	}
}

// pkgInfoFromDesc builds a PkgInfo from a package's desc block, which the
// .files index carries too (so file-based lookup gets the same download
// metadata as package-index lookup).
func pkgInfoFromDesc(repoName, desc string) (PkgInfo, bool) {
	name := descField(desc, "NAME")
	version := descField(desc, "VERSION")
	if name == "" || version == "" {
		return PkgInfo{}, false
	}
	return PkgInfo{
		Repo:     repoName,
		Name:     name,
		Version:  version,
		Arch:     descField(desc, "ARCH"),
		Filename: descField(desc, "FILENAME"),
		SHA256:   descField(desc, "SHA256SUM"),
	}, true
}

func descField(desc, key string) string {
	marker := "%" + key + "%"
	inField := false
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "%") {
			if inField {
				return ""
			}
			inField = line == marker
			continue
		}
		if inField && line != "" {
			return line
		}
	}
	return ""
}

func parseProvides(desc string) []string {
	var provides []string
	inProvides := false
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "%") {
			inProvides = line == "%PROVIDES%"
			continue
		}
		if !inProvides || line == "" {
			continue
		}
		if soname, ok := provideToSoname(line); ok {
			provides = append(provides, soname)
		}
	}
	return provides
}

func provideToSoname(provide string) (string, bool) {
	provide = strings.TrimSpace(provide)
	if base, ver, found := strings.Cut(provide, "="); found {
		if !strings.HasSuffix(base, ".so") {
			return "", false
		}
		abi, _, _ := strings.Cut(ver, "-")
		return base + "." + abi, true
	}
	// Bare soname provide, e.g. "libfoo.so.1" or "libfoo.so".
	if strings.HasSuffix(provide, ".so") || looksLikeSoname(provide) {
		return provide, true
	}
	return "", false
}

func looksLikeSoname(s string) bool {
	i := strings.LastIndex(s, ".so.")
	return i > 0 && i+4 <= len(s) && s[i+4] >= '0' && s[i+4] <= '9'
}

// loadFileIndex fetches each repo's files index — the file list pacman keeps
// alongside the package index — and maps every shared object under usr/lib to
// the package shipping it. It runs on first use only: sonames such as
// libpython3.14.so.1.0 are not covered by any %PROVIDES% entry, but the file
// they live in still has to be found.
func (idx *Index) loadFileIndex(soname string) {
	if err := idx.Load(); err != nil {
		return
	}
	idx.filesMu.Lock()
	defer idx.filesMu.Unlock()
	if idx.filesLoaded {
		return
	}
	idx.filesLoaded = true

	fmt.Fprintf(os.Stderr, "  note: %s is not declared in package metadata; loading repo file indexes (downloaded once, then cached)\n", soname)

	type result struct {
		bySoname map[string][]PkgInfo
		err      error
	}
	res := make([]result, len(idx.repos))
	var wg sync.WaitGroup
	for i := range idx.repos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			def := idx.repos[i]
			if err := idx.fetchIndex(def, "files"); err != nil {
				res[i].err = err
				return
			}
			res[i].bySoname, res[i].err = parseFilesIndex(def.name, idx.indexFile(def.name, "files"))
		}(i)
	}
	wg.Wait()

	idx.mu.Lock()
	defer idx.mu.Unlock()
	for i, r := range res {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "warning: repo %s: file index: %v\n", idx.repos[i].name, r.err)
			continue
		}
		// Merged in repo order, appended after the %PROVIDES% entries, so
		// declared providers keep priority and resolution stays deterministic.
		for sn, pkgs := range r.bySoname {
			for _, p := range pkgs {
				// Prefer the package index's copy of the metadata: the file
				// list can be from an older snapshot than the package index.
				if cur, ok := idx.pkgMeta[p.Repo+"/"+p.Name]; ok {
					p = cur
				}
				if !hasProvider(idx.fileMap[sn], p) {
					idx.fileMap[sn] = append(idx.fileMap[sn], p)
				}
			}
		}
	}
}

func hasProvider(list []PkgInfo, p PkgInfo) bool {
	for _, e := range list {
		if e.Repo == p.Repo && e.Name == p.Name {
			return true
		}
	}
	return false
}

// parseFilesIndex maps sonames to the packages shipping them, keyed by the
// shared-object basenames under usr/lib.
func parseFilesIndex(repoName, dbFile string) (map[string][]PkgInfo, error) {
	switch dbKind(dbFile) {
	case "gzip":
		return parseFilesGzipDb(repoName, dbFile)
	case "zstd":
		return parseFilesZstdDb(repoName, dbFile)
	default:
		return nil, fmt.Errorf("unknown db compression in %s", dbFile)
	}
}

func parseFilesGzipDb(repoName, dbFile string) (map[string][]PkgInfo, error) {
	f, err := os.Open(dbFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gr.Close()

	out := map[string][]PkgInfo{}
	tr := tar.NewReader(gr)
	var (
		cur    PkgInfo
		have   bool
		curDir string
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(hdr.Name, "/") {
			continue
		}
		// Entries are grouped per package, desc before files.
		if dir := path.Dir(hdr.Name); dir != curDir {
			curDir, have = dir, false
		}
		switch path.Base(hdr.Name) {
		case "desc":
			data, err := io.ReadAll(tr)
			if err != nil {
				continue
			}
			cur, have = pkgInfoFromDesc(repoName, string(data))
		case "files":
			if !have {
				continue
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				continue
			}
			addLibFiles(out, cur, string(data))
		}
	}
	return out, nil
}

func parseFilesZstdDb(repoName, dbFile string) (map[string][]PkgInfo, error) {
	tmp, err := extractTo(dbFile, "repofiles-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	out := map[string][]PkgInfo{}
	err = filepath.Walk(tmp, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || filepath.Base(p) != "desc" {
			return nil
		}
		descData, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		pkg, ok := pkgInfoFromDesc(repoName, string(descData))
		if !ok {
			return nil
		}
		if filesData, err := os.ReadFile(filepath.Join(filepath.Dir(p), "files")); err == nil {
			addLibFiles(out, pkg, string(filesData))
		}
		return nil
	})
	return out, err
}

// addLibFiles indexes the shared objects pkg ships under usr/lib — the tree
// extractLibs can stage — keyed by basename, which is what a NEEDED entry
// refers to. Directories are listed with a trailing slash and skipped.
func addLibFiles(out map[string][]PkgInfo, pkg PkgInfo, files string) {
	const prefix = "usr/lib/"
	for _, line := range strings.Split(files, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		base := strings.TrimPrefix(line, prefix)
		if base == "" || strings.HasSuffix(base, "/") {
			continue
		}
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:] // plugin subdir, e.g. usr/lib/kitty/libfoo.so.1
		}
		if !strings.Contains(base, ".so") {
			continue
		}
		if !hasProvider(out[base], pkg) {
			out[base] = append(out[base], pkg)
		}
	}
}

// Resolve returns the highest-priority provider for soname: the first
// %PROVIDES% match in pacman.conf order, falling back to the file indexes.
func (idx *Index) Resolve(soname string) (PkgInfo, error) {
	providers := idx.Providers(soname)
	if len(providers) == 0 {
		return PkgInfo{}, fmt.Errorf("no package provides %s", soname)
	}
	return providers[0], nil
}

// Providers returns every package providing soname, in priority order. When
// no package declares it, the repo file indexes are loaded once and consulted
// as a fallback, ranked below declared providers.
func (idx *Index) Providers(soname string) []PkgInfo {
	if out := idx.lookup(soname); len(out) > 0 {
		return out
	}
	idx.loadFileIndex(soname)
	return idx.lookup(soname)
}

func (idx *Index) lookup(soname string) []PkgInfo {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return append([]PkgInfo(nil), idx.fileMap[soname]...)
}

func (idx *Index) defFor(repoName string) (repoDef, bool) {
	for _, d := range idx.repos {
		if d.name == repoName {
			return d, true
		}
	}
	return repoDef{}, false
}

// orderedServers returns the mirror that served the .db first, then the
// repo's remaining servers, so a download can't silently mix snapshots.
func (idx *Index) orderedServers(def repoDef) []string {
	var pinned string
	if b, err := os.ReadFile(idx.serverFile(def.name)); err == nil {
		pinned = strings.TrimSpace(string(b))
	}
	var out []string
	if pinned != "" {
		out = append(out, pinned)
	}
	for _, s := range def.servers {
		if s != pinned {
			out = append(out, s)
		}
	}
	return out
}

func (idx *Index) pkgURL(server string, pkg PkgInfo) string {
	fn := pkg.Filename
	if fn == "" {
		fn = fmt.Sprintf("%s-%s-%s.pkg.tar.zst", pkg.Name, pkg.Version, pkg.Arch)
	}
	return strings.TrimSuffix(substVars(server, pkg.Repo), "/") + "/" + fn
}

func (idx *Index) pkgFileName(pkg PkgInfo) string {
	fn := pkg.Filename
	if fn == "" {
		fn = fmt.Sprintf("%s-%s-%s.pkg.tar.zst", pkg.Name, pkg.Version, pkg.Arch)
	}
	if filepath.Base(fn) != fn || fn == "" {
		return ""
	}
	return fn
}

// Download fetches the exact package recorded in the index, verified against
// the index's SHA256. Downloads are cached on disk keyed by filename.
func (idx *Index) Download(pkg PkgInfo) ([]byte, error) {
	fn := idx.pkgFileName(pkg)
	if fn == "" {
		return nil, fmt.Errorf("bad filename for %s %s", pkg.Name, pkg.Version)
	}

	cacheFile := filepath.Join(idx.pkgCacheDir, fn)
	if data, err := os.ReadFile(cacheFile); err == nil {
		if pkg.SHA256 == "" || sha256hex(data) == pkg.SHA256 {
			return data, nil
		}
		os.Remove(cacheFile)
	}

	def, ok := idx.defFor(pkg.Repo)
	if !ok {
		return nil, fmt.Errorf("unknown repo %q", pkg.Repo)
	}

	servers := idx.orderedServers(def)
	var lastErr error
	for i, srv := range servers {
		if i > 0 {
			fmt.Fprintf(os.Stderr, "warning: %s: trying fallback mirror %s\n", fn, srv)
		}
		data, err := get(idx.pkgURL(srv, pkg))
		if err != nil {
			lastErr = err
			continue
		}
		if sum := pkg.SHA256; sum != "" && sha256hex(data) != sum {
			lastErr = fmt.Errorf("checksum mismatch for %s from %s", fn, srv)
			continue
		}
		if err := atomicWrite(cacheFile, data); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pkg cache write: %v\n", err)
		}
		return data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no servers for repo %s", pkg.Repo)
	}
	return nil, fmt.Errorf("download %s: %w", fn, lastErr)
}

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func get(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func fetch(url, dest string) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func mtimeOlder(path string, d time.Duration) bool {
	st, err := os.Stat(path)
	if err != nil {
		return true
	}
	return time.Since(st.ModTime()) > d
}

// dbKind detects the repo index compression: "gzip" or "zstd" (used by
// chaotic-aur), "" if neither.
func dbKind(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return ""
	}
	if hdr[0] == 0x1f && hdr[1] == 0x8b {
		return "gzip"
	}
	if hdr[0] == 0x28 && hdr[1] == 0xb5 && hdr[2] == 0x2f && hdr[3] == 0xfd {
		return "zstd"
	}
	return ""
}

func validIndex(path string) bool {
	return dbKind(path) != ""
}
