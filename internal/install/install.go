package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pap/internal/config"
	"pap/internal/elf"
	"pap/internal/lock"
	"pap/internal/manifest"
	"pap/internal/repo"
	"pap/internal/resolver"
	"pap/internal/store"
)

// Version is the binary version (overridden via ldflags).
var Version = "0.2"

// InstallOptions controls install behavior.
type InstallOptions struct {
	Force       bool              // overwrite an existing app of the same name
	Locked      map[string]string // exact "repo/name" -> version lock (reinstall)
	DryRun      bool              // resolve and print plan without modifying apps/bin
	Quiet       bool              // suppress progress output
	Exe         string            // payload installs: explicit main executable, relative to the payload root
	Out         io.Writer         // progress output (nil = stdout)
	ArchiveDate string            // YYYY-MM-DD snapshot for ALA fallback ("" = auto from ELF mtime)
	NoArchive   bool              // disable the archive.org fallback entirely
	// ManifestSource/ManifestPackage override the manifest's source record.
	// Set by InstallRepoApp so reinstall/upgrade can re-fetch the same
	// package instead of pointing at a local path.
	ManifestSource  string
	ManifestPackage string
}

func (o InstallOptions) out() io.Writer {
	if o.Out != nil {
		return o.Out
	}
	return os.Stdout
}

func (o InstallOptions) printf(format string, args ...any) {
	if o.Quiet {
		return
	}
	fmt.Fprintf(o.out(), format, args...)
}

// Preflight checks external requirements with a friendly error.
func Preflight() error {
	var missing []string
	for _, bin := range []string{"bsdtar", "patchelf"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required tools: %s (install with: sudo pacman -S %s)", strings.Join(missing, ", "), strings.Join(missing, " "))
	}
	return nil
}

// Doctor reports environment health (used by `pkg doctor`).
func Doctor() error {
	if err := Preflight(); err != nil {
		return err
	}
	repo.EnsureMirrorlist()
	for _, d := range []string{config.AppsDir, config.ExeDir, config.StoreDir, config.PkgCacheDir, config.RepoCacheDir, config.BinDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("cannot create %s: %w", d, err)
		}
	}
	fmt.Println("doctor: bsdtar + patchelf found, state dirs writable")
	fmt.Printf("  arch: %s  base: %s  bin: %s\n", config.Arch, config.BaseDir, config.BinDir)
	return nil
}

// prepareInstall checks for existing installs and readies state dirs.
// Callers must hold the cross-process lock.
func prepareInstall(name string, force bool) error {
	if err := store.Init(); err != nil {
		return err
	}
	repo.EnsureMirrorlist()
	if err := os.MkdirAll(config.AppsDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(config.ExeDir, 0755); err != nil {
		return err
	}
	pruneStaleStages()

	if !force {
		if _, err := os.Stat(filepath.Join(config.AppsDir, name)); err == nil {
			return fmt.Errorf("app %q already installed (use --force to overwrite)", name)
		}
		if _, err := os.Stat(filepath.Join(config.ExeDir, name)); err == nil {
			return fmt.Errorf("app %q already installed (use --force to overwrite)", name)
		}
		if _, err := os.Lstat(filepath.Join(config.BinDir, name)); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", filepath.Join(config.BinDir, name))
		}
	}
	return nil
}

// stager owns a paired staging area: app (libs + manifest, published to
// apps/<name>) and exe (binary payload, published to exe/<name>).
type stager struct {
	app       string
	exe       string
	committed bool
}

func stagePair() (*stager, error) {
	app, err := os.MkdirTemp(config.AppsDir, ".stage-*")
	if err != nil {
		return nil, err
	}
	exe, err := os.MkdirTemp(config.ExeDir, ".stage-*")
	if err != nil {
		os.RemoveAll(app)
		return nil, err
	}
	return &stager{app: app, exe: exe}, nil
}

func (s *stager) cleanup() {
	if !s.committed {
		os.RemoveAll(s.app)
		os.RemoveAll(s.exe)
	}
}

func (s *stager) commit() { s.committed = true }

// checkELFHost validates an ELF against the host (arch + interpreter).
func checkELFHost(abs string, info *elf.LibInfo) error {
	if err := elf.MachineMismatch(info.Machine, config.Arch); err != nil {
		return err
	}
	if info.Interp != "" {
		if _, err := os.Stat(info.Interp); err != nil {
			return fmt.Errorf("interpreter %s not found on host (unsupported binary, e.g. musl)", info.Interp)
		}
	}
	if st, _ := os.Stat(abs); st != nil && st.Mode()&0o4000 != 0 {
		fmt.Println("warning: source is setuid; the installed copy drops the setuid bit")
	}
	return nil
}

// patchPrivateLibs rewrites vendored libs carrying their own RPATH/RUNPATH
// to $ORIGIN: otherwise they would bypass the exe's RPATH chain and leak to
// system libs.
func patchPrivateLibs(privateLibs []string, libsDir string) error {
	if len(privateLibs) > 0 {
		fmt.Println("\nPatching vendored libs with their own RUNPATH...")
	}
	for _, rel := range privateLibs {
		if err := patchelfRPATH(filepath.Join(libsDir, rel), "$ORIGIN"); err != nil {
			return fmt.Errorf("patchelf %s: %w", rel, err)
		}
	}
	return nil
}

