package resolver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"pap/internal/config"
	"pap/internal/elf"
	"pap/internal/repo"
)

// glibcFamily are the libraries owned by the host glibc/loader coupling.
// Unlike libstdc++/libgcc they cannot be isolated: the loader, libc and
// these stubs must come from the same glibc build as the running host.
var glibcFamily = []string{
	"libc.so.6",
	"libm.so.6",
	"libpthread.so.0",
	"libdl.so.2",
	"librt.so.1",
	"libresolv.so.2",
	"libutil.so.1",
	"libcrypt.so.1",
	"libcrypt.so.2",
	"libnsl.so.1",
	"libnsl.so.2",
	"libanl.so.1",
	"libmvec.so.1",
	"libthread_db.so.1",
	"libmemusage.so",
	"libpcprofile.so",
	"libSegFault.so",
}

// loadersByArch maps host arch to its ELF interpreter soname.
func loadersByArch(arch string) []string {
	switch arch {
	case "x86_64":
		return []string{"ld-linux-x86-64.so.2"}
	case "aarch64":
		return []string{"ld-linux-aarch64.so.1"}
	case "i686":
		return []string{"ld-linux.so.2"}
	default:
		return []string{"ld-linux.so.2", "ld-linux-x86-64.so.2", "ld-linux-aarch64.so.1"}
	}
}

// LoaderSonames returns the ELF interpreter sonames for arch (usually one).
// Used by --force-libc to vendor the loader and rewrite PT_INTERP.
func LoaderSonames(arch string) []string { return loadersByArch(arch) }

// HostGlibcLib reports whether soname belongs to the host glibc/loader
// coupling (the glibc family plus loaders): libraries that normally stay on
// the host and are skipped during resolution. --force-libc inverts that for
// exactly this set, vendoring it from the repos instead.
func HostGlibcLib(soname string) bool {
	base := soname
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	for _, l := range loadersByArch(config.Arch) {
		if base == l {
			return true
		}
	}
	if base == "ld-linux.so.2" {
		return true
	}
	for _, s := range glibcFamily {
		if base == s {
			return true
		}
	}
	return false
}

// SkipLibs is the process-wide host-provided set. It is rebuilt from
// BuildSkipLibs(config.Arch) plus a dynamic host closure (see HostLibs).
// Kept as a var for existing call sites; prefer Skip() which also handles
// libnss_* wildcards.
var SkipLibs = BuildSkipLibs(config.Arch)

// BuildSkipLibs returns the static host-provided set for arch.
func BuildSkipLibs(arch string) map[string]bool {
	out := map[string]bool{
		"linux-vdso.so.1": true,
		"linux-gate.so.1": true,
	}
	for _, l := range loadersByArch(arch) {
		out[l] = true
	}
	// Older 32-bit loader name is always host-provided when present.
	out["ld-linux.so.2"] = true
	for _, s := range glibcFamily {
		out[s] = true
	}
	for s := range HostLibs() {
		out[s] = true
	}
	return out
}

// Refresh rebuilds SkipLibs from the current config.Arch (call after
// config.Refresh in tests).
func Refresh() {
	ResetLDConfigCache()
	SkipLibs = BuildSkipLibs(config.Arch)
}

// ResetLDConfigCache drops the memoized `ldconfig -p` result so the next
// lookup re-probes the host. Tests call it via Refresh; production code
// never needs it (ldconfig output is stable per process).
func ResetLDConfigCache() {
	ldconfigOnce = sync.Once{}
	ldconfigCache = nil
}

// Skip reports whether soname is host-provided (kernel vdso, loader,
// glibc family, or nss plugins which must match the host libc).
func Skip(soname string) bool {
	if SkipLibs[soname] {
		return true
	}
	base := soname
	if i := strings.Index(base, ".so"); i >= 0 {
		// libnss_files.so.2, libnss_dns.so.2, ... all host-provided.
		name := base
		if j := strings.LastIndex(name, "/"); j >= 0 {
			name = name[j+1:]
		}
		if strings.HasPrefix(name, "libnss_") {
			return true
		}
	}
	return false
}

// HostLibs derives the host-provided set dynamically: starting from the
// host loader + libc on disk, walk their DT_NEEDED closure. Anything the
// host libc itself needs (loader, ld-linux deps) must come from the host,
// so the skip list tracks the running system instead of a hand-written
// list. Missing files simply contribute nothing.
func HostLibs() map[string]bool {
	out := map[string]bool{}
	var roots []string
	for _, p := range loaderPaths() {
		if _, err := os.Stat(p); err == nil {
			roots = append(roots, p)
		}
	}
	for _, p := range libcPaths() {
		if _, err := os.Stat(p); err == nil {
			roots = append(roots, p)
			break
		}
	}
	seen := map[string]bool{}
	queue := roots
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		info, err := elf.Parse(p)
		if err != nil {
			continue
		}
		for _, n := range info.NEEDED {
			out[n] = true
			if fp := findHostLib(n); fp != "" && !seen[fp] {
				queue = append(queue, fp)
			}
		}
		// The file itself, by SONAME.
		if info.SONAME != "" {
			out[info.SONAME] = true
		} else {
			out[filepath.Base(p)] = true
		}
	}
	return out
}

