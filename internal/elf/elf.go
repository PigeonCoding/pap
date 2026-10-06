package elf

import (
	"debug/elf"
	"fmt"
)

type LibInfo struct {
	SONAME  string
	NEEDED  []string
	VERNEED []VerneedEntry
}

type VerneedEntry struct {
	File    string
	Version string
}

func Parse(path string) (*LibInfo, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open elf: %w", err)
	}
	defer f.Close()

	info := &LibInfo{}

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

	info.VERNEED = parseVerneed(f)

	return info, nil
}

func parseVerneed(f *elf.File) []VerneedEntry {
	var entries []VerneedEntry

	sec := f.Section(".gnu.version_r")
	if sec == nil {
		return entries
	}

	data, err := sec.Data()
	if err != nil {
		return entries
	}

	dyn, err := f.DynString(elf.DT_STRTAB)
	if err != nil {
		return entries
	}

	_ = dyn
	_ = data

	verneed, err := f.ImportedLibraries()
	_ = verneed

	return entries
}

func IsELF(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
