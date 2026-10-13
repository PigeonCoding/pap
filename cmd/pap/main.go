package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// Help never parses flags: `pap help [command]`, `pap --help [command]`.
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		topic := ""
		if len(os.Args) > 2 && !strings.HasPrefix(os.Args[2], "-") {
			topic = os.Args[2]
		}
		if topic == "" {
			printUsage(os.Stdout)
			return
		}
		if !printCommandHelp(os.Stdout, topic) {
			fmt.Fprintf(os.Stderr, "unknown help topic: %s\n", topic)
			os.Exit(1)
		}
		return
	}
	args := os.Args[2:]

	force := false
	dryRun := false
	quiet := false
	archiveDate := ""
	noArchive := false
	noGlibcCompat := false
	glibcCompatFlag := false
	forceLibc := false
	pkgVersion := ""
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--archive-date" {
			i++
			archiveDate = requireFlagValue(args, i, "--archive-date", "--archive-date YYYY-MM-DD")
			continue
		}
		if v, ok := strings.CutPrefix(a, "--archive-date="); ok {
			archiveDate = requireInlineValue(v, "--archive-date", "--archive-date YYYY-MM-DD")
			continue
		}
		if a == "--pkg-version" {
			i++
			pkgVersion = requireFlagValue(args, i, "--pkg-version", "--pkg-version <pkgver>")
			continue
		}
		if v, ok := strings.CutPrefix(a, "--pkg-version="); ok {
			pkgVersion = requireInlineValue(v, "--pkg-version", "--pkg-version <pkgver>")
			continue
		}
		switch a {
		case "--force", "-f":
			force = true
		case "--dry-run", "-n":
			dryRun = true
		case "--quiet", "-q":
			quiet = true
		case "--no-archive":
			noArchive = true
		case "--glibc-compat":
			glibcCompatFlag = true
		case "--no-glibc-compat":
			noGlibcCompat = true
		case "--force-libc":
			forceLibc = true
		case "--help", "-h":
			if !printCommandHelp(os.Stdout, cmd) {
				fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
				os.Exit(1)
			}
			return
		case "--add-path":
			fmt.Fprintf(os.Stderr, "--add-path is a command now: %s add-path\n", os.Args[0])
			os.Exit(1)
		case "--version", "-V":
			fmt.Printf("pap %s (arch %s)\n", install.Version, config.Arch)
			return
		default:
			if len(a) > 0 && a[0] == '-' {
				fmt.Fprintf(os.Stderr, "unknown flag: %s (run `pap help %s`)\n", a, cmd)
				os.Exit(1)
			}
			pos = append(pos, a)
		}
	}
	if archiveDate != "" && cmd != "install" && cmd != "upgrade" {
		fmt.Fprintf(os.Stderr, "--archive-date only applies to install/upgrade (reinstall stays pinned)\n")
		os.Exit(1)
	}
	if glibcCompatFlag && noGlibcCompat {
		fmt.Fprintf(os.Stderr, "cannot combine --glibc-compat with --no-glibc-compat\n")
		os.Exit(1)
	}
	if (glibcCompatFlag || noGlibcCompat || forceLibc) && cmd != "install" && cmd != "upgrade" {
		fmt.Fprintf(os.Stderr, "--glibc-compat/--no-glibc-compat/--force-libc only apply to install/upgrade (reinstall carries the manifest setting)\n")
		os.Exit(1)
	}
	opts := func() install.InstallOptions {
		return install.InstallOptions{Force: force, DryRun: dryRun, Quiet: quiet, ArchiveDate: archiveDate, NoArchive: noArchive, NoGlibcCompat: noGlibcCompat, ForceLibc: forceLibc}
	}

	switch cmd {
	case "install":
		if len(pos) < 1 || len(pos) > 2 {
			fmt.Fprintf(os.Stderr, "usage: %s install [options] <pkg> [name] (run `pap help install`)\n", os.Args[0])
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
			fmt.Fprintf(os.Stderr, "usage: %s reinstall <name> (run `pap help reinstall`)\n", os.Args[0])
			os.Exit(1)
		}
		o := opts()
		o.Force = true
		if err := install.ReinstallWithOptions(pos[0], o); err != nil {
			fail(err)
		}
	case "upgrade":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s upgrade [options] <name> (run `pap help upgrade`)\n", os.Args[0])
			os.Exit(1)
		}
		o := opts()
		o.Force = true
		if err := install.UpgradeWithOptions(pos[0], o); err != nil {
			fail(err)
		}
	case "remove", "uninstall":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s remove <name>\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.Uninstall(pos[0]); err != nil {
			fail(err)
		}
	case "add-path":
		if len(pos) > 0 {
			fmt.Fprintf(os.Stderr, "usage: %s add-path\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.AddPath(); err != nil {
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

// requireFlagValue returns args[idx] for a space-separated flag, exiting when
// missing or flag-shaped.
func requireFlagValue(args []string, idx int, flag, hint string) string {
	if idx >= len(args) || strings.HasPrefix(args[idx], "-") {
		fmt.Fprintf(os.Stderr, "%s requires a value: %s (run `pap help install`)\n", flag, hint)
		os.Exit(1)
	}
	return args[idx]
}

// requireInlineValue validates a --flag=value form, exiting on empty.
func requireInlineValue(v, flag, hint string) string {
	if v == "" {
		fmt.Fprintf(os.Stderr, "%s requires a value: %s (run `pap help install`)\n", flag, hint)
		os.Exit(1)
	}
	return v
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
	printUsage(os.Stderr)
}

// prog is the invoked program name for help screens (argv[0] under
// `go run` is a long build path nobody wants to read).
func prog() string {
	if b := filepath.Base(os.Args[0]); b != "" {
		return b
	}
	return os.Args[0]
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: %s <command> [options] [args]\n", prog())
	fmt.Fprintf(w, "\ncommands:\n")
	fmt.Fprintf(w, "  install <pkg> [name]   Install an Arch repo package with isolated libs\n")
	fmt.Fprintf(w, "  reinstall <name>       Re-fetch the pinned package version\n")
	fmt.Fprintf(w, "  upgrade <name>         Re-resolve to the newest compatible version\n")
	fmt.Fprintf(w, "  remove <name>           Remove an installed app (alias: uninstall)\n")
	fmt.Fprintf(w, "  list                   List installed apps\n")
	fmt.Fprintf(w, "  info <name>            Show app manifest, source and pinned versions\n")
	fmt.Fprintf(w, "  gc                     Garbage-collect unused store entries\n")
	fmt.Fprintf(w, "  add-path               Append the bin dir to your shell rc\n")
	fmt.Fprintf(w, "  doctor                 Check tools, dirs and host glibc\n")
	fmt.Fprintf(w, "  version                Print version\n")
	fmt.Fprintf(w, "  help [command]         Show help\n")
	fmt.Fprintf(w, "\noptions (install, upgrade):\n")
	fmt.Fprintf(w, "  -f, --force            Overwrite an existing install\n")
	fmt.Fprintf(w, "  -n, --dry-run          Print the resolve plan without changing anything\n")
	fmt.Fprintf(w, "  -q, --quiet            Suppress progress output\n")
	fmt.Fprintf(w, "      --pkg-version VER  Pin an exact package version (Archive fallback)\n")
	fmt.Fprintf(w, "      --archive-date DATE\n")
	fmt.Fprintf(w, "                         Resolve missing libs from the Archive snapshot of DATE\n")
	fmt.Fprintf(w, "      --no-archive       Live repos only, no Archive fallback\n")
	fmt.Fprintf(w, "      --no-glibc-compat  Take live latest even if host glibc is too old\n")
	fmt.Fprintf(w, "      --force-libc       Vendor Arch glibc + loader, rewrite PT_INTERP\n")
	fmt.Fprintf(w, "\nrun `%s help <command>` for details.\n", prog())
	fmt.Fprintf(w, "env: PAP_HOME overrides ~/.pap (isolation), PAP_BIN_DIR overrides bin dir\n")
}

// printCommandHelp writes detail for one command. It reports false for
// unknown topics so callers can fail instead of printing blindly.
func printCommandHelp(w io.Writer, cmd string) bool {
	switch cmd {
	case "install":
		fmt.Fprintf(w, "usage: %s install [options] <pkg> [name]\n", prog())
		fmt.Fprintf(w, "\nInstall an Arch repo package with isolated libraries.\n")
		fmt.Fprintf(w, "\n<pkg> is a repo package name; [name] overrides the installed app name.\n")
		fmt.Fprintf(w, "\nBy default pap installs the newest build your host glibc can run,\n")
		fmt.Fprintf(w, "auto-downgrading through the Arch Linux Archive on older distros.\n")
		fmt.Fprintf(w, "\n  --pkg-version VER    Pin an exact version (bypasses glibc auto-selection;\n")
		fmt.Fprintf(w, "                       fetched from the Archive when mirrors no longer carry it)\n")
		fmt.Fprintf(w, "  --archive-date DATE  Resolve missing libs from the Archive snapshot of DATE\n")
		fmt.Fprintf(w, "                       (default: app binary mtime)\n")
		fmt.Fprintf(w, "  --no-archive         Live repos only\n")
		fmt.Fprintf(w, "  --no-glibc-compat    Take live latest even if host glibc is too old\n")
		fmt.Fprintf(w, "  --force-libc         Vendor Arch glibc + loader into the app and rewrite\n")
		fmt.Fprintf(w, "                       PT_INTERP, so live latest runs even on old distros\n")
		fmt.Fprintf(w, "  -f, --force          Overwrite an existing install of the same name\n")
		fmt.Fprintf(w, "  -n, --dry-run        Print the resolve plan without changing anything\n")
		fmt.Fprintf(w, "  -q, --quiet          Suppress progress output\n")
	case "upgrade":
		fmt.Fprintf(w, "usage: %s upgrade [options] <name>\n", prog())
		fmt.Fprintf(w, "\nReinstall <name> from source, re-resolving to the newest compatible\n")
		fmt.Fprintf(w, "version. Accepts the same options as install; a --force-libc install\n")
		fmt.Fprintf(w, "stays vendored unless reinstalled fresh.\n")
	case "reinstall":
		fmt.Fprintf(w, "usage: %s reinstall <name>\n", prog())
		fmt.Fprintf(w, "\nRe-fetch the exact package versions pinned in the manifest.\n")
		fmt.Fprintf(w, "Stays pinned: glibc/archived-version options do not apply.\n")
	case "remove", "uninstall":
		fmt.Fprintf(w, "usage: %s remove <name>\n", prog())
		fmt.Fprintf(w, "\nRemove an installed app (only removes binaries pap placed).\n")
	case "info":
		fmt.Fprintf(w, "usage: %s info <name>\n", prog())
		fmt.Fprintf(w, "\nShow app manifest, source and pinned versions.\n")
	case "list":
		fmt.Fprintf(w, "usage: %s list\n", prog())
		fmt.Fprintf(w, "\nList installed apps.\n")
	case "gc":
		fmt.Fprintf(w, "usage: %s gc\n", prog())
		fmt.Fprintf(w, "\nGarbage-collect unused store entries.\n")
	case "add-path":
		fmt.Fprintf(w, "usage: %s add-path\n", prog())
		fmt.Fprintf(w, "\nAppend the bin dir to your shell rc so it stays on PATH.\n")
	case "doctor":
		fmt.Fprintf(w, "usage: %s doctor\n", prog())
		fmt.Fprintf(w, "\nCheck bsdtar/patchelf, state dirs and host glibc resolution.\n")
	case "version":
		fmt.Fprintf(w, "usage: %s version\n", prog())
		fmt.Fprintf(w, "\nPrint version.\n")
	default:
		return false
	}
	return true
}
