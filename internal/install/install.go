package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"probe/internal/config"
	"probe/internal/elf"
	"probe/internal/manifest"
	"probe/internal/repo"
	"probe/internal/resolver"
	"probe/internal/store"
)

// InstallOptions controls install behavior.
type InstallOptions struct {
	Force  bool            // overwrite an existing app of the same name
	Locked map[string]string // exact "repo/name" -> version lock (reinstall)
}

func InstallElf(elfPath, name string, opts InstallOptions) error {
	absElf, err := filepath.Abs(elfPath)
	if err != nil {
		return err
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(absElf), filepath.Ext(absElf))
	}
	if err := validName(name); err != nil {
		return err
	}
	if _, err := os.Stat(absElf); err != nil {
		return err
	}

	if err := store.Init(); err != nil {
		return err
	}
	if err := os.MkdirAll(config.AppsDir, 0755); err != nil {
		return err
	}
	pruneStaleStages()

	appDir := filepath.Join(config.AppsDir, name)
	if _, err := os.Stat(appDir); err == nil && !opts.Force {
		return fmt.Errorf("app %q already installed (use --force to overwrite)", name)
	}
	if _, err := os.Stat(filepath.Join(config.BinDir, name)); err == nil && !opts.Force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", filepath.Join(config.BinDir, name))
	}

	if elf.IsScript(absElf) {
		return installScript(absElf, name, opts)
	}
	if !elf.IsELF(absElf) {
		return fmt.Errorf("%s: not an ELF binary or shebang script", absElf)
	}

	info, err := elf.Parse(absElf)
	if err != nil {
		return err
	}
	if err := elf.MachineMismatch(info.Machine, config.Arch); err != nil {
		return err
	}
	if info.Interp != "" {
		if _, err := os.Stat(info.Interp); err != nil {
			return fmt.Errorf("interpreter %s not found on host (unsupported binary, e.g. musl)", info.Interp)
		}
	}
	if st, _ := os.Stat(absElf); st != nil && st.Mode()&0o4000 != 0 {
		fmt.Println("warning: source is setuid; the installed copy drops the setuid bit")
	}

	fmt.Printf("Installing %s from %s\n", name, absElf)
	fmt.Printf("Direct NEEDED: %v\n", info.NEEDED)

	if err := repo.Default().Load(); err != nil {
		return fmt.Errorf("load repo index: %w", err)
	}

	var toResolve []string
	for _, lib := range info.NEEDED {
		if !resolver.SkipLibs[lib] {
			toResolve = append(toResolve, lib)
		}
	}

	stage, err := os.MkdirTemp(config.AppsDir, ".stage-*")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(stage)
		}
	}()

	libsDir := filepath.Join(stage, "libs")
	if err := os.MkdirAll(libsDir, 0755); err != nil {
		return err
	}

	var installedLibs []string
	var privateLibs []string
	usedPkgs := map[string]string{}
	seen := map[string]bool{}
	var failures []string
	pkgCache := map[string][]byte{}

	if len(toResolve) > 0 {
		fmt.Println("\nResolving dependencies (recursive)...")
	}
	queue := toResolve
	for len(queue) > 0 {
		soname := queue[0]
		queue = queue[1:]

		if seen[soname] {
			continue
		}
		seen[soname] = true

		providers := repo.Default().Providers(soname)
		if len(providers) == 0 {
			msg := fmt.Sprintf("%s: no package provides %s", soname, soname)
			fmt.Printf("  [fail] %s\n", msg)
			failures = append(failures, msg)
			continue
		}

		// Try providers in pacman priority order: skip ones that violate a
		// version lock or don't actually contain the soname, fall through to
		// the next candidate instead of failing the install.
		var (
			names, privates []string
			staged          bool
			lastErr         error
		)
		for _, pkg := range providers {
			if opts.Locked != nil {
				key := pkg.Repo + "/" + pkg.Name
				want, ok := opts.Locked[key]
				if !ok {
					lastErr = fmt.Errorf("%s is not in the locked package set", key)
					continue
				}
				if want != pkg.Version {
					lastErr = fmt.Errorf("locked version %s unavailable (index has %s)", want, pkg.Version)
					continue
				}
			}

			pkgKey := pkg.Name + "-" + pkg.Version + "-" + pkg.Arch
			data, ok := pkgCache[pkgKey]
			if !ok {
				var err error
				data, err = repo.Default().Download(pkg)
				if err != nil {
					lastErr = fmt.Errorf("download %s: %w", pkg.Name, err)
					continue
				}
				pkgCache[pkgKey] = data
			}

			n, p, err := extractLibs(pkg, data, soname, libsDir)
			if err != nil {
				lastErr = err
				continue
			}

			names, privates = n, p
			usedPkgs[pkg.Repo+"/"+pkg.Name] = pkg.Version
			fmt.Printf("  [ ok ] %s -> %s/%s %s\n", soname, pkg.Repo, pkg.Name, pkg.Version)
			staged = true
			break
		}
		if !staged {
			if lastErr == nil {
				lastErr = fmt.Errorf("no usable provider")
			}
			msg := fmt.Sprintf("%s: %v", soname, lastErr)
			fmt.Printf("  [fail] %s\n", msg)
			failures = append(failures, msg)
			continue
		}
		installedLibs = append(installedLibs, names...)
		privateLibs = append(privateLibs, privates...)

		depInfo, err := elf.Parse(filepath.Join(libsDir, soname))
		if err == nil {
			for _, n := range depInfo.NEEDED {
				if !resolver.SkipLibs[n] && !seen[n] {
					queue = append(queue, n)
				}
			}
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("cannot install: unsatisfied dependencies:\n  %s", strings.Join(failures, "\n  "))
	}

	// Vendored libs that carry their own RPATH/RUNPATH would bypass the
	// exe's RPATH chain; rewrite them to $ORIGIN so they stay inside libs/.
	if len(privateLibs) > 0 {
		fmt.Println("\nPatching vendored libs with their own RUNPATH...")
	}
	for _, rel := range privateLibs {
		if err := patchelfRPATH(filepath.Join(libsDir, rel), "$ORIGIN"); err != nil {
			return fmt.Errorf("patchelf %s: %w", rel, err)
		}
	}

	fmt.Println("\nChecking dependency compatibility...")
	if err := checkCompat(absElf, libsDir); err != nil {
		return err
	}

	fmt.Println("\nInstalling binary...")
	stagedBin := filepath.Join(stage, name)
	if err := copyFile(absElf, stagedBin); err != nil {
		return err
	}

	// The binary ends up in ~/.local/bin, so RPATH must point at the app's
	// final libs dir (absolute), not the staging dir.
	libsAbs := filepath.Join(config.AppsDir, name, "libs")
	fmt.Println("Patching RPATH...")
	if err := patchBinaryRPATH(stagedBin, libsAbs); err != nil {
		return err
	}

	installedLibs = dedup(installedLibs)
	m := &manifest.Manifest{
		Name:     name,
		Binary:   name,
		Libs:     installedLibs,
		Packages: usedPkgs,
		Source:   absElf,
	}
	if err := manifest.Save(stage, m); err != nil {
		return err
	}

	if err := placeBinary(stagedBin, name, opts.Force); err != nil {
		return err
	}
	defer func() {
		if !published {
			removePlacedBinary(name)
		}
	}()

	if err := publish(stage, name, opts.Force); err != nil {
		return err
	}
	published = true

	ensurePath()
	fmt.Printf("\nDone. Run: %s\n", filepath.Join(config.BinDir, name))
	return nil
}