// installDir installs a materialized payload tree (a folder install, or a
// repo package extracted to a temp dir): the tree is copied verbatim to
// ~/.pap/exe/<name>/pkg/ (layout preserved, so sibling resources and plain
// `lib/` lookups keep working), every ELF in the tree is patched in place
// so its .so deps resolve to apps/<name>/libs, and ~/.local/bin/<name>
// symlinks to the main entrypoint (which may itself be a shebang script —
// the surrounding ELFs are still patched, so the script stays local).
// It takes the cross-process lock and prepares state dirs itself.
func installDir(absDir, name string, opts InstallOptions) error {
	if err := Preflight(); err != nil {
		return err
	}
	lh, err := lock.Acquire(config.LockFile)
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer lh.Release()

	if err := validName(name); err != nil {
		return err
	}
	if err := prepareInstall(name, opts.Force); err != nil {
		return err
	}
	var mainRel string
	if opts.Exe != "" {
		var err error
		mainRel, err = resolveExplicitExe(absDir, opts.Exe)
		if err != nil {
			return err
		}
	} else {
		var err error
		mainRel, err = resolveMainExe(absDir, name)
		if err != nil {
			return err
		}
	}
	absMain := filepath.Join(absDir, mainRel)
	mainIsScript := !elf.IsELF(absMain) && elf.IsScript(absMain)
	var info *elf.LibInfo
	if !mainIsScript {
		var err error
		info, err = elf.Parse(absMain)
		if err != nil {
			return err
		}
		if err := checkELFHost(absMain, info); err != nil {
			return err
		}
	}
	srcHash := fileSHA256Hex(absMain)

	opts.printf("Installing %s from %s (main executable: %s)\n", name, absDir, mainRel)
	if mainIsScript {
		opts.printf("Main entrypoint is a script; patching bundled ELFs...\n")
	} else {
		opts.printf("Direct NEEDED: %v\n", info.NEEDED)
	}

	if err := repo.Default().Load(); err != nil {
		return fmt.Errorf("load repo index: %w", err)
	}

	// Every ELF in the folder contributes its DT_NEEDED: the main binary
	// is just one of potentially many (helpers, plugins, sub-commands).
	// A script entrypoint has no NEEDED of its own — its children do.
	// Helpers built for another architecture (e.g. a bundled arm64 test
	// proxy) can neither run nor resolve here: skip them with a warning
	// instead of failing the install. The main executable already passed
	// the strict check above.
	folderELFs := listFolderELFs(absDir)
	var skippedForeign []string
	var keptELFs []string
	for _, p := range folderELFs {
		fi, err := elf.Parse(p)
		if err != nil {
			continue
		}
		if err := elf.MachineMismatch(fi.Machine, config.Arch); err != nil {
			rel := relTo(absDir, p)
			opts.printf("  [skip] %s: %v\n", rel, err)
			skippedForeign = append(skippedForeign, rel)
			continue
		}
		if err := checkELFHost(p, fi); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		keptELFs = append(keptELFs, p)
	}
	folderELFs = keptELFs
	skipSet := map[string]bool{}
	for _, rel := range skippedForeign {
		skipSet[rel] = true
	}
	var toResolve []string
	if info != nil {
		for _, lib := range info.NEEDED {
			if !resolver.Skip(lib) {
				toResolve = append(toResolve, lib)
			}
		}
	}
	toResolve = append(toResolve, neededFromELFList(folderELFs)...)

	// Installed exe: exe/<name>/pkg/<mainRel>. External resource trees
	// (outside the folder) are mapped against its directory; trees already
	// inside the folder ship with the copy.
	exeRel := filepath.Join("pkg", mainRel)
	type stagedRes struct{ host, rel string }
	var staged []stagedRes
	var extHosts []string
	resDirs, _ := appendSiblingResources(detectResourceDirs(absMain, name), absMain)
	for _, rd := range resDirs {
		if withinDir(rd.host, absDir) {
			continue
		}
		dest, ok := stageResourceDest(exeRel, rd)
		if !ok {
			opts.printf("  skip resources %s: escapes the exe payload\n", rd.host)
			continue
		}
		opts.printf("App resources: %s -> %s\n", rd.host, filepath.Join("exe", name, dest))
		staged = append(staged, stagedRes{host: rd.host, rel: dest})
		extHosts = append(extHosts, rd.host)
		toResolve = append(toResolve, neededFromTree(rd.host)...)
	}
	// The folder payload itself is authoritative for the sonames it ships.
	localRoots := append([]string{absDir}, extHosts...)
	local := buildLocalLibs(append(localRoots, originLibDirs(absMain)...))

	if opts.DryRun {
		opts.printf("Dry run: would copy folder %s to exe/%s/pkg/\n", absDir, name)
		opts.printf("Dry run: would patch %d bundled ELF(s)\n", len(folderELFs))
		if len(staged) > 0 {
			opts.printf("Dry run: would vendor %d resource dir(s)\n", len(staged))
		}
		return dryRun([]string{absMain}, toResolve, local, opts)
	}

	stg, err := stagePair()
	if err != nil {
		return err
	}
	defer stg.cleanup()
	stageApp, stageExe := stg.app, stg.exe

	opts.printf("Copying folder...\n")
	if err := copyTree(absDir, filepath.Join(stageExe, "pkg")); err != nil {
		return fmt.Errorf("copy folder: %w", err)
	}

	libsDir := filepath.Join(stageApp, "libs")
	if err := os.MkdirAll(libsDir, 0755); err != nil {
		return err
	}

	// Patch every ELF in the staged copy so each one resolves its deps
	// from the private libs dir — not just the main entrypoint. A script
	// main is left alone (no RPATH to patch); its ELF children still go local.
	// Foreign-arch helpers skipped during resolution are skipped here too.
	stagedRoot := filepath.Join(stageExe, "pkg")
	var stagedELFs []string
	for _, p := range listFolderELFs(stagedRoot) {
		if skipSet[relTo(stagedRoot, p)] {
			continue
		}
		stagedELFs = append(stagedELFs, p)
	}
	stagedMain := filepath.Join(stageExe, exeRel)

	installedLibs, _, usedPkgs, err := resolveStageCompat(stagedELFs, toResolve, libsDir, local, opts)
	if err != nil {
		return err
	}

	libsAbs := filepath.Join(config.AppsDir, name, "libs")
	opts.printf("Patching RPATH (%d bundled ELF(s))...\n", len(stagedELFs))
	mainPatched := false
	patched := 0
	for _, p := range stagedELFs {
		if isStaticELF(p) {
			continue // statically linked: loads nothing, patchelf N/A
		}
		if sameFile(p, stagedMain) {
			if err := patchBinaryRPATH(p, libsAbs); err != nil {
				return err
			}
			mainPatched = true
			patched++
			continue
		}
		if err := patchFolderELF(p, libsAbs); err != nil {
			return fmt.Errorf("patchelf %s: %w", relTo(stagedRoot, p), err)
		}
		patched++
	}
	if patched == 0 {
		opts.printf("  (all bundled ELFs statically linked; nothing to patch)\n")
	}
	if !mainPatched && !mainIsScript {
		// Main ELF missing from the scan (e.g. it was a symlink): patch it directly.
		opts.printf("Patching RPATH...\n")
		if err := patchBinaryRPATH(stagedMain, libsAbs); err != nil {
			return err
		}
	}

	for _, sr := range staged {
		opts.printf("Vendoring resources %s...\n", sr.host)
		if err := copyTree(sr.host, filepath.Join(stageExe, sr.rel)); err != nil {
			return fmt.Errorf("vendor resources %s: %w", sr.host, err)
		}
	}

	installedLibs = dedup(installedLibs)
	src := absDir
	if opts.ManifestSource != "" {
		src = opts.ManifestSource
	}
	m := &manifest.Manifest{
		Name:         name,
		Binary:       name,
		Libs:         installedLibs,
		Packages:     usedPkgs,
		Package:      opts.ManifestPackage,
		Source:       src,
		SourceSHA256: srcHash,
		PlacedBinary: true,
		ExeRel:       exeRel,
	}
	if err := manifest.Save(stageApp, m); err != nil {
		return err
	}

	return finishInstall(stg, name, m.ExeRel, opts)
}

// resolveMainExe finds the folder's main executable (ELF or shebang script).
// Explicit matches win:
// <dir>/<name>, <dir>/<dirbase>, <dir>/bin/<name>, <dir>/bin/<dirbase>
// (upstream bundles like kitty nest binaries under bin/). Otherwise the
// single top-level or bin/ executable is used when unambiguous.
//
// Scripts win over ELFs among explicit matches: when a bundle ships both
// e.g. <dir>/codium (backend ELF) and <dir>/bin/codium (launcher script),
// the script is the entrypoint that sets up argv/env and sibling paths.
func resolveMainExe(absDir, name string) (string, error) {
	tryKind := func(rel string, wantScript bool) (string, bool) {
		p := filepath.Join(absDir, rel)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			return "", false
		}
		isELF := elf.IsELF(p)
		isScript := !isELF && elf.IsScript(p)
		if !isELF && !isScript {
			return "", false
		}
		if isScript != wantScript {
			return "", false
		}
		return rel, true
	}
	base := filepath.Base(absDir)
	candidates := []string{name, base, filepath.Join("bin", name), filepath.Join("bin", base)}
	// Pass 1: launcher scripts; pass 2: ELF binaries.
	for _, wantScript := range []bool{true, false} {
		for _, rel := range candidates {
			if rel == "" {
				continue
			}
			if got, ok := tryKind(rel, wantScript); ok {
				return got, nil
			}
		}
	}
	try := func(rel string) (string, bool) {
		if got, ok := tryKind(rel, true); ok {
			return got, true
		}
		return tryKind(rel, false)
	}
	var cands []string
	for _, sub := range []string{".", "bin"} {
		dir := absDir
		if sub != "." {
			dir = filepath.Join(absDir, sub)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			rel := e.Name()
			if sub != "." {
				rel = filepath.Join(sub, e.Name())
			}
			if _, ok := try(rel); ok {
				cands = append(cands, rel)
			}
		}
	}
	switch len(cands) {
	case 0:
		return "", fmt.Errorf("%s: no executable found (expected %s/%s or %s/bin/%s)", absDir, absDir, name, absDir, name)
	case 1:
		return cands[0], nil
	default:
		return "", fmt.Errorf("%s: multiple executables (%s); pass [name] to select", absDir, strings.Join(cands, ", "))
	}
}

// resolveExplicitExe validates a pinned entrypoint: a path relative to the
// payload root pointing at an ELF binary or shebang script. It must stay
// inside the payload (no absolute paths, no ".." escapes).
func resolveExplicitExe(absDir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("entrypoint must be relative to the payload, got %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entrypoint %q escapes the payload", rel)
	}
	p := filepath.Join(absDir, clean)
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("entrypoint %q: %w", rel, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("entrypoint %q is a directory", rel)
	}
	if !elf.IsELF(p) && !elf.IsScript(p) {
		return "", fmt.Errorf("entrypoint %q is not an executable (ELF or shebang script)", rel)
	}
	return clean, nil
}

// stageResourceDest maps a resource dir to its exe-payload-relative
// destination, resolved against the installed binary's directory the same
// way the runtime lookup resolves (e.g. installed "bin/app" + ../lib/x ->
// "lib/x"). It reports false when the reference escapes the payload.
func stageResourceDest(installedRel string, rd resourceDir) (string, bool) {
	parts := strings.Split(filepath.Join(filepath.Dir(installedRel), "..", rd.cls, rd.first), string(filepath.Separator))
	var stack []string
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "", false
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, p)
		}
	}
	if len(stack) == 0 {
		return "", false
	}
	return filepath.Join(stack...), true
}