func loaderPaths() []string {
	var out []string
	for _, arch := range []string{config.Arch, "x86_64", "aarch64", "i686"} {
		for _, l := range loadersByArch(arch) {
			for _, dir := range hostSearchDirs() {
				out = append(out, filepath.Join(dir, l))
			}
		}
	}
	return out
}

func libcPaths() []string {
	var out []string
	for _, dir := range hostSearchDirs() {
		out = append(out, filepath.Join(dir, "libc.so.6"))
	}
	return out
}

// FindHostLib locates soname on the host. It consults `ldconfig -p` first
// (authoritative on Debian multiarch, Nix, etc.), then falls back to a
// directory search covering both Arch (/usr/lib) and Debian
// (/usr/lib/x86_64-linux-gnu, /lib/x86_64-linux-gnu, ...) layouts.
// Every candidate is validated (must parse as an ELF for this host's
// architecture with a matching SONAME); stale ldconfig entries and
// wrong-architecture copies are skipped, never returned.
// Returns "" when not found.
func FindHostLib(soname string) string {
	if strings.Contains(soname, "/") {
		if _, err := os.Stat(soname); err == nil {
			return soname
		}
		return ""
	}
	cands := []string{ldconfigLibs()[soname]}
	for _, dir := range hostSearchDirs() {
		cands = append(cands, filepath.Join(dir, soname))
	}
	for _, p := range cands {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if hostCandidateValid(p, soname) {
			return p
		}
	}
	return ""
}

// hostCandidateValid reports whether path is usable as the host provider of
// soname: it must parse as an ELF built for this host (ldconfig can list
// stale paths and foreign-architecture copies, e.g. i386 libc on x86_64),
// and its SONAME must match when it declares one.
func hostCandidateValid(path, soname string) bool {
	info, err := elf.Parse(path)
	if err != nil {
		return false
	}
	if err := elf.MachineMismatch(info.Machine, config.Arch); err != nil {
		return false
	}
	if info.SONAME != "" && info.SONAME != soname {
		return false
	}
	return true
}

func findHostLib(soname string) string {
	return FindHostLib(soname)
}

// hostSearchDirs covers Arch (/usr/lib) and Debian multiarch
// (/usr/lib/<triplet>, /lib/<triplet>) layouts. Globs are expanded once
// per call; missing dirs simply contribute nothing.
func hostSearchDirs() []string {
	base := []string{"/usr/lib", "/lib", "/usr/lib64", "/lib64", "/usr/lib32", "/lib32"}
	out := append([]string(nil), base...)
	for _, root := range []string{"/usr/lib", "/lib"} {
		matches, _ := filepath.Glob(filepath.Join(root, "*-linux-gnu"))
		out = append(out, matches...)
	}
	seen := map[string]bool{}
	var dedup []string
	for _, d := range out {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		dedup = append(dedup, d)
	}
	return dedup
}

var (
	ldconfigOnce  sync.Once
	ldconfigCache map[string]string
)

// ldconfigLibs parses `ldconfig -p` into soname -> path. Empty when ldconfig
// is missing or fails; callers fall back to directory search. Debian keeps
// ldconfig in /sbin (off a regular user's PATH), so probe that too.
func ldconfigLibs() map[string]string {
	ldconfigOnce.Do(func() {
		ldconfigCache = map[string]string{}
		var out []byte
		var err error
		for _, bin := range []string{"ldconfig", "/sbin/ldconfig"} {
			out, err = exec.Command(bin, "-p").Output()
			if err == nil {
				break
			}
		}
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			// e.g. "\tlibc.so.6 (libc6,x86-64) => /lib/x86_64-linux-gnu/libc.so.6"
			arrow := strings.Index(line, "=>")
			if arrow < 0 {
				continue
			}
			left := strings.TrimSpace(line[:arrow])
			right := strings.TrimSpace(line[arrow+2:])
			if left == "" || right == "" {
				continue
			}
			soname := strings.Fields(left)[0]
			if _, err := os.Stat(right); err != nil {
				continue
			}
			if _, ok := ldconfigCache[soname]; !ok {
				ldconfigCache[soname] = right
			}
		}
	})
	return ldconfigCache
}

func Resolve(soname string) (repo.PkgInfo, error) {
	return repo.Default().Resolve(soname)
}