func Reinstall(name string) error {
	m, err := manifest.Load(filepath.Join(config.AppsDir, name))
	if err != nil {
		return fmt.Errorf("app %q not installed: %w", name, err)
	}
	if m.Source == "" {
		return fmt.Errorf("%s: manifest has no source path (installed by an older version); use install instead", name)
	}
	return InstallElf(m.Source, name, InstallOptions{Force: true, Locked: m.Packages})
}

func Upgrade(name string) error {
	m, err := manifest.Load(filepath.Join(config.AppsDir, name))
	if err != nil {
		return fmt.Errorf("app %q not installed: %w", name, err)
	}
	if m.Source == "" {
		return fmt.Errorf("%s: manifest has no source path (installed by an older version); use install instead", name)
	}
	return InstallElf(m.Source, name, InstallOptions{Force: true})
}

// installScript passes shebang scripts through verbatim: there are no ELF
// dependencies to isolate.
func installScript(path, name string, opts InstallOptions) error {
	fmt.Printf("Installing %s from %s (script, copied as-is)\n", name, path)
	stage, err := os.MkdirTemp(config.AppsDir, ".stage-*")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(stage)
		}
	}()

	if err := copyFile(path, filepath.Join(stage, name)); err != nil {
		return err
	}
	m := &manifest.Manifest{
		Name:     name,
		Binary:   name,
		Packages: map[string]string{},
		Source:   path,
	}
	if err := manifest.Save(stage, m); err != nil {
		return err
	}

	if err := placeBinary(filepath.Join(stage, name), name, opts.Force); err != nil {
		return err
	}
	defer func() {
		if !published {
			removePlacedBinary(name)
		}
	}()

	if err := publish(stage, name, opts.Force); err != nil {
		return err
	}
	published = true
	ensurePath()
	fmt.Printf("Done. Run: %s\n", filepath.Join(config.BinDir, name))
	return nil
}