// withinDir reports whether p lies inside dir.
func withinDir(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// buildLocalLibs indexes ELF libraries shipped next to the source: the
// bundle is self-consistent (upstream kitty ships a patched libpython with
// private symbols the repo build lacks), so a soname found here wins over a
// repo download. Keys are basenames and SONAMEs; first root wins.
func buildLocalLibs(roots []string) map[string]string {
	out := map[string]string{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		count := 0
		filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || count > 2000 {
				return nil
			}
			if !strings.Contains(d.Name(), ".so") {
				return nil
			}
			if !hasELFMagic(path) {
				return nil
			}
			count++
			if _, ok := out[d.Name()]; !ok {
				out[d.Name()] = path
			}
			if info, err := elf.Parse(path); err == nil && info.SONAME != "" {
				if _, ok := out[info.SONAME]; !ok {
					out[info.SONAME] = path
				}
			}
			return nil
		})
	}
	return out
}

// originLibDirs expands the binary's own $ORIGIN RPATH/RUNPATH entries to
// host directories (e.g. upstream kitty's $ORIGIN/../lib -> <bundle>/lib).
func originLibDirs(absExe string) []string {
	info, err := elf.Parse(absExe)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	exeDir := filepath.Dir(absExe)
	for _, rp := range []string{info.RPATH, info.RUNPATH} {
		for _, e := range strings.Split(rp, ":") {
			if !strings.HasPrefix(e, "$ORIGIN") {
				continue
			}
			dir := filepath.Join(exeDir, e[len("$ORIGIN"):])
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				out = append(out, dir)
			}
		}
	}
	return out
}

// stageLocalLib stages a bundled .so into libsDir under its soname: content
// goes through the content-addressed store like repo libs (RPATH-carrying
// files stay private), so the loader finds the exact build the app shipped.
func stageLocalLib(soname, src, libsDir string) (names, private []string, err error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, nil, err
	}
	real := src
	if t, err := filepath.EvalSymlinks(src); err == nil {
		real = t
	}
	mode := os.FileMode(0755)
	if fi, err := os.Stat(real); err == nil {
		mode = fi.Mode()
	}
	privateFile := false
	if info, err := elf.Parse(real); err == nil && (info.RPATH != "" || info.RUNPATH != "") {
		privateFile = true
	}
	dst := filepath.Join(libsDir, soname)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return nil, nil, err
	}
	if privateFile {
		if err := os.WriteFile(dst, data, mode.Perm()&0777); err != nil {
			return nil, nil, err
		}
		return []string{soname}, []string{soname}, nil
	}
	storePath, err := store.StoreWithMode(data, mode)
	if err != nil {
		return nil, nil, err
	}
	store.Symlink(storePath, dst)
	return []string{soname}, nil, nil
}

// finishInstall atomically publishes both stage dirs and links the binary
// into BinDir. On any failure the previous install is restored.
func finishInstall(stg *stager, name, exeRel string, opts InstallOptions) error {
	target := filepath.Join(config.ExeDir, name, exeRel)
	if err := publishAll(stg.app, stg.exe, name, opts.Force); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			stg.commit()
		} else {
			rollbackAppDirs(name)
		}
	}()

	if err := placeSymlink(target, name, opts.Force); err != nil {
		return err
	}
	commitAppDirs(name)
	commitPlacedBinary(name)
	committed = true

	hintPath()
	opts.printf("\nDone. Run: %s\n", filepath.Join(config.BinDir, name))
	return nil
}

// archiveScope carries the dated ALA fallback state for one install run: the
// target snapshot date plus the lazily loaded archive index. ELF files record
// no package versions, so the snapshot date (from --archive-date or the app
// binary's mtime) is the version proxy: sonames missing from the live repos
// resolve against the repo .db as of that date instead of latest.
type archiveScope struct {
	enabled bool
	date    time.Time
	idx     *repo.Index
	used    string // snapshot actually loaded (YYYY-MM-DD, after backtrack)
	// warned records sonames already reported as archive-resolved, to keep
	// output readable when many transitive deps fall back together.
	warned map[string]bool
	// explicit is set when --archive-date was passed: tests override Default
	// with httptest indexes, and auto-mode archive lookups would hit the
	// real network — skip those, but honor explicit dates (tests for the
	// archive path itself set one).
	explicit bool
}

func newArchiveScope(roots []string, opts InstallOptions) *archiveScope {
	s := &archiveScope{warned: map[string]bool{}}
	if opts.NoArchive {
		return s
	}
	if opts.ArchiveDate != "" {
		d, err := repo.ParseArchiveDate(opts.ArchiveDate)
		if err != nil {
			opts.printf("warning: %v; archive fallback disabled\n", err)
			return s
		}
		s.enabled, s.date = true, d
		s.explicit = true
		return s
	}
	// Auto mode: snapshot date = main root ELF's mtime (build-date proxy).
	// Disabled when roots carry no usable mtime would just mean "now",
	// which duplicates the live index — still useful as a fallback source
	// for sonames the live mirrors already pruned.
	s.enabled, s.date = true, repo.ArchiveDateFromRoots(roots)
	return s
}

// providersFor returns the live providers when non-empty, else the archived
// snapshot's providers (loading the snapshot once). The returned index is the
// one to download from so package bytes match the resolving snapshot.
func (s *archiveScope) providersFor(soname string) ([]repo.PkgInfo, *repo.Index, string) {
	live := repo.Default().Providers(soname)
	if len(live) > 0 || !s.enabled {
		return live, nil, ""
	}
	// When tests override Default with an httptest index, ArchiveIndex would
	// hit the real network — skip it unless an explicit --archive-date opts
	// in (unit tests below cover archive.go directly against httptest).
	if repo.IsTestOverride() && !s.explicit && s.used == "" && s.idx == nil {
		return live, nil, ""
	}
	if s.idx == nil && s.used == "" {
		idx, used, err := repo.ArchiveIndex(s.date)
		if err != nil {
			return live, nil, ""
		}
		s.idx, s.used = idx, used
	}
	if s.idx == nil {
		return live, nil, ""
	}
	arch := s.idx.Providers(soname)
	if len(arch) == 0 {
		return live, nil, ""
	}
	return arch, s.idx, s.used
}

