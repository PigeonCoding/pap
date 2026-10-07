package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pap/internal/config"
	"pap/internal/elf"
	"pap/internal/lock"
	"pap/internal/repo"
)

// InstallRepoApp installs an app package from the Arch repos (plus chaotic-aur
// for latest) instead of a local path. version "" tracks the live latest;
// an exact version (pkgver, e.g. "8.11.1-3") that the mirrors no longer carry
// is fetched from the Arch Linux Archive packages/ endpoint. The package
// payload is materialized and installed through the folder machinery, so
// library isolation, RPATH patching and the manifest work exactly as for
// folder installs. name defaults to the package name.
func InstallRepoApp(pkgName, version, name string, opts InstallOptions) error {
	if err := Preflight(); err != nil {
		return err
	}
	lh, err := lock.Acquire(config.LockFile)
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	released := false
	defer func() {
		if !released {
			lh.Release()
		}
	}()

	if pkgName == "" {
		return fmt.Errorf("no package name given (usage: pap install <pkg> [--pkg-version <ver>] [name])")
	}
	if name == "" {
		name = pkgName
	}
	if err := validName(name); err != nil {
		return err
	}
	if err := prepareInstall(name, opts.Force); err != nil {
		return err
	}

	data, info, err := repo.Default().DownloadAppPackage(pkgName, version)
	if err != nil {
		return err
	}
	if version != "" {
		opts.printf("Installing %s %s from %s/%s\n", name, info.Version, info.Repo, info.Name)
	} else {
		opts.printf("Installing %s %s from %s/%s (live latest)\n", name, info.Version, info.Repo, info.Name)
	}

	work, err := os.MkdirTemp("", "pap-repopkg-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	pkgFile := filepath.Join(work, info.Filename)
	if pkgFile == work || info.Filename == "" {
		pkgFile = filepath.Join(work, "app.pkg.tar.zst")
	}
	if err := os.WriteFile(pkgFile, data, 0644); err != nil {
		return err
	}
	payload := filepath.Join(work, "payload")
	if err := os.MkdirAll(payload, 0755); err != nil {
		return err
	}
	if out, err := exec.Command("bsdtar", "-xf", pkgFile, "-C", payload).CombinedOutput(); err != nil {
		return fmt.Errorf("extract %s %s: %w: %s", info.Name, info.Version, err, out)
	}

	mainRel, err := findRepoMainExe(payload, pkgName, name)
	if err != nil {
		return err
	}
	// Hand the lock to installDir (flock would deadlock if nested): it
	// re-acquires and re-checks, so a concurrent install of the same name
	// still fails cleanly instead of clobbering.
	released = true
	lh.Release()
	// Route through the folder installer with the entrypoint pinned: the
	// payload layout (usr/bin/...) is preserved verbatim under exe/<name>/pkg.
	opts.Exe = mainRel
	opts.ManifestSource = "repo:" + pkgName
	opts.ManifestPackage = info.Repo + "/" + info.Name + " " + info.Version
	return installDir(payload, name, opts)
}

// findRepoMainExe locates the app binary inside an extracted package payload:
// usr/bin/<app>, usr/bin/<pkg>, then usr/sbin, then the first executable
// ELF/script under usr/bin. Library-only packages have no entrypoint.
func findRepoMainExe(payload, pkgName, appName string) (string, error) {
	for _, rel := range []string{
		filepath.Join("usr", "bin", appName),
		filepath.Join("usr", "bin", pkgName),
		filepath.Join("usr", "sbin", appName),
		filepath.Join("usr", "sbin", pkgName),
	} {
		p := filepath.Join(payload, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			if elf.IsELF(p) || elf.IsScript(p) {
				return rel, nil
			}
		}
	}
	for _, dir := range []string{filepath.Join("usr", "bin"), filepath.Join("usr", "sbin"), "usr"} {
		root := filepath.Join(payload, dir)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(root, e.Name())
			st, err := os.Stat(p)
			if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
				continue
			}
			if elf.IsELF(p) || elf.IsScript(p) {
				rel, _ := filepath.Rel(payload, p)
				return rel, nil
			}
		}
	}
	return "", fmt.Errorf("package %s ships no executables under usr/bin (library-only package? install the app package instead)", pkgName)
}

// splitRepoSource reports whether a manifest source refers to a repo app
// ("repo:<pkg>") as opposed to a local path.
func splitRepoSource(source string) (string, bool) {
	if rest, ok := strings.CutPrefix(source, "repo:"); ok && rest != "" {
		return rest, true
	}
	return "", false
}

// splitPinnedPackage parses the manifest's pinned app package
// ("<repo>/<name> <version>") back into name and version.
func splitPinnedPackage(pin string) (name, version string, ok bool) {
	repoName, ver, found := strings.Cut(pin, " ")
	if !found || repoName == "" || ver == "" {
		return "", "", false
	}
	_, name, _ = strings.Cut(repoName, "/")
	if name == "" {
		return "", "", false
	}
	return name, ver, true
}
