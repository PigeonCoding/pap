package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"probe/internal/elf"
	"probe/internal/manifest"
	"probe/internal/repo"
	"probe/internal/resolver"
	"probe/internal/store"
)

var BaseDir = "/tmp/opencode/probe-test"
var AppsDir = filepath.Join(BaseDir, "apps")

func InstallElf(elfPath, name string) error {
	absElf, err := filepath.Abs(elfPath)
	if err != nil {
		return err
	}

	if name == "" {
		name = strings.TrimSuffix(filepath.Base(absElf), filepath.Ext(absElf))
	}

	if err := store.Init(); err != nil {
		return err
	}
	if err := os.MkdirAll(AppsDir, 0755); err != nil {
		return err
	}

	info, err := elf.Parse(absElf)
	if err != nil {
		return err
	}

	fmt.Printf("Installing %s from %s\n", name, absElf)
	fmt.Printf("Direct NEEDED: %v\n", info.NEEDED)

	if err := repo.Default().Load(); err != nil {
		return fmt.Errorf("load repo index: %w", err)
	}

	var toResolve []string
	for _, lib := range info.NEEDED {
		if !resolver.SkipLibs[lib] {
			toResolve = append(toResolve, lib)
		}
	}

	if len(toResolve) == 0 {
		fmt.Println("No external libs to isolate.")
		return nil
	}

	libsDir := filepath.Join(AppsDir, name, "libs")
	if err := os.MkdirAll(libsDir, 0755); err != nil {
		return err
	}

	var installedLibs []string
	usedPkgs := map[string]string{}
	seen := map[string]bool{}
	queue := toResolve
	pkgCache := map[string][]byte{}

	fmt.Println("\nResolving dependencies (recursive)...")
	for len(queue) > 0 {
		soname := queue[0]
		queue = queue[1:]

		if seen[soname] {
			continue
		}
		seen[soname] = true

		pkg, err := resolver.Resolve(soname)
		if err != nil {
			fmt.Printf("  [warn] %s: %v\n", soname, err)
			continue
		}

		usedPkgs[pkg.Repo+"/"+pkg.Name] = pkg.Version
		fmt.Printf("  [ ok ] %s -> %s/%s %s\n", soname, pkg.Repo, pkg.Name, pkg.Version)

		key := pkg.Name + "-" + pkg.Version + "-" + pkg.Arch
		data, ok := pkgCache[key]
		if !ok {
			data, err = repo.Default().Download(pkg)
			if err != nil {
				fmt.Printf("  [fail] download: %v\n", err)
				continue
			}
			pkgCache[key] = data
		}

		libs, err := extractLibs(pkg, data, soname, libsDir)
		if err != nil {
			fmt.Printf("  [fail] extract: %v\n", err)
			continue
		}
		installedLibs = append(installedLibs, libs...)

		depInfo, err := elf.Parse(filepath.Join(libsDir, soname))
		if err == nil {
			for _, n := range depInfo.NEEDED {
				if !resolver.SkipLibs[n] && !seen[n] {
					queue = append(queue, n)
				}
			}
		}
	}

	fmt.Println("\nChecking symbol version compatibility...")
	checkPaths := []string{absElf}
	for _, lib := range installedLibs {
		checkPaths = append(checkPaths, filepath.Join(libsDir, lib))
	}
	if err := checkCompat(checkPaths, libsDir); err != nil {
		return err
	}

	fmt.Println("\nInstalling binary...")
	installed := filepath.Join(AppsDir, name, filepath.Base(absElf))
	if err := copyFile(absElf, installed); err != nil {
		return err
	}

	fmt.Println("Patching RPATH...")
	if err := patchelfRPATH(installed, "$ORIGIN/libs"); err != nil {
		return err
	}

	m := &manifest.Manifest{
		Name:     name,
		Binary:   filepath.Base(absElf),
		Libs:     installedLibs,
		Packages: usedPkgs,
	}
	if err := manifest.Save(AppsDir, name, m); err != nil {
		return err
	}

	fmt.Printf("\nDone. Run: %s\n", installed)
	return nil
}

