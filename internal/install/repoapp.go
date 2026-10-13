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
//
// With default options and no pinned version, the install is glibc-aware:
// when the live latest needs newer glibc than the host provides, the newest
// compatible archived build is installed instead (dependencies resolved
// era-consistently). When even that build's dependency closure turns out too
// new, older builds are tried automatically, bounded by maxInstallAttempts.
func InstallRepoApp(pkgName, version, name string, opts InstallOptions) error {
	if err := Preflight(); err != nil {
		return err
	}
	if pkgName == "" {
		return fmt.Errorf("no package name given (usage: pap install <pkg> [--pkg-version <ver>] [name])")
	}
	if name == "" {
		name = pkgName
	}
	if err := validName(name); err != nil {
		return err
	}

	auto := version == "" && !opts.NoGlibcCompat && !opts.NoArchive && !opts.ForceLibc && allowGlibcCompatSearch()
	tried := map[string]bool{}
	const maxInstallAttempts = 4
	for attempt := 0; ; attempt++ {
		// Short critical section per attempt: claim the name and prune
		// stale stages under the cross-process lock. Downloads and the
		// install itself run unlocked (installDir takes the lock itself);
		// a concurrent install racing us fails cleanly at the next
		// prepareInstall instead of clobbering.
		lh, err := lock.Acquire(config.LockFile)
		if err != nil {
			return fmt.Errorf("acquire lock: %w", err)
		}
		prepErr := prepareInstall(name, opts.Force)
		lh.Release()
		if prepErr != nil {
			return prepErr
		}

		ver := version
		if auto && ver == "" {
			// Automatic glibc-compatible version selection: when the
			// live latest Arch build needs newer glibc than the host
			// provides, install the newest archived build the host
			// can run instead, with dependencies resolved
			// era-consistently (PreferArchive).
			if compatVer, ok := autoGlibcVersion(pkgName, opts); ok {
				ver = compatVer
				opts.PreferArchive = true
			}
		}

		data, info, err := repo.Default().DownloadAppPackage(pkgName, ver)
		if err != nil {
			return err
		}
		if ver != "" {
			opts.printf("Installing %s %s from %s/%s\n", name, info.Version, info.Repo, info.Name)
		} else {
			opts.printf("Installing %s %s from %s/%s (live latest)\n", name, info.Version, info.Repo, info.Name)
		}

		work, err := os.MkdirTemp("", "pap-repopkg-*")
		if err != nil {
			return err
		}
		pkgFile := filepath.Join(work, info.Filename)
		if pkgFile == work || info.Filename == "" {
			pkgFile = filepath.Join(work, "app.pkg.tar.zst")
		}
		if werr := os.WriteFile(pkgFile, data, 0644); werr != nil {
			os.RemoveAll(work)
			return werr
		}
		payload := filepath.Join(work, "payload")
		if merr := os.MkdirAll(payload, 0755); merr != nil {
			os.RemoveAll(work)
			return merr
		}
		if out, xerr := exec.Command("bsdtar", "-xf", pkgFile, "-C", payload).CombinedOutput(); xerr != nil {
			os.RemoveAll(work)
			return fmt.Errorf("extract %s %s: %w: %s", info.Name, info.Version, xerr, out)
		}

		mainRel, merr := findRepoMainExe(payload, pkgName, name)
		if merr != nil {
			os.RemoveAll(work)
			return merr
		}
		// Route through the folder installer with the entrypoint pinned: the
		// payload layout (usr/bin/...) is preserved verbatim under exe/<name>/pkg.
		attemptOpts := opts
		attemptOpts.Exe = mainRel
		attemptOpts.ManifestSource = "repo:" + pkgName
		attemptOpts.ManifestPackage = info.Repo + "/" + info.Name + " " + info.Version
		ierr := installDir(payload, name, attemptOpts)
		os.RemoveAll(work)
		if ierr == nil {
			return nil
		}
		// A dependency closure still too new for the host means the
		// era-consistent snapshot moved past the host as well: continue
		// the search with older app builds instead of failing. Anything
		// else (network, missing deps, pinned versions) returns as-is.
		// installDir cleans up its uncommitted stages, so retries start
		// from a clean slate.
		if !auto || !IsHostGlibcError(ierr) || attempt+1 >= maxInstallAttempts {
			return ierr
		}
		tried[info.Version] = true
		next, nerr := nextCompatibleVersion(pkgName, tried, opts)
		if nerr != nil {
			opts.printf("warning: %v\n", nerr)
			return ierr
		}
		opts.printf("Dependency closure still needs newer glibc than this host provides; trying older build %s %s ...\n", pkgName, next)
		version = next
		opts.PreferArchive = true
	}
}

// allowGlibcCompatSearch reports whether archive-backed glibc-compatible
// version selection may run. Under test indexes (httptest Default) archive
// access would hit the real network, so it is skipped — unless a test
// explicitly opted in via testArchiveAccess (with ArchiveBaseURL also
// pointed at httptest, keeping everything hermetic).
var testArchiveAccess = false

func allowGlibcCompatSearch() bool {
	return !repo.IsTestOverride() || testArchiveAccess
}

// findRepoMainExe locates the app binary inside an extracted package payload:
// usr/bin/<app>, usr/bin/<pkg>, usr/sbin/<app>, usr/sbin/<pkg>, else the first
// ELF under usr/bin, usr/sbin or usr — a script only when the package ships no
// ELF at all. Library-only packages have no entrypoint.
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
	// No name match: the package's own command is the ELF in its bin dir.
	// A script shipped beside it (corepack, npm, ...) only wraps that ELF
	// and would exec a name the host PATH does not have, so scripts count
	// as a candidate only when the package ships no ELF at all.
	for _, wantELF := range []bool{true, false} {
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
				if isELF := elf.IsELF(p); isELF != wantELF {
					continue
				}
				if !wantELF && !elf.IsScript(p) {
					continue
				}
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
