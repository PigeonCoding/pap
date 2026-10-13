# pap

Per-app library isolation for repo packages.

Give it an Arch package name and it downloads that exact package, resolves
every non-host library recursively, vendors the `.so` files into a
content-addressed store, and patches `RPATH` to a private `libs/` folder.

State lives in `~/.pap` (`PAP_HOME` overrides it).

## Requirements

- libarchive 
- patchelf  # bsdtar + patchelf
- Go 1.27+ to build

## Usage

```sh
go build -o pap ./cmd/pap

pap install [options] <pkg> [name]
pap reinstall <name>   # re-fetch the pinned package version
pap upgrade <name>     # re-resolve to the newest compatible package version
pap add-path           # append ~/.local/bin to your shell rc
pap list | pap info <name> | pap remove <name> | pap gc | pap doctor | pap version
pap help [command]     # show help (run `pap help install` for all options)
```

`<pkg>` is a repo package name; `[name]` overrides the installed app name.
`--pkg-version` pins an exact pkgver, fetched from the Arch Linux Archive
when the mirrors no longer carry it (bypasses glibc auto-selection).
`pap install` is repo-only: there are
no file or folder inputs.

By default pap installs the newest build your host glibc can run: when the
live Arch build needs newer glibc than the host provides (common on
Debian/stable), it walks back through the Arch Linux Archive and installs
the newest compatible build, with dependencies resolved era-consistently.
`--no-glibc-compat` disables that and takes the live latest Arch build.
`--force-libc` instead vendors the Arch glibc + loader into the app and
rewrites `PT_INTERP`, so the live latest build runs even on old distros —
at the cost of a larger private `libs/` (shared blobs are deduplicated in
the content-addressed store). The choice is recorded in the manifest, so
reinstall/upgrade keep it.

`PAP_BIN_DIR` overrides the bin dir. `--dry-run` prints the resolve plan
without changing anything; `--force` swaps atomically via `.bak`.

## Example

```sh
$ pap install kitty
Installing kitty ... from extra/kitty (live latest)
...
Done. Run: /home/user/.local/bin/kitty

$ pap install --pkg-version 8.11.1-3 curl curl-old
Installing curl-old 8.11.1-3 from archive/curl
...
Done. Run: /home/user/.local/bin/curl-old
```

## How it works

- App payload (the repo package's `usr/` tree) unpacks under `exe/<name>/pkg/`.
- Soname → package via `%PROVIDES%` (`.files` index fallback), no `ldd`.
- Downloads are version-pinned and verified (`SHA256SUM` + `.PKGINFO` name/version/arch).
- `.so` files shipped by the app package win over repo downloads.
- `patchelf --force-rpath` sets an absolute `RPATH` to `apps/<name>/libs`
  (transitive deps need `RPATH`, not `RUNPATH`); vendored libs with their own
  `RUNPATH` are rewritten to `$ORIGIN`.
- `~/.local/bin/<name>` is a symlink to the real binary. Shell rc is never
  edited unless you run `pap add-path`.
- With `--force-libc`, the Arch `glibc` coupling (libc, libm, loader,
  `libnss_files/dns/compat`) is vendored like any other dependency and
  `PT_INTERP` is rewritten to the private loader.

```
~/.local/bin/<name>   →  ~/.pap/exe/<name>/… (on PATH)
~/.pap/apps/<name>/   libs/ (SONAME symlinks → store) + manifest.json
~/.pap/exe/<name>/    pkg/usr/bin/… (payload) + vendored resources
~/.pap/store/         content-addressed blobs (sha256), shared, GC-able
```

## Limitations

- glibc and the loader stay on the host (VERNEED-checked); every other
  soname must map to a package that ships it. On hosts older than Arch
  (e.g. Debian stable) installs default to the newest host-compatible
  build; only glibc-compatible closures install. `--force-libc` lifts this
  by vendoring Arch's glibc instead, with caveats: systemd NSS plugins
  (`libnss_systemd/resolve`) stay on the host, locales fall back to the
  built-in C.UTF-8, and the host kernel must satisfy the vendored glibc.
- `dlopen()` is partially covered: plugin dirs under `usr/lib/` are vendored,
  absolute-path `dlopen` is not.
- GUI stacks are out of scope for now.

## Security

- No signatures: checksums come from the mirror-served index (corruption-safe,
  not malicious-mirror-safe) — use mirrors you trust.
- No `ldd` (targets are never executed), setuid bits are dropped, concurrent
  runs are serialized with a `flock`.

## Development

```sh
go vet ./...
go test ./...
PAP_HOME=$(mktemp -d) go test ./...  # isolated state
```
