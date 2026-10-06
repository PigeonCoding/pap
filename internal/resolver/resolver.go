package resolver

import (
	"fmt"

	"probe/internal/elf"
	"probe/internal/repo"
)

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
	"libgcc_s.so.1":        true,
	"libstdc++.so.6":       true,
	"libgomp.so.1":         true,
}

type Dep struct {
	SONAME string
	Pkg    repo.PkgInfo
}

func Resolve(soname string) (repo.PkgInfo, error) {
	return repo.Default().Resolve(soname)
}

func ResolveRecursive(elfPath string) ([]Dep, error) {
	idx := repo.Default()
	if err := idx.Load(); err != nil {
		return nil, fmt.Errorf("load repo index: %w", err)
	}

	seen := map[string]bool{}
	var deps []Dep

	var walk func(path string) error
	walk = func(path string) error {
		info, err := elf.Parse(path)
		if err != nil {
			return err
		}

		for _, needed := range info.NEEDED {
			if SkipLibs[needed] || seen[needed] {
				continue
			}
			seen[needed] = true

			pkg, err := idx.Resolve(needed)
			if err != nil {
				fmt.Printf("  [warn] %s: %v\n", needed, err)
				continue
			}

			deps = append(deps, Dep{SONAME: needed, Pkg: pkg})
		}
		return nil
	}

	if err := walk(elfPath); err != nil {
		return nil, err
	}

	return deps, nil
}