// pruneStaleStages removes staging dirs orphaned by killed installs.
func pruneStaleStages() {
	entries, _ := os.ReadDir(config.AppsDir)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".stage-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.RemoveAll(filepath.Join(config.AppsDir, e.Name()))
		}
	}
}

func publish(stage, name string, force bool) error {
	final := filepath.Join(config.AppsDir, name)
	if _, err := os.Stat(final); err == nil {
		if !force {
			return fmt.Errorf("app %q already installed (use --force to overwrite)", name)
		}
		if err := os.RemoveAll(final); err != nil {
			return err
		}
	}
	return os.Rename(stage, final)
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.Contains(name, string(filepath.Separator)) {
		return fmt.Errorf("invalid app name %q", name)
	}
	return nil
}

func InstallPkg(pkgName string) error {
	return fmt.Errorf("not yet implemented")
}

func Uninstall(name string) error {
	appDir := filepath.Join(config.AppsDir, name)
	if _, err := os.Stat(appDir); os.IsNotExist(err) {
		return fmt.Errorf("app %s not found", name)
	}

	m, err := manifest.Load(filepath.Join(config.AppsDir, name))
	if err != nil {
		fmt.Printf("Warning: no manifest found, removing anyway\n")
	}

	if err := os.RemoveAll(appDir); err != nil {
		return err
	}
	if removePlacedBinary(name) {
		fmt.Printf("  Removed %s\n", filepath.Join(config.BinDir, name))
	}

	fmt.Printf("Uninstalled %s\n", name)
	if m != nil {
		fmt.Printf("  Removed %d libs from %s\n", len(m.Libs), name)
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

	fmt.Printf("%-20s %-30s %s\n", "NAME", "BINARY", "LIBS")
	fmt.Println(strings.Repeat("-", 70))
	for _, m := range manifests {
		fmt.Printf("%-20s %-30s %d libs\n", m.Name, m.Binary, len(m.Libs))
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

	if err := verifyPkgFile(pkgFile, pkg); err != nil {
		return nil, nil, err
	}

	extract := exec.Command("bsdtar", "-xf", pkgFile, "-C", tmpDir, "usr/lib/")
	extract.Stdout = os.Stdout
	extract.Stderr = os.Stderr
	if err := extract.Run(); err != nil {
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

		privateFile := false
		if info, err := elf.Parse(src); err == nil && (info.RPATH != "" || info.RUNPATH != "") {
			privateFile = true
		}

		dst := filepath.Join(libsDir, rel)
		if privateFile {
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return
			}
			if err := os.WriteFile(dst, data, 0755); err != nil {
				return
			}
			private = append(private, rel)
		} else {
			storePath, err := store.Store(data)
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

// placeBinary moves the staged binary (plus an optional .real target for the
// wrapper fallback) into ~/.local/bin/<name>.
func placeBinary(stagedBin, name string, force bool) error {
	if err := os.MkdirAll(config.BinDir, 0755); err != nil {
		return err
	}
	dest := filepath.Join(config.BinDir, name)
	if !force {
		for _, p := range []string{dest, dest + ".real"} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s already exists (use --force to overwrite)", p)
			}
		}
	}
	if _, err := os.Stat(stagedBin + ".real"); err == nil {
		if err := moveFile(stagedBin+".real", dest+".real"); err != nil {
			return err
		}
	}
	return moveFile(stagedBin, dest)
}

func removePlacedBinary(name string) bool {
	removed := false
	for _, p := range []string{filepath.Join(config.BinDir, name), filepath.Join(config.BinDir, name+".real")} {
		if _, err := os.Stat(p); err == nil && os.Remove(p) == nil {
			removed = true
		}
	}
	return removed
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// different filesystem: copy then remove
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// ensurePath guarantees ~/.local/bin is on PATH: if nothing in the shell rc
// files mentions it, an export line is appended; if the current session's
// PATH misses it, a hint is printed.
func ensurePath() {
	inSession := false
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == config.BinDir {
			inSession = true
			break
		}
	}

	var rcAdded string
	if !rcMentionsLocalBin() {
		rcAdded = appendLocalBinToRc()
	}

	switch {
	case rcAdded != "":
		fmt.Printf("Note: added ~/.local/bin to PATH in %s (restart your shell)\n", rcAdded)
	case !inSession:
		fmt.Printf("Note: ~/.local/bin is not in this shell's PATH; run: export PATH=\"$HOME/.local/bin:$PATH\"\n")
	}
}

func rcCandidates() []string {
	home := filepath.Dir(config.BaseDir)
	return []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".profile"),
	}
}

func rcMentionsLocalBin() bool {
	for _, p := range rcCandidates() {
		if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), ".local/bin") {
			return true
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

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0755)
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
