package main

import (
	"fmt"
	"os"

	"probe/internal/install"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	force := false
	var pos []string
	for _, a := range args {
		switch a {
		case "--force", "-f":
			force = true
		default:
			pos = append(pos, a)
		}
	}

	switch cmd {
	case "install":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s install [--force] <elf-path> [name]\n", os.Args[0])
			os.Exit(1)
		}
		name := ""
		if len(pos) > 1 {
			name = pos[1]
		}
		if err := install.InstallElf(pos[0], name, install.InstallOptions{Force: force}); err != nil {
			fail(err)
		}
	case "reinstall":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s reinstall <name>\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.Reinstall(pos[0]); err != nil {
			fail(err)
		}
	case "upgrade":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s upgrade <name>\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.Upgrade(pos[0]); err != nil {
			fail(err)
		}
	case "install-pkg":
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s install-pkg <pkg-name>\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.InstallPkg(pos[0]); err != nil {
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
	fmt.Fprintf(os.Stderr, "  install [--force] <elf-path> [name]  Install an ELF with isolated libs\n")
	fmt.Fprintf(os.Stderr, "  reinstall <name>                     Reinstall from source, pinned to manifest versions\n")
	fmt.Fprintf(os.Stderr, "  upgrade <name>                       Reinstall from source, re-resolving to current versions\n")
	fmt.Fprintf(os.Stderr, "  install-pkg <pkg-name>               Install from Arch package name\n")
	fmt.Fprintf(os.Stderr, "  uninstall <name>                     Remove an installed app\n")
	fmt.Fprintf(os.Stderr, "  gc                                   Garbage-collect unused store entries\n")
	fmt.Fprintf(os.Stderr, "  list                                 List installed apps\n")
}
