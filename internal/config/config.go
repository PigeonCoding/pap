package config

import (
	"os"
	"path/filepath"
	"runtime"
)

var (
	BaseDir     = baseDir()
	AppsDir     = filepath.Join(BaseDir, "apps")
	StoreDir    = filepath.Join(BaseDir, "store")
	PkgCacheDir = filepath.Join(BaseDir, "pkgcache")
	BinDir      = filepath.Join(filepath.Dir(BaseDir), ".local", "bin")
	Arch        = hostArch()
)

func baseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".pap")
}

func hostArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "386":
		return "i686"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

// ArchVars returns substitutions for pacman mirror URLs, longest key first.
func ArchVars() [][2]string {
	v3 := Arch
	v4 := Arch
	if Arch == "x86_64" {
		v3 = "x86_64_v3"
		v4 = "x86_64_v4"
	}
	return [][2]string{
		{"$arch_v4", v4},
		{"$arch_v3", v3},
		{"$arch", Arch},
	}
}