// resolveAndStage BFS-resolves sonames, downloading packages in parallel
// per frontier and extracting sequentially for determinism. Sonames present
// in local (libraries shipped next to the source) are staged from disk and
// never downloaded: the bundle is self-consistent. localStaged reports which
// sonames came from the bundle, so callers can re-stage them from the repo
// when the bundled copy turns out to be incompatible.
func resolveAndStage(roots []string, toResolve []string, libsDir string, local map[string]string, opts InstallOptions) (installed, private []string, usedPkgs map[string]string, localStaged map[string]bool, err error) {
	var installedLibs []string
	var privateLibs []string
	usedPkgs = map[string]string{}
	localStaged = map[string]bool{}
	seen := map[string]bool{}
	var failures []string
	lockMismatches := 0
	freshPins := 0
	pkgCache := map[string][]byte{}
	var pkgMu sync.Mutex
	arch := newArchiveScope(roots, opts)
	// Locked reinstalls pin manifest versions: never reach into the archive
	// for a different version — refuse instead so the pin stays exact.
	if opts.Locked != nil {
		arch.enabled = false
	}

	if len(toResolve) > 0 {
		opts.printf("\nResolving dependencies (recursive)...\n")
	}
	queue := append([]string(nil), toResolve...)
	for len(queue) > 0 {
		// Frontier: all currently queued, unseen sonames.
		var frontier []string
		for _, s := range queue {
			if !seen[s] {
				seen[s] = true
				frontier = append(frontier, s)
			}
		}
		queue = nil
		if len(frontier) == 0 {
			break
		}
		// Provider lookup is cheap (in-memory index); downloads dominate,
		// so prefetch every frontier's first-choice package in parallel.
		var tasks []dlTask
		provLists := make([][]repo.PkgInfo, len(frontier))
		provIdx := make([]*repo.Index, len(frontier))
		provSnap := make([]string, len(frontier))
		provFresh := make([]bool, len(frontier))
		for i, soname := range frontier {
			providers, srcIdx, snap := arch.providersFor(soname)
			// Version lock: when none of the providers are in the locked
			// set, the soname is a dependency the manifest never pinned
			// (e.g. newly discovered via resource-tree scan on reinstall)
			// — resolve it fresh and pin the current version below.
			// Otherwise enforce the lock strictly so prefetch never grabs
			// a version reinstall must refuse.
			if opts.Locked != nil && lockInvolved(providers, opts.Locked) {
				var kept []repo.PkgInfo
				for _, p := range providers {
					if want, ok := opts.Locked[p.Repo+"/"+p.Name]; ok && want == p.Version {
						kept = append(kept, p)
					}
				}
				// Keep original list for error reporting; prefetch none.
				if len(kept) == 0 && len(providers) > 0 {
					provLists[i] = providers
					continue
				}
				providers = kept
			} else if opts.Locked != nil && len(providers) > 0 {
				provFresh[i] = true
			}
			provLists[i] = providers
			provIdx[i] = srcIdx
			provSnap[i] = snap
			if _, ok := local[soname]; ok {
				continue // staged from disk below; nothing to prefetch
			}
			if len(providers) == 0 {
				continue
			}
			key := providers[0].Name + "-" + providers[0].Version + "-" + providers[0].Arch
			pkgMu.Lock()
			_, cached := pkgCache[key]
			pkgMu.Unlock()
			if !cached {
				tasks = append(tasks, dlTask{soname: soname, pkg: providers[0], key: key, idx: srcIdx})
			}
		}
		parallelDownload(tasks, pkgCache, &pkgMu, opts)

		for fi, soname := range frontier {
			var (
				names, privates []string
				staged          bool
				lastErr         error
			)
			// Bundled copies first: the app shipped them, so they match
			// by construction. Repo providers below are the fallback.
			if lp, ok := local[soname]; ok {
				var lerr error
				names, privates, lerr = stageLocalLib(soname, lp, libsDir)
				if lerr == nil {
					opts.printf("  [local] %s -> %s\n", soname, lp)
					staged = true
					localStaged[soname] = true
				} else {
					opts.printf("  [warn] %s: bundled copy unusable (%v); trying repo\n", soname, lerr)
				}
			}
			if !staged {
				providers := provLists[fi]
				srcIdx := provIdx[fi]
				snap := provSnap[fi]
				downloadFrom := func(pkg repo.PkgInfo) ([]byte, error) {
					if srcIdx != nil {
						return srcIdx.Download(pkg)
					}
					return repo.Default().Download(pkg)
				}
				if len(providers) == 0 {
					// Recompute unfiltered list for a precise error.
					raw := repo.Default().Providers(soname)
					if len(raw) == 0 {
						msg := fmt.Sprintf("%s: no package provides %s", soname, soname)
						if arch.enabled && arch.used == "" {
							msg += " (live and archive have no provider)"
						} else if arch.enabled && arch.used != "" {
							msg += fmt.Sprintf(" (live and archive %s have no provider)", arch.used)
						}
						opts.printf("  [fail] %s\n", msg)
						failures = append(failures, msg)
					} else {
						msg := fmt.Sprintf("%s: locked version unavailable (index has %s; run upgrade to re-resolve)", soname, raw[0].Version)
						opts.printf("  [fail] %s\n", msg)
						failures = append(failures, msg)
						lockMismatches++
					}
					continue
				}
				for _, pkg := range providers {
					if opts.Locked != nil && !provFresh[fi] {
						key := pkg.Repo + "/" + pkg.Name
						want, ok := opts.Locked[key]
						if !ok {
							lastErr = fmt.Errorf("%s is not in the locked package set (run upgrade to re-resolve)", key)
							lockMismatches++
							continue
						}
						if want != pkg.Version {
							lastErr = fmt.Errorf("locked version %s unavailable (index has %s; run upgrade to re-resolve)", want, pkg.Version)
							lockMismatches++
							continue
						}
					}

					pkgKey := pkg.Name + "-" + pkg.Version + "-" + pkg.Arch
					pkgMu.Lock()
					data, ok := pkgCache[pkgKey]
					pkgMu.Unlock()
					if !ok {
						var derr error
						data, derr = downloadFrom(pkg)
						if derr != nil {
							lastErr = fmt.Errorf("download %s: %w", pkg.Name, derr)
							continue
						}
						pkgMu.Lock()
						pkgCache[pkgKey] = data
						pkgMu.Unlock()
					}

					n, p, eerr := extractLibs(pkg, data, soname, libsDir)
					if eerr != nil {
						lastErr = eerr
						continue
					}

					names, privates = n, p
					usedPkgs[pkg.Repo+"/"+pkg.Name] = pkg.Version
					if provFresh[fi] {
						freshPins++
						opts.printf("  [ ok ] %s -> %s/%s %s (new pin)\n", soname, pkg.Repo, pkg.Name, pkg.Version)
					} else if snap != "" {
						opts.printf("  [archive %s] %s -> %s/%s %s\n", snap, soname, pkg.Repo, pkg.Name, pkg.Version)
					} else {
						opts.printf("  [ ok ] %s -> %s/%s %s\n", soname, pkg.Repo, pkg.Name, pkg.Version)
					}
					staged = true
					break
				}
			}
			if !staged {
				if lastErr == nil {
					lastErr = fmt.Errorf("no usable provider")
				}
				msg := fmt.Sprintf("%s: %v", soname, lastErr)
				opts.printf("  [fail] %s\n", msg)
				failures = append(failures, msg)
				continue
			}
			installedLibs = append(installedLibs, names...)
			privateLibs = append(privateLibs, privates...)

			depInfo, err := elf.Parse(filepath.Join(libsDir, soname))
			if err == nil {
				for _, n := range depInfo.NEEDED {
					if !resolver.Skip(n) && !seen[n] {
						queue = append(queue, n)
					}
				}
			}
		}
	}

	if len(failures) > 0 {
		if lockMismatches > 0 && lockMismatches == len(failures) {
			return nil, nil, nil, nil, fmt.Errorf("cannot reinstall: locked versions gone from the repos (mirror pruned them):\n  %s\nrun upgrade to re-resolve to current versions", strings.Join(failures, "\n  "))
		}
		return nil, nil, nil, nil, fmt.Errorf("cannot install: unsatisfied dependencies:\n  %s", strings.Join(failures, "\n  "))
	}
	if freshPins > 0 {
		opts.printf("  note: pinned %d new package(s) not in the previous manifest\n", freshPins)
	}
	return installedLibs, privateLibs, usedPkgs, localStaged, nil
}

type dlTask struct {
	soname string
	pkg    repo.PkgInfo
	key    string
	idx    *repo.Index // nil = live default; non-nil = archive snapshot
}

// resolveStageCompat resolves and stages like resolveAndStage, then verifies
// dependency compatibility from roots. A bundled library that breaks symbol
// versioning for a repo library (e.g. kitty's libncursesw vs repo
// libpanelw) is re-staged from the repo instead: such mixed closures crash
// at runtime, so repo consistency wins over the bundle for that soname.
// Retries are bounded; problems that no fallback heals fail as before.
func resolveStageCompat(roots []string, toResolve []string, libsDir string, local map[string]string, opts InstallOptions) (installed, private []string, usedPkgs map[string]string, err error) {
	usedPkgs = map[string]string{}
	forced := map[string]bool{}
	const maxRounds = 4
	for round := 0; ; round++ {
		active := local
		if len(forced) > 0 {
			active = map[string]string{}
			for k, v := range local {
				if !forced[k] {
					active[k] = v
				}
			}
		}
		ins, priv, used, localStaged, rerr := resolveAndStage(roots, toResolve, libsDir, active, opts)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		installed = append(installed, ins...)
		private = append(private, priv...)
		for k, v := range used {
			usedPkgs[k] = v
		}
		if err := patchPrivateLibs(priv, libsDir); err != nil {
			return nil, nil, nil, err
		}
		opts.printf("\nChecking dependency compatibility...\n")
		problems := checkCompatDetailed(roots, libsDir)
		if len(problems) == 0 {
			return dedup(installed), dedup(private), usedPkgs, nil
		}
		var heal []string
		seenHeal := map[string]bool{}
		for _, p := range problems {
			if p.Needed == "" || p.Provider == "" {
				continue // unparseable or missing: no fallback exists
			}
			if !localStaged[p.Needed] || forced[p.Needed] || seenHeal[p.Needed] {
				continue
			}
			// Healable when the live index provides it, or when the archive
			// fallback is enabled (resolveAndStage will try the snapshot).
			if len(repo.Default().Providers(p.Needed)) == 0 && (opts.NoArchive || opts.Locked != nil) {
				continue
			}
			seenHeal[p.Needed] = true
			heal = append(heal, p.Needed)
		}
		if len(heal) == 0 || round+1 >= maxRounds {
			msgs := make([]string, len(problems))
			for i, pr := range problems {
				msgs[i] = pr.Error()
			}
			return nil, nil, nil, fmt.Errorf("dependency compatibility:\n  %s", strings.Join(msgs, "\n  "))
		}
		for _, s := range heal {
			forced[s] = true
			os.RemoveAll(filepath.Join(libsDir, s))
			opts.printf("  [repo-fallback] %s: bundled copy incompatible, using repo build\n", s)
		}
	}
}