func InstallPkg(pkgName string) error {
	return fmt.Errorf("not yet implemented")
}

func Uninstall(name string) error {
	appDir := filepath.Join(AppsDir, name)
	if _, err := os.Stat(appDir); os.IsNotExist(err) {
		return fmt.Errorf("app %s not found", name)
	}

	m, err := manifest.Load(AppsDir, name)
	if err != nil {
		fmt.Printf("Warning: no manifest found, removing anyway\n")
	}

	if err := os.RemoveAll(appDir); err != nil {
		return err
	}

	fmt.Printf("Uninstalled %s\n", name)
	if m != nil {
		fmt.Printf("  Removed %d libs from %s\n", len(m.Libs), name)
	}
	return nil
}

func GC() error {
	used := store.ScanUsed()
	removed, err := store.GC(used)
	if err != nil {
		return err
	}
	fmt.Printf("GC: removed %d unused entries\n", removed)
	return nil
}

func List() error {
	manifests, err := manifest.List(AppsDir)
	if err != nil {
		return err
	}

	if len(manifests) == 0 {
		fmt.Println("No apps installed.")
		return nil
	}

	fmt.Printf("%-20s %-30s %s\n", "NAME", "BINARY", "LIBS")
	fmt.Println(strings.Repeat("-", 70))
	for _, m := range manifests {
		fmt.Printf("%-20s %-30s %d libs\n", m.Name, m.Binary, len(m.Libs))
	}
	return nil
}

func extractLibs(pkg repo.PkgInfo, pkgData []byte, soname, libsDir string) ([]string, error) {
	tmpDir, err := os.MkdirTemp("", "pkg-dl-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	pkgFile := filepath.Join(tmpDir, "pkg.tar.zst")
	if err := os.WriteFile(pkgFile, pkgData, 0644); err != nil {
		return nil, err
	}

	if err := verifyPkgFile(pkgFile, pkg); err != nil {
		return nil, err
	}

	extract := exec.Command("bsdtar", "-xf", pkgFile, "-C", tmpDir, "usr/lib/")
	extract.Stdout = os.Stdout
	extract.Stderr = os.Stderr
	if err := extract.Run(); err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}

	var results []string
	libDir := filepath.Join(tmpDir, "usr", "lib")
	entries, _ := os.ReadDir(libDir)
	sonameBase := strings.TrimSuffix(soname, filepath.Ext(soname))

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name != soname && !strings.HasPrefix(name, sonameBase) {
			continue
		}

		src := filepath.Join(libDir, name)
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}

		storePath, err := store.Store(name, data)
		if err != nil {
			continue
		}

		linkPath := filepath.Join(libsDir, name)
		store.Symlink(storePath, linkPath)
		results = append(results, name)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("package %s/%s %s contains no %s", pkg.Repo, pkg.Name, pkg.Version, soname)
	}

	return results, nil
}

func verifyPkgFile(pkgFile string, want repo.PkgInfo) error {
	out, err := exec.Command("bsdtar", "-xOf", pkgFile, ".PKGINFO").Output()
	if err != nil {
		return fmt.Errorf("read .PKGINFO: %w", err)
	}

	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		got[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}

	if got["pkgname"] != want.Name {
		return fmt.Errorf("version mismatch: expected pkg %s, got %s", want.Name, got["pkgname"])
	}
	if got["pkgver"] != want.Version {
		return fmt.Errorf("version mismatch: expected %s %s, got pkgver %s", want.Name, want.Version, got["pkgver"])
	}
	if got["arch"] != want.Arch {
		return fmt.Errorf("arch mismatch: expected %s, got %s", want.Arch, got["arch"])
	}
	return nil
}

func patchelfRPATH(binary, rpath string) error {
	cmd := exec.Command("patchelf", "--force-rpath", "--set-rpath", rpath, binary)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0755)
}
