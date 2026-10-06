# pkg

Per-app library isolation for ELF binaries on Arch Linux.

Give it any ELF file: it resolves every non-host library the binary needs
(recursively), downloads the exact packages that provide them straight from the
Arch repo mirrors, vendors the `.so` files into a content-addressed store, and
patches the binary's `RPATH` to a private `libs/` folder. Library overlap
between apps is solved by deduplication; nothing touches `pacman` or the system
package database.

## How it works

```
input ELF
  │  readelf-equivalent: DT_NEEDED (recursive, BFS)
  ▼
sonames ──► repo index (.db: core/extra/multilib/chaotic-aur, %PROVIDES% → soname map;
             undeclared sonames fall back to the .files index)
  ▼
exact pkg name-version-arch ──► download from mirror (version-pinned URL)
  ▼
verify .PKGINFO (pkgname/pkgver/arch) ──► extract usr/lib/*
  ▼
store/<sha256>-<file>  ◄──symlinks──  apps/<name>/libs/
  ▼
patchelf --force-rpath '$ORIGIN/libs'
```

- **Resolution** uses Arch's own mechanism: packages declare
  `provides=(libcurl.so=4-64)`, which maps 1:1 to the `SONAME` the ELF asks
  for. No `ldd` (it executes code), no `pacman -F`.
- **Repositories**: official Arch repos (`core`, `extra`, `multilib`) plus
  `chaotic-aur` only — third-party repos from `pacman.conf` (cachyos,
  lizardbyte, ...) are ignored. Servers come from `~/.pap/mirrorlist` when it
  exists (a pkg-only override for the official repos; chaotic-aur keeps its own
  mirrors), else `pacman.conf` / the system mirrorlist; each repo's `.db` is
  cached with a TTL and downloads are pinned to the mirror that served the
  index.
- **Version pinning + checksum**: the download URL is built from the exact
  `%FILENAME%` in the repo `.db` and the payload is verified against the
  index's `SHA256SUM`. Before anything is extracted, the archive's `.PKGINFO`
  is checked against the expected pkgname/pkgver/arch — a stale mirror serving
  a different file fails the install instead of silently installing the wrong
  lib.
- **Symbol version check**: the binary's `.gnu.version_r` (VERNEED) and every
  extracted lib's are verified against the provider's `.gnu.version_d`
  (VERDEF). If the ELF needs `GLIBC_2.43` and the host only has `GLIBC_2.40`,
  you get a clear error before install completes.
- **`DT_RPATH`, not `RUNPATH`**: glibc does not inherit `RUNPATH` into
  transitive dependency lookups, but it does walk `DT_RPATH` up the loading
  chain. `patchelf --force-rpath` is what makes all N levels load from
  `libs/` — with plain `--set-rpath` only direct dependencies are isolated.
- **`~/.local/bin`**: the patched binary is installed there (the tool makes
  sure that directory is on PATH, adding an export to your shell rc if
  nothing mentions it), and its RPATH points at the app's own
  `~/.pap/apps/<name>/libs` folder.

## Layout

```
~/.local/bin/<name>        the patched binary (on PATH)

$HOME/.pap/
├── apps/<name>/
│   ├── libs/             symlinks named by SONAME → store (per-app, saves space)
│   └── manifest.json     libs + locked package versions (repo/name: version)
├── store/
│   └── <sha256>          content-addressed (sha256 of file bytes), shared, GC-able
└── mirrorlist            optional pkg-only mirrors for core/extra/multilib
```

## Usage

```sh
go build -o pkg ./cmd/pkg

pkg install [--force] <elf> [name]  # isolate an arbitrary ELF
pkg reinstall <name>                 # reinstall from source, pinned to manifest versions
pkg upgrade <name>                   # reinstall, re-resolving to current repo versions
pkg list                             # installed apps
pkg uninstall <name>                 # remove app: binary from ~/.local/bin + app dir
pkg gc                               # drop store entries no app references
```

Example:

```console
$ pkg install /usr/bin/curl curl
Installing curl from /usr/bin/curl

Resolving dependencies (recursive)...
  [ ok ] libcurl.so.4 -> core/curl 8.22.0-1
  [ ok ] libnghttp2.so.14 -> core/libnghttp2 1.70.0-1
  ... 22 transitive libs ...
Checking dependency compatibility...

Done. Run: /home/you/.local/bin/curl
```

After install, `ldd ~/.local/bin/curl` shows every non-glibc lib resolving
into `~/.pap/apps/curl/libs/`.

## Scope / limitations

- **glibc and the ELF loader are not isolated** — the host provides
  `libc.so.6`, `libm`, `libpthread`, `ld-linux`, etc. The VERNEED check is the
  safety net: host glibc must satisfy the binary's symbol requirements.
- **Every soname must map to a package that ships it** — packages declaring
  `.so=` provides are preferred; sonames no package declares (e.g.
  `libpython3.14.so.1.0`, which `python` ships without a provide) are looked up
  in the repos' file indexes.
- **`dlopen()` is partially covered** — plugin directories shipped under
  `usr/lib/` (ossl-modules, gconv, krb5 plugins, ...) are vendored with their
  relative layout, but absolute-path `dlopen` calls and plugins outside
  `usr/lib` are not rewritten and won't be found.
- **GUI stacks are out of scope** for now.
- `install-pkg` (install straight from a repo package name) is not implemented
  yet.
- All state lives under `$HOME/.pap` — `apps/` and `store/` are created on
  first install (`gc`/`list` work from there too).

## Internals

| Package             | Role                                                        |
|---------------------|-------------------------------------------------------------|
| `internal/elf`      | `DT_NEEDED`, VERNEED/VERDEF parsing (`debug/elf`)           |
| `internal/repo`     | pacman.conf/mirrorlist servers, gzip+zstd `.db` parse, sha256-pinned downloads |
| `internal/config`   | shared `~/.pap` paths + host architecture                   |
| `internal/resolver` | soname → package, host-lib skip list                        |
| `internal/store`    | content-addressed store, symlinks, GC                       |
| `internal/install`  | BFS install flow, `.PKGINFO` verify, compat check, patchelf |
| `internal/manifest` | per-app manifest + locked package versions                  |

## Development

```sh
go vet ./...
go test ./...
```
