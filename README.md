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

pap install [--force] [--dry-run] [--quiet] [--pkg-version <ver>] <pkg> [name]
pap reinstall <name>   # re-fetch the pinned package version
pap upgrade <name>     # re-resolve to the live latest package version
pap add-path           # append ~/.local/bin to your shell rc
pap list | pap info <name> | pap uninstall <name> | pap gc | pap doctor | pap version
```

`<pkg>` is a repo package name; `[name]` overrides the installed app name.
`--pkg-version` pins an exact pkgver, fetched from the Arch Linux Archive
when the mirrors no longer carry it. `pap install` is repo-only: there are
no file or folder inputs.

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

```
~/.local/bin/<name>   →  ~/.pap/exe/<name>/… (on PATH)
~/.pap/apps/<name>/   libs/ (SONAME symlinks → store) + manifest.json
~/.pap/exe/<name>/    pkg/usr/bin/… (payload) + vendored resources
~/.pap/store/         content-addressed blobs (sha256), shared, GC-able
```

## Limitations

- glibc and the loader stay on the host (VERNEED-checked); every other
  soname must map to a package that ships it.
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
