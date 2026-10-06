package resolver

import (
	"probe/internal/repo"
)

// SkipLibs are provided by the host (glibc loader coupling) or the kernel.
// Unlike glibc, libstdc++/libgcc are isolatable and are NOT skipped.
var SkipLibs = map[string]bool{
	"linux-vdso.so.1":      true,
	"linux-gate.so.1":      true,
	"ld-linux-x86-64.so.2": true,
	"ld-linux.so.2":        true,
	"libc.so.6":            true,
	"libm.so.6":            true,
	"libpthread.so.0":      true,
	"libdl.so.2":           true,
	"librt.so.1":           true,
	"libresolv.so.2":       true,
}

func Resolve(soname string) (repo.PkgInfo, error) {
	return repo.Default().Resolve(soname)
}
