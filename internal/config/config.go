package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// Config groups all filesystem locations derived from a single root.
// BaseDir defaults to ~/.pap, or $PAP_HOME when set (tests, isolation).
type Config struct {
	BaseDir      string
	AppsDir      string
	ExeDir       string
	StoreDir     string
	PkgCacheDir  string
	CacheDir     string
	RepoCacheDir string
	Mirrorlist   string
	BinDir       string
	LockFile     string
	Arch         string
}

// Resolve builds a Config from the environment.
func Resolve() Config {
	base := baseDir()
	apps := filepath.Join(base, "apps")
	exe := filepath.Join(base, "exe")
	store := filepath.Join(base, "store")
	pkgCache := filepath.Join(base, "pkgcache")
	cache := cacheDir(base)
	repoCache := filepath.Join(cache, "repo")
	mirror := filepath.Join(base, "mirrorlist")
	home, err := os.UserHomeDir()
	if err != nil {
		home = filepath.Dir(base)
	}
	bin := filepath.Join(home, ".local", "bin")
	// When PAP_HOME isolates state, keep binaries inside it too so tests
	// never touch the real ~/.local/bin.
	if v := os.Getenv("PAP_HOME"); v != "" {
		bin = filepath.Join(base, "bin")
	}
	if v := os.Getenv("PAP_BIN_DIR"); v != "" {
		bin = v
	}
	return Config{
		BaseDir:      base,
		AppsDir:      apps,
		ExeDir:       exe,
		StoreDir:     store,
		PkgCacheDir:  pkgCache,
		CacheDir:     cache,
		RepoCacheDir: repoCache,
		Mirrorlist:   mirror,
		BinDir:       bin,
		LockFile:     filepath.Join(base, "lock"),
		Arch:         hostArch(),
	}
}

// Refresh recomputes the package-level vars from the environment.
// Tests set PAP_HOME then call Refresh; callers should Refresh once at
// startup after flag parsing.
func Refresh() {
	c := Resolve()
	BaseDir = c.BaseDir
	AppsDir = c.AppsDir
	ExeDir = c.ExeDir
	StoreDir = c.StoreDir
	PkgCacheDir = c.PkgCacheDir
	CacheDir = c.CacheDir
	RepoCacheDir = c.RepoCacheDir
	Mirrorlist = c.Mirrorlist
	BinDir = c.BinDir
	LockFile = c.LockFile
	Arch = c.Arch
}

var (
	BaseDir      = Resolve().BaseDir
	AppsDir      = Resolve().AppsDir
	ExeDir       = Resolve().ExeDir
	StoreDir     = Resolve().StoreDir
	PkgCacheDir  = Resolve().PkgCacheDir
	CacheDir     = Resolve().CacheDir
	RepoCacheDir = Resolve().RepoCacheDir
	Mirrorlist   = Resolve().Mirrorlist
	BinDir       = Resolve().BinDir
	LockFile     = Resolve().LockFile
	Arch         = Resolve().Arch
)

func baseDir() string {
	if v := os.Getenv("PAP_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".pap")
}

func cacheDir(base string) string {
	// Isolated runs keep cache inside the state dir; real runs follow XDG.
	if v := os.Getenv("PAP_HOME"); v != "" {
		return filepath.Join(base, "cache")
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "pap")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(base, "cache")
	}
	return filepath.Join(home, ".cache", "pap")
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