// lockInvolved reports whether any provider's package is mentioned in the
// version lock. If none is, the soname is new to the lock (never pinned)
// and may be resolved fresh.
func lockInvolved(providers []repo.PkgInfo, locked map[string]string) bool {
	for _, p := range providers {
		if _, ok := locked[p.Repo+"/"+p.Name]; ok {
			return true
		}
	}
	return false
}

func parallelDownload(tasks []dlTask, cache map[string][]byte, mu *sync.Mutex, opts InstallOptions) {
	if len(tasks) == 0 {
		return
	}
	const workers = 8
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func(t dlTask) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			_, ok := cache[t.key]
			mu.Unlock()
			if ok {
				return
			}
			data, err := func() ([]byte, error) {
				if t.idx != nil {
					return t.idx.Download(t.pkg)
				}
				return repo.Default().Download(t.pkg)
			}()
			if err != nil {
				return // sequential pass reports the error
			}
			mu.Lock()
			if _, ok := cache[t.key]; !ok {
				cache[t.key] = data
			}
			mu.Unlock()
		}(t)
	}
	wg.Wait()
}

func dryRun(roots []string, toResolve []string, local map[string]string, opts InstallOptions) error {
	opts.printf("Dry run: would resolve %d top-level libs\n", len(toResolve))
	arch := newArchiveScope(roots, opts)
	if opts.Locked != nil {
		arch.enabled = false
	}
	for _, soname := range toResolve {
		if lp, ok := local[soname]; ok {
			opts.printf("  [local] %s -> %s\n", soname, lp)
			continue
		}
		providers, _, snap := arch.providersFor(soname)
		if len(providers) == 0 {
			opts.printf("  [fail] %s: no package provides %s\n", soname, soname)
			continue
		}
		p := providers[0]
		if snap != "" {
			opts.printf("  [archive %s] %s -> %s/%s %s\n", snap, soname, p.Repo, p.Name, p.Version)
			continue
		}
		opts.printf("  [ ok ] %s -> %s/%s %s\n", soname, p.Repo, p.Name, p.Version)
	}
	opts.printf("Dry run: no changes made\n")
	return nil
}

func fileSHA256Hex(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func Reinstall(name string) error {
	return ReinstallWithOptions(name, InstallOptions{Force: true})
}

func ReinstallWithOptions(name string, opts InstallOptions) error {
	m, err := manifest.Load(filepath.Join(config.AppsDir, name))
	if err != nil {
		return fmt.Errorf("app %q not installed: %w", name, err)
	}
	pkg, ok := splitRepoSource(m.Source)
	if !ok {
		return fmt.Errorf("%s: manifest has no repo source (installed by an older version); use install instead", name)
	}
	opts.Force = true
	if opts.Locked == nil {
		opts.Locked = m.Packages
	}
	// Re-fetch the pinned package version.
	_, version, pok := splitPinnedPackage(m.Package)
	if !pok {
		return fmt.Errorf("%s: manifest has a repo source but no pinned package version; use install instead", name)
	}
	return InstallRepoApp(pkg, version, name, opts)
}

func Upgrade(name string) error {
	return UpgradeWithOptions(name, InstallOptions{Force: true})
}

func UpgradeWithOptions(name string, opts InstallOptions) error {
	m, err := manifest.Load(filepath.Join(config.AppsDir, name))
	if err != nil {
		return fmt.Errorf("app %q not installed: %w", name, err)
	}
	pkg, ok := splitRepoSource(m.Source)
	if !ok {
		return fmt.Errorf("%s: manifest has no repo source (installed by an older version); use install instead", name)
	}
	opts.Force = true
	opts.Locked = nil
	// Re-resolve to the live latest.
	return InstallRepoApp(pkg, "", name, opts)
}

// pruneStaleStages removes staging dirs orphaned by killed installs.
// Callers must hold the cross-process lock.
func pruneStaleStages() {
	for _, dir := range []string{config.AppsDir, config.ExeDir} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), ".stage-") {
				continue
			}
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// publishAll swaps both stage dirs into place: stageExe -> exe/<name> and
// stageApp -> apps/<name>. Previous installs (if any) are kept as .bak until
// commitAppDirs, so a crash or --force failure never destroys the working
// app. Force handling is checked by prepareInstall; both renames proceed.
func publishAll(stageApp, stageExe, name string, force bool) error {
	if err := publishOne(stageExe, filepath.Join(config.ExeDir, name), force); err != nil {
		return err
	}
	if err := publishOne(stageApp, filepath.Join(config.AppsDir, name), force); err != nil {
		rollbackAppDirs(name)
		return err
	}
	return nil
}

func publishOne(stage, final string, force bool) error {
	if _, err := os.Stat(final); err == nil {
		if !force {
			return fmt.Errorf("%s already exists (use --force to overwrite)", final)
		}
		bak := final + ".bak"
		os.RemoveAll(bak)
		if err := os.Rename(final, bak); err != nil {
			return fmt.Errorf("backup previous install: %w", err)
		}
	}
	if err := os.Rename(stage, final); err != nil {
		rollbackOne(final)
		return err
	}
	return nil
}

func commitAppDirs(name string) {
	os.RemoveAll(filepath.Join(config.AppsDir, name+".bak"))
	os.RemoveAll(filepath.Join(config.ExeDir, name+".bak"))
}

func rollbackAppDirs(name string) {
	rollbackOne(filepath.Join(config.AppsDir, name))
	rollbackOne(filepath.Join(config.ExeDir, name))
}

func rollbackOne(final string) {
	bak := final + ".bak"
	// If the new final exists and a backup exists, the rename succeeded:
	// drop the half-published tree and restore the backup.
	if _, err := os.Stat(final); err == nil {
		if _, berr := os.Stat(bak); berr == nil {
			os.RemoveAll(final)
			os.Rename(bak, final)
			return
		}
	}
	// Rename never happened: just restore a stranded backup (if any).
	if _, berr := os.Stat(bak); berr == nil {
		if _, err := os.Stat(final); os.IsNotExist(err) {
			os.Rename(bak, final)
		}
	}
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.Contains(name, string(filepath.Separator)) {
		return fmt.Errorf("invalid app name %q", name)
	}
	return nil
}

// resourceDir maps a host resource tree to its destination under the exe
// payload root. The binary finds these at runtime via exe-relative lookups
// such as ../lib/<app>, so they are replicated next to the installed copy
// and the real binary lives under ~/.pap/exe/<name> (symlinked from
// ~/.local/bin, through which both absolute-RPATH lib lookup and
// exe-relative resource lookup keep working).
type resourceDir struct {
	host  string // absolute host path (existing dir)
	cls   string // "lib" or "share"
	first string // top-level dir name, e.g. "kitty"
}

// rel is the exe-payload-relative destination, e.g. "lib/kitty".
func (r resourceDir) rel() string { return filepath.Join(r.cls, r.first) }

// detectResourceDirs finds host resource trees the binary resolves relative
// to its own location. Two sources: string references to ../lib/ or
// ../share/ embedded in the binary, plus the conventional ../lib/<name>
// sibling (covers binaries whose refs are constructed at runtime).
func detectResourceDirs(exeAbs, name string) []resourceDir {
	exeDir := filepath.Dir(exeAbs)
	seen := map[string]bool{}
	var out []resourceDir
	add := func(host, cls, first string) {
		host = filepath.Clean(host)
		if seen[host] {
			return
		}
		st, err := os.Stat(host)
		if err != nil || !st.IsDir() {
			return
		}
		seen[host] = true
		out = append(out, resourceDir{host: host, cls: cls, first: first})
	}
	for _, ref := range exeRelativeRefs(exeAbs) {
		parts := strings.Split(ref, "/")
		if len(parts) < 3 || parts[0] != ".." || parts[1] != "lib" && parts[1] != "share" {
			continue
		}
		add(filepath.Join(exeDir, "..", parts[1], parts[2]), parts[1], parts[2])
	}
	base := strings.TrimSuffix(filepath.Base(exeAbs), filepath.Ext(filepath.Base(exeAbs)))
	for _, cand := range []string{name, base} {
		if cand == "" {
			continue
		}
		add(filepath.Join(exeDir, "..", "lib", cand), "lib", cand)
	}
	return out
}

