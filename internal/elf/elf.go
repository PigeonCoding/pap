package elf

import (
	"debug/elf"
	"fmt"
	"io"
	"os"
)

type LibInfo struct {
	SONAME  string
	NEEDED  []string
	RPATH   string
	RUNPATH string
	Interp  string
	Machine elf.Machine
}

func Parse(path string) (*LibInfo, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open elf: %w", err)
	}
	defer f.Close()

	info := &LibInfo{Machine: f.Machine}

	if f.Type == elf.ET_DYN || f.Type == elf.ET_EXEC {
		needed, err := f.ImportedLibraries()
		if err != nil {
			return nil, fmt.Errorf("imported libraries: %w", err)
		}
		info.NEEDED = needed
	}

	soname, err := f.DynString(elf.DT_SONAME)
	if err == nil && len(soname) > 0 {
		info.SONAME = soname[0]
	}

	if rpath, err := f.DynString(elf.DT_RPATH); err == nil && len(rpath) > 0 {
		info.RPATH = rpath[0]
	}
	if runpath, err := f.DynString(elf.DT_RUNPATH); err == nil && len(runpath) > 0 {
		info.RUNPATH = runpath[0]
	}

	info.Interp = interpPath(f)

	return info, nil
}

func interpPath(f *elf.File) string {
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(p.Open(), 1<<10))
		if err != nil {
			return ""
		}
		for i, b := range data {
			if b == 0 {
				return string(data[:i])
			}
		}
		return string(data)
	}
	return ""
}

func IsELF(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func IsScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [2]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	return hdr[0] == '#' && hdr[1] == '!'
}

// IsStatic reports whether path is a statically linked ELF (no .dynamic
// section): it loads no shared libraries, so there is nothing to isolate
// and patchelf cannot rewrite it.
func IsStatic(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return f.Section(".dynamic") == nil
}

// HostMachineError returns a descriptive error when the ELF targets a
// different architecture than the host we are installing for.
func MachineMismatch(machine elf.Machine, hostArch string) error {
	switch hostArch {
	case "x86_64":
		if machine != elf.EM_X86_64 {
			return fmt.Errorf("binary is %s, host is x86_64: cross-architecture installs are not supported", machine)
		}
	case "aarch64":
		if machine != elf.EM_AARCH64 {
			return fmt.Errorf("binary is %s, host is aarch64: cross-architecture installs are not supported", machine)
		}
	case "i686":
		if machine != elf.EM_386 {
			return fmt.Errorf("binary is %s, host is i686: cross-architecture installs are not supported", machine)
		}
	}
	return nil
}
