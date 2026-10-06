package main

import (
	"fmt"
	"os"

	"probe/internal/install"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <command> [args...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\ncommands:\n")
		fmt.Fprintf(os.Stderr, "  install <elf-path> [name]  Install an ELF with isolated libs\n")
		fmt.Fprintf(os.Stderr, "  install-pkg <pkg-name>     Install from Arch package name\n")
		fmt.Fprintf(os.Stderr, "  uninstall <name>           Remove an installed app\n")
		fmt.Fprintf(os.Stderr, "  gc                         Garbage-collect unused store entries\n")
		fmt.Fprintf(os.Stderr, "  list                       List installed apps\n")
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "install":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s install <elf-path> [name]\n", os.Args[0])
			os.Exit(1)
		}
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		if err := install.InstallElf(args[0], name); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "install-pkg":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s install-pkg <pkg-name>\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.InstallPkg(args[0]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "uninstall":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s uninstall <name>\n", os.Args[0])
			os.Exit(1)
		}
		if err := install.Uninstall(args[0]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "gc":
		if err := install.GC(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "list":
		if err := install.List(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(1)
	}
}