// appendSiblingResources extends directly-detected resource dirs with
// runtime-composed sibling trees, matching names against corpus: the main
// binary plus already-detected resource contents. Paths like
// `<exe>/../lib/kitty-extensions` are built at runtime from
// `%s/%s/kitty-extensions` (or inside frozen Python), so no single
// `../lib/...` string exists to find in the ELF. System library dirs are
// excluded: their hundreds of unrelated siblings belong to other packages.
// It returns the extended dirs and the reference corpus.
func appendSiblingResources(direct []resourceDir, exeAbs string) ([]resourceDir, string) {
	var hosts []string
	for _, rd := range direct {
		hosts = append(hosts, rd.host)
	}
	corpus := referenceCorpus(exeAbs, hosts)
	seen := map[string]bool{}
	for _, rd := range direct {
		seen[rd.host] = true
	}
	out := append([]resourceDir(nil), direct...)
	for _, rd := range direct {
		parent := filepath.Dir(rd.host)
		if !siblingScanAllowed(parent) {
			continue
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || len(e.Name()) < 2 {
				continue
			}
			p := filepath.Join(parent, e.Name())
			if seen[p] {
				continue
			}
			if !mentionsName(corpus, e.Name()) {
				continue
			}
			seen[p] = true
			out = append(out, resourceDir{host: p, cls: rd.cls, first: e.Name()})
		}
	}
	return out, corpus
}

// referenceCorpus concatenates the binary with the contents of detected
// resource trees (bounded), so runtime-built path components frozen into
// Python blobs or data files can still be matched.
func referenceCorpus(exeAbs string, resHosts []string) string {
	var sb strings.Builder
	if data, err := os.ReadFile(exeAbs); err == nil && len(data) <= 64<<20 {
		sb.Write(data)
	}
	const maxTotal = 64 << 20
	for _, h := range resHosts {
		if sb.Len() >= maxTotal {
			break
		}
		filepath.WalkDir(h, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || sb.Len() >= maxTotal {
				return nil
			}
			if st, err := d.Info(); err != nil || st.Size() > 16<<20 {
				return nil
			}
			if data, err := os.ReadFile(p); err == nil && len(data) <= 16<<20 {
				sb.Write(data)
			}
			return nil
		})
	}
	return sb.String()
}

// mentionsName reports whether name occurs as a whole path component in
// corpus (bounded by non-filename chars), avoiding substring hits like
// "kitten" inside "kittens".
func mentionsName(corpus, name string) bool {
	if name == "" {
		return false
	}
	idx := 0
	for {
		i := strings.Index(corpus[idx:], name)
		if i < 0 {
			return false
		}
		s := idx + i
		e := s + len(name)
		if !isNameChar(byteAt(corpus, s-1)) && !isNameChar(byteAt(corpus, e)) {
			return true
		}
		idx = e
	}
}

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func isNameChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '_' || b == '-' || b == '.' || b == '+'
}

// neededFromFile collects a single ELF's DT_NEEDED (non-host) entries.
func neededFromFile(path string) []string {
	info, err := elf.Parse(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range info.NEEDED {
		if !resolver.Skip(n) {
			out = append(out, n)
		}
	}
	return out
}

// siblingScanAllowed reports whether sibling vendoring may look inside
// parent: never inside system library dirs.
func siblingScanAllowed(parent string) bool {
	switch filepath.Clean(parent) {
	case "/usr/lib", "/usr/lib64", "/lib", "/lib64",
		"/usr/share", "/usr/local/lib", "/usr/local/lib64", "/usr/local/share":
		return false
	}
	return true
}

// exeContent returns the binary as a string for substring scans (capped).
func exeContent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64<<20 {
		return ""
	}
	return string(data)
}

