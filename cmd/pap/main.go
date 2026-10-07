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
	exe := ""
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--exe" {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "--exe requires a value: --exe <path relative to the folder>\n")
				usage()
				os.Exit(1)
			}
			exe = args[i]
			continue
		}
		if v, ok := strings.CutPrefix(a, "--exe="); ok {
			if v == "" {
				fmt.Fprintf(os.Stderr, "--exe requires a value: --exe <path relative to the folder>\n")
				usage()
				os.Exit(1)
			}
			exe = v
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
	if exe != "" && cmd != "install" {
		fmt.Fprintf(os.Stderr, "--exe only applies to install (reinstall reuses the stored entrypoint)\n")
		usage()
		os.Exit(1)
	}
	opts := func() install.InstallOptions {
		return install.InstallOptions{Force: force, DryRun: dryRun, Quiet: quiet, AddPath: addPath, Exe: exe}
	}

	switch cmd {
	case "install":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s install [--force] [--dry-run] [--quiet] [--add-path] [--exe <rel>] <path> [name]\n", os.Args[0])
			os.Exit(1)
		}
		pathArg, name, swapped, err := install.SplitInstallArgs(pos)
		if err != nil {
			fmt.Fprintf(os.Stderr, "usage: %s install [--force] [--dry-run] [--quiet] [--add-path] [--exe <rel>] <path> [name]\n", os.Args[0])
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if swapped {
			fmt.Fprintf(os.Stderr, "note: swapped arguments to <path> [name]\n")
		}
		o := opts()
		if err := install.InstallElf(pathArg, name, o); err != nil {
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

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s <command> [args...]\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\ncommands:\n")
	fmt.Fprintf(os.Stderr, "  install [--force] [--dry-run] [--quiet] [--add-path] [--exe <rel>] <path> [name]  Install an ELF, script, or app folder with isolated libs\n")
	fmt.Fprintf(os.Stderr, "      <path> is an ELF binary, a shebang script, or a folder; the folder's\n")
	fmt.Fprintf(os.Stderr, "      main executable is <folder>/<name> (or <folder>/bin/<name>), ELF or\n")
	fmt.Fprintf(os.Stderr, "      script — launcher scripts win ties — and every bundled ELF is patched.\n")
	fmt.Fprintf(os.Stderr, "      --exe <rel> pins the entrypoint explicitly (path relative to the folder).\n")
	fmt.Fprintf(os.Stderr, "  reinstall <name>                     Reinstall from source, pinned to manifest versions\n")
	fmt.Fprintf(os.Stderr, "  upgrade <name>                       Reinstall from source, re-resolving to current versions\n")
	fmt.Fprintf(os.Stderr, "  uninstall <name>                     Remove an installed app (only removes binaries we placed)\n")
	fmt.Fprintf(os.Stderr, "  gc                                   Garbage-collect unused store entries\n")
	fmt.Fprintf(os.Stderr, "  list                                 List installed apps\n")
	fmt.Fprintf(os.Stderr, "  info <name>                          Show app manifest, source and pinned versions\n")
	fmt.Fprintf(os.Stderr, "  doctor                               Check bsdtar/patchelf and state dirs\n")
	fmt.Fprintf(os.Stderr, "  version                              Print version\n")
	fmt.Fprintf(os.Stderr, "\nflags (install/reinstall/upgrade): --force/-f, --dry-run/-n, --quiet/-q, --add-path\n")
	fmt.Fprintf(os.Stderr, "env: PAP_HOME overrides ~/.pap (isolation), PAP_BIN_DIR overrides bin dir\n")
}
