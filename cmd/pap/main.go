package main

import (
	"fmt"
	"os"
	"strings"

	"pap/internal/config"
	"pap/internal/install"
)

func main() {
	config.Refresh()
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	force := false
	dryRun := false
	quiet := false
	addPath := false
	archiveDate := ""
	noArchive := false
	pkgVersion := ""
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--archive-date" {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "--archive-date requires a value: --archive-date YYYY-MM-DD\n")
				usage()
				os.Exit(1)
			}
			archiveDate = args[i]
			continue
		}
		if v, ok := strings.CutPrefix(a, "--archive-date="); ok {
			if v == "" {
				fmt.Fprintf(os.Stderr, "--archive-date requires a value: --archive-date YYYY-MM-DD\n")
				usage()
				os.Exit(1)
			}
			archiveDate = v
			continue
		}
		if a == "--pkg-version" {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "--pkg-version requires a value: --pkg-version <pkgver>\n")
				usage()
				os.Exit(1)
			}
			pkgVersion = args[i]
			continue
		}
		if v, ok := strings.CutPrefix(a, "--pkg-version="); ok {
			if v == "" {
				fmt.Fprintf(os.Stderr, "--pkg-version requires a value: --pkg-version <pkgver>\n")
				usage()
				os.Exit(1)
			}
			pkgVersion = v
			continue
		}
		switch a {
		case "--force", "-f":
			force = true
		case "--dry-run", "-n":
			dryRun = true
		case "--quiet", "-q":
			quiet = true
		case "--add-path":
			addPath = true
		case "--no-archive":
			noArchive = true
		case "--version", "-V":
			fmt.Printf("pap %s (arch %s)\n", install.Version, config.Arch)
			return
		default:
			if len(a) > 0 && a[0] == '-' {
				fmt.Fprintf(os.Stderr, "unknown flag: %s\n", a)
				usage()
				os.Exit(1)
			}
			pos = append(pos, a)
		}
	}
	if archiveDate != "" && cmd != "install" && cmd != "upgrade" {
		fmt.Fprintf(os.Stderr, "--archive-date only applies to install/upgrade (reinstall stays pinned)\n")
		usage()
		os.Exit(1)
	}
	opts := func() install.InstallOptions {
		return install.InstallOptions{Force: force, DryRun: dryRun, Quiet: quiet, AddPath: addPath, ArchiveDate: archiveDate, NoArchive: noArchive}
	}

	switch cmd {
	case "install":
		if len(pos) < 1 || len(pos) > 2 {
			fmt.Fprintf(os.Stderr, "usage: %s install [--force] [--dry-run] [--quiet] [--add-path] [--pkg-version <ver>] [--archive-date YYYY-MM-DD] [--no-archive] <pkg> [name]\n", os.Args[0])
			os.Exit(1)
		}
		if err := validPkgName(pos[0]); err != nil {
			fmt.Fprintf(os.Stderr, "invalid package name %q\n", pos[0])
			os.Exit(1)
		}
		name := ""
		if len(pos) == 2 {
			name = pos[1]
		}
		o := opts()
		if err := install.InstallRepoApp(pos[0], pkgVersion, name, o); err != nil {
			fail(err)
		}
	case "reinstall":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s reinstall <name>\n", os.Args[0])
			os.Exit(1)
		}
		o := opts()
		o.Force = true
		if err := install.ReinstallWithOptions(pos[0], o); err != nil {
			fail(err)
		}
	case "upgrade":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s upgrade <name>\n", os.Args[0])
			os.Exit(1)
		}
		o := opts()
		o.Force = true
		if err := install.UpgradeWithOptions(pos[0], o); err != nil {
			fail(err)
		}
	case "uninstall":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s uninstall <name>\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.Uninstall(pos[0]); err != nil {
			fail(err)
		}
	case "gc":
		if err := install.GC(); err != nil {
			fail(err)
		}
	case "list":
		if err := install.List(); err != nil {
			fail(err)
		}
	case "info":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s info <name>\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.Info(pos[0]); err != nil {
			fail(err)
		}
	case "doctor":
		if err := install.Doctor(); err != nil {
			fail(err)
		}
	case "--version", "-V", "version":
		fmt.Printf("pap %s %s\n", install.Version, config.Arch)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

// validPkgName rejects empty names and path/flag-shaped input for the
// install positional (repo package names are plain `[a-z0-9@._+-]` words).
func validPkgName(pkg string) error {
	if pkg == "" {
		return fmt.Errorf("empty package name")
	}
	if strings.HasPrefix(pkg, "-") || strings.ContainsAny(pkg, "/ \t\n") {
		return fmt.Errorf("invalid package name %q", pkg)
	}
	return nil
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s <command> [args...]\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\ncommands:\n")
	fmt.Fprintf(os.Stderr, "  install [--force] [--dry-run] [--quiet] [--add-path] [--pkg-version <ver>] [--archive-date YYYY-MM-DD] [--no-archive] <pkg> [name]  Install an app package from the Arch repos with isolated libs\n")
	fmt.Fprintf(os.Stderr, "      <pkg> is a repo package name; [name] overrides the installed app name.\n")
	fmt.Fprintf(os.Stderr, "      --pkg-version <ver> pins an exact pkgver, fetched from the Arch Linux\n")
	fmt.Fprintf(os.Stderr, "        Archive when the mirrors no longer carry it.\n")
	fmt.Fprintf(os.Stderr, "      --archive-date YYYY-MM-DD resolves missing sonames from the Arch Linux\n")
	fmt.Fprintf(os.Stderr, "        Archive snapshot of that date instead of latest (default: app binary mtime).\n")
	fmt.Fprintf(os.Stderr, "      --no-archive disables the Archive fallback (live repos only).\n")
	fmt.Fprintf(os.Stderr, "  reinstall <name>                     Reinstall from source, pinned to manifest versions\n")
	fmt.Fprintf(os.Stderr, "  upgrade <name>                       Reinstall from source, re-resolving to current versions\n")
	fmt.Fprintf(os.Stderr, "  uninstall <name>                     Remove an installed app (only removes binaries we placed)\n")
	fmt.Fprintf(os.Stderr, "  gc                                   Garbage-collect unused store entries\n")
	fmt.Fprintf(os.Stderr, "  list                                 List installed apps\n")
	fmt.Fprintf(os.Stderr, "  info <name>                          Show app manifest, source and pinned versions\n")
	fmt.Fprintf(os.Stderr, "  doctor                               Check bsdtar/patchelf and state dirs\n")
	fmt.Fprintf(os.Stderr, "  version                              Print version\n")
	fmt.Fprintf(os.Stderr, "\nflags (install/reinstall/upgrade): --force/-f, --dry-run/-n, --quiet/-q, --add-path, --pkg-version, --archive-date, --no-archive\n")
	fmt.Fprintf(os.Stderr, "env: PAP_HOME overrides ~/.pap (isolation), PAP_BIN_DIR overrides bin dir\n")
}