// exeRelativeRefs scans the binary for embedded ../lib/ and ../share/
// references (e.g. kitty's "../lib/kitty").
func exeRelativeRefs(path string) []string {
	s := exeContent(path)
	if s == "" {
		return nil
	}
	data := []byte(s)
	seen := map[string]bool{}
	var out []string
	for _, prefix := range []string{"../lib/", "../share/"} {
		idx := 0
		for {
			i := strings.Index(s[idx:], prefix)
			if i < 0 || len(out) >= 32 {
				break
			}
			start := idx + i
			end := start
			for end < len(data) && isRefByte(data[end]) {
				end++
			}
			ref := string(data[start:end])
			idx = end
			if seen[ref] || len(ref) > 128 {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}

func isRefByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '_' || b == '-' || b == '.' || b == '/' || b == '+'
}

// listFolderELFs returns every ELF file under root, recursively. Symlinks
// are skipped: the link target itself is visited (when inside the tree),
// so each inode is patched exactly once and external links are untouched.
func listFolderELFs(root string) []string {
	var out []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !hasELFMagic(path) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out
}

// neededFromELFList collects DT_NEEDED (non-host) entries from an explicit
// ELF list, deduplicated. Unlike neededFromTree it does not filter by
// filename: folder payloads may ship executables without ".so" or "bin/"
// in their names.
func neededFromELFList(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		for _, n := range neededFromFile(p) {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// patchFolderELF rewrites one bundled ELF's RPATH to the app's libs dir,
// preserving any original entries. Unlike patchBinaryRPATH it never falls
// back to a shell wrapper: replacing a helper binary or .so with a script
// would corrupt the payload — a patchelf failure is a hard error instead.
func patchFolderELF(path, libsAbs string) error {
	orig, rerr := printRpath(path)
	if rerr != nil {
		return rerr
	}
	if strings.Contains(orig, libsAbs) {
		return nil
	}
	rpath := libsAbs
	if orig != "" {
		rpath = orig + ":" + libsAbs
	}
	return patchelfRPATH(path, rpath)
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func relTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

// neededFromTree collects DT_NEEDED entries of ELF objects under a host
// resource tree (e.g. kitty's python extensions), so their own dependencies
// join the BFS and get vendored too.
func neededFromTree(root string) []string {
	seen := map[string]bool{}
	var out []string
	count := 0
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || count > 500 {
			return nil
		}
		if !strings.Contains(d.Name(), ".so") && !strings.Contains(path, "bin/") {
			return nil
		}
		if !hasELFMagic(path) {
			return nil
		}
		count++
		info, err := elf.Parse(path)
		if err != nil {
			return nil
		}
		for _, n := range info.NEEDED {
			if !resolver.Skip(n) && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
		return nil
	})
	return out
}

// copyTree replicates src dir at dst, preserving symlinks, file modes and
// mtimes. The mtimes matter: ArchiveDateFromRoots derives the ALA snapshot
// date (the era the payload's dependencies must resolve against) from them,
// so a copy that reset them to "now" would pin every install to today.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, fi.Mode().Perm()&0777)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.Remove(target)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		mode := fi.Mode().Perm() & 0777
		if mode == 0 {
			mode = 0644
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
		if err := os.Chmod(target, mode); err != nil {
			return err
		}
		return os.Chtimes(target, fi.ModTime(), fi.ModTime())
	})
}

// placeSymlink links ~/.local/bin/<name> to the real binary under
// ~/.pap/exe/<name>. The kernel resolves the symlink for /proc/self/exe,
// so exe-relative resource lookups work exactly as if the binary lived on
// PATH — no launcher process, argv[0] untouched. An existing entry is kept
// as .bak until commitPlacedBinary, mirroring the publish backups.
//
// Shebang scripts are the exception: the kernel does not resolve $0
// through a symlink, so a script entrypoint that finds helpers via
// $(dirname $0) would look next to the symlink instead of the payload.
// Scripts get a tiny launcher that execs the real path, making $0 resolve
// to the payload (sibling ELFs are already patched, so the script stays local).
func placeSymlink(targetAbs, name string, force bool) error {
	if err := os.MkdirAll(config.BinDir, 0755); err != nil {
		return err
	}
	if _, err := os.Stat(targetAbs); err != nil {
		return fmt.Errorf("staged binary missing: %w", err)
	}
	dest := filepath.Join(config.BinDir, name)
	if _, err := os.Lstat(dest); err == nil {
		if !force {
			return fmt.Errorf("%s already exists (use --force to overwrite)", dest)
		}
		os.Remove(dest + ".bak")
		if err := os.Rename(dest, dest+".bak"); err != nil {
			return fmt.Errorf("backup %s: %w", dest, err)
		}
	}
	if elf.IsScript(targetAbs) && !elf.IsELF(targetAbs) {
		return placeScriptLauncher(targetAbs, dest, name)
	}
	tmp, err := os.CreateTemp(config.BinDir, ".link-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	os.Remove(tmpName)
	if err := os.Symlink(targetAbs, tmpName); err != nil {
		os.Remove(tmpName)
		rollbackPlacedBinary(name)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		rollbackPlacedBinary(name)
		return err
	}
	return nil
}

// placeScriptLauncher installs a BinDir entry for a script entrypoint: a
// small executable that execs the real script by absolute path. The inner
// script then sees $0 under ~/.pap/exe/<name>/, so $(dirname $0)-relative
// helper lookups land in the payload instead of the BinDir.
func placeScriptLauncher(targetAbs, dest, name string) error {
	body := "#!/bin/sh\nexec \"" + targetAbs + "\" \"$@\"\n"
	tmp, err := os.CreateTemp(config.BinDir, ".launcher-*")
	if err != nil {
		rollbackPlacedBinary(name)
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		rollbackPlacedBinary(name)
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmpName, 0755); err != nil {
		os.Remove(tmpName)
		rollbackPlacedBinary(name)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		rollbackPlacedBinary(name)
		return err
	}
	return nil
}

func Uninstall(name string) error {
	lh, err := lock.Acquire(config.LockFile)
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer lh.Release()

	appDir := filepath.Join(config.AppsDir, name)
	exeDir := filepath.Join(config.ExeDir, name)
	if _, err := os.Stat(appDir); os.IsNotExist(err) {
		if _, err := os.Stat(exeDir); os.IsNotExist(err) {
			return fmt.Errorf("app %s not found", name)
		}
	}

	m, merr := manifest.Load(filepath.Join(config.AppsDir, name))
	if merr != nil {
		fmt.Printf("Warning: no manifest found, removing app dirs only (leaving ~/.local/bin/%s alone)\n", name)
		os.RemoveAll(appDir)
		os.RemoveAll(exeDir)
		return nil
	}

	os.RemoveAll(appDir)
	os.RemoveAll(exeDir)
	// Only remove the binary we placed (manifest-gated): never delete a
	// user's own file when the manifest is missing.
	if m.PlacedBinary || m.Binary != "" {
		if removePlacedBinary(name) {
			fmt.Printf("  Removed %s\n", filepath.Join(config.BinDir, name))
		}
	}

	fmt.Printf("Uninstalled %s\n", name)
	if m != nil {
		fmt.Printf("  Removed %d libs from %s\n", len(m.Libs), name)
	}
	// Reclaim now-unreferenced store blobs (best-effort).
	if used := store.ScanUsed(); used != nil {
		if n, err := store.GC(used); err == nil && n > 0 {
			fmt.Printf("  GC: removed %d unused store entries\n", n)
		}
	}
	return nil
}

func GC() error {
	used := store.ScanUsed()
	removed, err := store.GC(used)
	if err != nil {
		return err
	}
	fmt.Printf("GC: removed %d unused entries\n", removed)
	return nil
}

func List() error {
	manifests, err := manifest.List(config.AppsDir)
	if err != nil {
		return err
	}

	if len(manifests) == 0 {
		fmt.Println("No apps installed.")
		return nil
	}

	fmt.Printf("%-20s %-30s %-8s %s\n", "NAME", "SOURCE", "LIBS", "VERSIONS")
	fmt.Println(strings.Repeat("-", 90))
	for _, m := range manifests {
		vers := "-"
		if len(m.Packages) > 0 {
			var parts []string
			for k, v := range m.Packages {
				parts = append(parts, filepath.Base(k)+" "+v)
				if len(parts) >= 2 {
					break
				}
			}
			vers = strings.Join(parts, ", ")
			if len(m.Packages) > 2 {
				vers += fmt.Sprintf(" (+%d more)", len(m.Packages)-2)
			}
		}
		fmt.Printf("%-20s %-30s %-8d %s\n", m.Name, manifestSource(m), len(m.Libs), vers)
	}
	return nil
}

// manifestSource renders a manifest's SOURCE column: where the app came from,
// plus the exact package version behind it when the install recorded one
// (manifest.Package is "<repo>/<name> <version>", set for every repo app and
// the record of a --pkg-version pin). The result is truncated to the column.
func manifestSource(m *manifest.Manifest) string {
	src := m.Source
	if src == "" {
		src = m.Binary
	}
	if _, ver, ok := splitPinnedPackage(m.Package); ok {
		src += " (" + ver + ")"
	}
	if len(src) > 28 {
		src = "…" + src[len(src)-27:]
	}
	return src
}

// Info prints one app's manifest, binary location and store usage.
func Info(name string) error {
	m, err := manifest.Load(filepath.Join(config.AppsDir, name))
	if err != nil {
		return fmt.Errorf("app %q not installed: %w", name, err)
	}
	fmt.Printf("name:    %s\n", m.Name)
	switch {
	case m.ExeRel != "":
		fmt.Printf("binary:  %s (symlink -> %s)\n", filepath.Join(config.BinDir, m.Binary), filepath.Join(config.ExeDir, m.Name, m.ExeRel))
	case m.Launcher:
		fmt.Printf("binary:  %s (legacy launcher -> %s)\n", filepath.Join(config.BinDir, m.Binary), filepath.Join(config.AppsDir, m.Name, "bin", m.Binary))
	default:
		fmt.Printf("binary:  %s\n", filepath.Join(config.BinDir, m.Binary))
	}
	fmt.Printf("source:  %s\n", m.Source)
	if m.SourceSHA256 != "" {
		fmt.Printf("src-sha: %.12s\n", m.SourceSHA256)
	}
	fmt.Printf("libs:    %d\n", len(m.Libs))
	for _, l := range m.Libs {
		fmt.Printf("  %s\n", l)
	}
	fmt.Printf("packages: %d pinned\n", len(m.Packages))
	for k, v := range m.Packages {
		fmt.Printf("  %s %s\n", k, v)
	}
	return nil
}

// extractLibs extracts every file for soname from pkg into libsDir, plus any
// plugin subdirectory ELF objects (ossl-modules, gconv, NSS, ...). Files whose
// own ELF declares RPATH/RUNPATH are copied privately (so patchelf can rewrite
// them); everything else is symlinked into the content-addressed store.
// Returns staged relative paths and the subset that needs patching.
func extractLibs(pkg repo.PkgInfo, pkgData []byte, soname, libsDir string) (names, private []string, err error) {
	tmpDir, err := os.MkdirTemp("", "pkg-dl-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmpDir)

	pkgFile := filepath.Join(tmpDir, "pkg.tar.zst")
	if err := os.WriteFile(pkgFile, pkgData, 0644); err != nil {
		return nil, nil, err
	}

	return extractLibsFromFile(pkg, pkgFile, soname, libsDir)
}

// extractLibsFromFile is the streaming variant: the package already lives
// on disk (DownloadToFile), so hundreds of MB never sit in a []byte.
func extractLibsFromFile(pkg repo.PkgInfo, pkgFile, soname, libsDir string) (names, private []string, err error) {
	tmpDir, err := os.MkdirTemp("", "pkg-ex-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmpDir)

	if err := verifyPkgFile(pkgFile, pkg); err != nil {
		return nil, nil, err
	}

	extract := exec.Command("bsdtar", "-xf", pkgFile, "-C", tmpDir, "usr/lib/")
	var extractOut bytes.Buffer
	extract.Stdout = &extractOut
	extract.Stderr = &extractOut
	if err := extract.Run(); err != nil {
		if s := strings.TrimSpace(extractOut.String()); s != "" {
			fmt.Printf("  [warn] bsdtar %s: %s\n", pkg.Name, s)
		}
		return nil, nil, fmt.Errorf("extract: %w", err)
	}

	libDir := filepath.Join(tmpDir, "usr", "lib")
	sonameBase := strings.TrimSuffix(soname, filepath.Ext(soname))

	seenRel := map[string]bool{}
	stage := func(rel string) {
		if seenRel[rel] {
			return
		}
		seenRel[rel] = true

		src := filepath.Join(libDir, rel)
		data, err := os.ReadFile(src)
		if err != nil {
			return
		}
		srcMode := os.FileMode(0755)
		if fi, err := os.Stat(src); err == nil {
			srcMode = fi.Mode()
		}

		privateFile := false
		if info, err := elf.Parse(src); err == nil && (info.RPATH != "" || info.RUNPATH != "") {
			privateFile = true
		}

		dst := filepath.Join(libsDir, rel)
		if privateFile {
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return
			}
			if err := os.WriteFile(dst, data, srcMode.Perm()&0777); err != nil {
				return
			}
			private = append(private, rel)
		} else {
			storePath, err := store.StoreWithMode(data, srcMode)
			if err != nil {
				return
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return
			}
			store.Symlink(storePath, dst)
		}
		names = append(names, rel)
	}

	filepath.WalkDir(libDir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(libDir, path)
		if err != nil {
			return nil
		}
		if !strings.Contains(d.Name(), ".so") {
			return nil
		}
		if strings.Contains(rel, string(filepath.Separator)) {
			// plugin subdir: only real ELF objects
			if hasELFMagic(path) {
				stage(rel)
			}
			return nil
		}
		if d.Name() == soname || strings.HasPrefix(d.Name(), sonameBase) {
			stage(rel)
		}
		return nil
	})

	if !seenRel[soname] {
		// Roll back anything this package staged (e.g. plugin dirs) so a
		// failed candidate doesn't leak files into the app.
		for _, rel := range names {
			os.RemoveAll(filepath.Join(libsDir, rel))
		}
		return nil, nil, fmt.Errorf("package %s/%s %s contains no %s", pkg.Repo, pkg.Name, pkg.Version, soname)
	}
	return names, private, nil
}

// isStaticELF reports whether path is a statically linked ELF (no
// .dynamic section): it loads no shared libraries, so there is nothing to
// isolate and patchelf cannot rewrite it. Such files are skipped, not failed.
func isStaticELF(path string) bool {
	return elf.IsStatic(path)
}

func hasELFMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [4]byte
	if _, err := f.Read(hdr[:]); err != nil {
		return false
	}
	return hdr == [4]byte{0x7f, 'E', 'L', 'F'}
}

func verifyPkgFile(pkgFile string, want repo.PkgInfo) error {
	out, err := exec.Command("bsdtar", "-xOf", pkgFile, ".PKGINFO").Output()
	if err != nil {
		return fmt.Errorf("read .PKGINFO: %w", err)
	}

	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		got[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}

	if got["pkgname"] != want.Name {
		return fmt.Errorf("version mismatch: expected pkg %s, got %s", want.Name, got["pkgname"])
	}
	if got["pkgver"] != want.Version {
		return fmt.Errorf("version mismatch: expected %s %s, got pkgver %s", want.Name, want.Version, got["pkgver"])
	}
	if got["arch"] != want.Arch {
		return fmt.Errorf("arch mismatch: expected %s, got %s", want.Arch, got["arch"])
	}
	return nil
}

// patchBinaryRPATH preserves the binary's original RPATH and appends the
// app's absolute libs dir (the binary lives in ~/.local/bin, not next to
// libs/). If patchelf can't handle the ELF at all, it falls back to a
// launcher script that sets LD_LIBRARY_PATH instead of failing.
func patchBinaryRPATH(binary, libsAbs string) error {
	if elf.IsStatic(binary) {
		return nil // statically linked: loads nothing, patchelf N/A
	}
	orig, rerr := printRpath(binary)
	if rerr == nil {
		if strings.Contains(orig, libsAbs) {
			return nil
		}
		rpath := libsAbs
		if orig != "" {
			rpath = orig + ":" + libsAbs
		}
		if err := patchelfRPATH(binary, rpath); err == nil {
			return nil
		}
	}
	return wrapperFallback(binary, libsAbs)
}

func printRpath(path string) (string, error) {
	out, err := exec.Command("patchelf", "--print-rpath", path).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// wrapperFallback replaces the binary with a shell launcher that points
// LD_LIBRARY_PATH at the app's libs dir, used when patchelf can't rewrite the ELF.
func wrapperFallback(binary, libsAbs string) error {
	real := binary + ".real"
	if err := os.Rename(binary, real); err != nil {
		return fmt.Errorf("patchelf failed and wrapper fallback unavailable: %w", err)
	}
	script := "#!/bin/sh\n" +
		"export LD_LIBRARY_PATH=\"" + libsAbs + "${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\"\n" +
		"exec \"$(dirname -- \"$0\")/" + filepath.Base(real) + "\" \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0755); err != nil {
		return fmt.Errorf("patchelf failed and wrapper write failed: %w", err)
	}
	fmt.Println("  patchelf could not rewrite the ELF; installed an LD_LIBRARY_PATH launcher instead")
	return nil
}

func commitPlacedBinary(name string) {
	for _, p := range []string{filepath.Join(config.BinDir, name), filepath.Join(config.BinDir, name+".real")} {
		os.Remove(p + ".bak")
	}
}

func rollbackPlacedBinary(name string) {
	for _, p := range []string{filepath.Join(config.BinDir, name), filepath.Join(config.BinDir, name+".real")} {
		bak := p + ".bak"
		if _, err := os.Lstat(bak); err != nil {
			continue
		}
		// Drop the half-placed new entry, then restore the backup.
		os.Remove(p)
		os.Rename(bak, p)
	}
}

func removePlacedBinary(name string) bool {
	removed := false
	for _, p := range []string{filepath.Join(config.BinDir, name), filepath.Join(config.BinDir, name+".real")} {
		// Lstat: the BinDir entry is usually a symlink whose target (under
		// ~/.pap/exe) may already be gone; Stat would miss it.
		if _, err := os.Lstat(p); err == nil && os.Remove(p) == nil {
			removed = true
		}
	}
	return removed
}

// binDirInSession reports whether config.BinDir is already on this shell's PATH.
func binDirInSession() bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == config.BinDir {
			return true
		}
	}
	return false
}

