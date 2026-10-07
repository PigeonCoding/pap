package resolver

import (
	"os"
	"path/filepath"
	"strings"

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
	SkipLibs = BuildSkipLibs(config.Arch)
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
			for _, dir := range []string{"/usr/lib", "/lib", "/usr/lib64", "/lib64"} {
				out = append(out, filepath.Join(dir, l))
			}
		}
	}
	return out
}

func libcPaths() []string {
	return []string{
		"/usr/lib/libc.so.6",
		"/lib/libc.so.6",
		"/usr/lib64/libc.so.6",
		"/lib64/libc.so.6",
	}
}

func findHostLib(soname string) string {
	for _, dir := range []string{"/usr/lib", "/lib", "/usr/lib64", "/lib64"} {
		p := filepath.Join(dir, soname)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func Resolve(soname string) (repo.PkgInfo, error) {
	return repo.Default().Resolve(soname)
}