// hintPath only tells the user how to get the bin dir on PATH; it never
// edits a shell rc file (that is AddPath's job).
func hintPath() {
	if binDirInSession() {
		return
	}
	if !rcMentionsLocalBin() {
		fmt.Printf("Note: %s is not on PATH; run `pap add-path` to append it to your shell rc, or: export PATH=\"%s:$PATH\"\n", config.BinDir, config.BinDir)
	} else {
		fmt.Printf("Note: %s is not in this shell's PATH; run: export PATH=\"%s:$PATH\"\n", config.BinDir, config.BinDir)
	}
}

// AddPath appends the bin dir to the first existing shell rc file. It is
// idempotent: an rc that already mentions the dir is left alone.
func AddPath() error {
	if rcMentionsLocalBin() {
		fmt.Printf("Note: %s is already referenced in your shell rc\n", config.BinDir)
	} else if rc := appendLocalBinToRc(); rc != "" {
		fmt.Printf("Note: added %s to PATH in %s (restart your shell)\n", config.BinDir, rc)
	} else {
		return fmt.Errorf("could not append %s to any shell rc file", config.BinDir)
	}
	if !binDirInSession() {
		fmt.Printf("Note: %s is not in this shell's PATH; run: export PATH=\"%s:$PATH\"\n", config.BinDir, config.BinDir)
	}
	return nil
}

func rcCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = filepath.Dir(config.BaseDir)
	}
	// PAP_HOME isolation must never touch the real home rc files.
	if v := os.Getenv("PAP_HOME"); v != "" {
		return []string{filepath.Join(v, ".profile")}
	}
	return []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".profile"),
	}
}

func rcMentionsLocalBin() bool {
	want := []string{config.BinDir, "$HOME/.local/bin", "~/.local/bin", ".local/bin"}
	for _, p := range rcCandidates() {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		content := string(data)
		for _, w := range want {
			if strings.Contains(content, w) {
				// Exact-ish match: the rc mentions a local bin dir on PATH.
				// The broad ".local/bin" fallback stays for legacy lines.
				return true
			}
		}
	}
	return false
}

func appendLocalBinToRc() string {
	block := "\n# pap: keep ~/.local/bin on PATH\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"
	for _, p := range rcCandidates() {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			continue
		}
		_, err = f.WriteString(block)
		f.Close()
		if err == nil {
			return p
		}
	}
	p := filepath.Join(filepath.Dir(config.BaseDir), ".profile")
	if v := os.Getenv("PAP_HOME"); v != "" {
		p = filepath.Join(v, ".profile")
	}
	if err := os.WriteFile(p, []byte(block), 0644); err != nil {
		return ""
	}
	return p
}

func patchelfRPATH(binary, rpath string) error {
	cmd := exec.Command("patchelf", "--force-rpath", "--set-rpath", rpath, binary)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// copyFile preserves the source mode (masked to 0777) and never propagates
// setuid/setgid bits.
func copyFile(dst, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	mode := fi.Mode().Perm() & 0777
	if mode == 0 {
		mode = 0755
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	// Ensure exact mode regardless of umask.
	return os.Chmod(dst, mode)
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
