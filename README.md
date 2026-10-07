# pap

Per-app library isolation for ELF binaries on Arch Linux.

Give it any ELF file: it resolves every non-host library the binary needs
(recursively), downloads the exact packages that provide them straight from the
Arch repo mirrors, vendors the `.so` files into a content-addressed store, and
patches the binary's `RPATH` to a private `libs/` folder. Library overlap
between apps is solved by deduplication; nothing touches `pacman` or the system
package database.

Naming is unified on `pap`: module `pap`, binary `pap`, state `~/.pap`
(`PAP_HOME` overrides the state dir for isolation/tests).

## Requirements

- Arch Linux (pacman repos: `core`, `extra`, `multilib`, `chaotic-aur`)
- `bsdtar` (`libarchive`) — package extraction and index handling
- `patchelf` — RPATH rewriting (with an `LD_LIBRARY_PATH` launcher fallback)
- Go 1.27+ (to build)

```sh
sudo pacman -S libarchive patchelf
```

## How it works

```
input path: ELF file, shebang script, or app folder
  │  files/scripts pass through; folders resolve their main executable
  │  (<folder>/<name>, or the single top-level ELF) and are copied verbatim
  ▼
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
store/<sha256>  ◄──symlinks──  apps/<name>/libs/
  ▼
binary: patchelf RPATH to absolute ~/.pap/apps/<name>/libs (+ orig RPATH kept)
vendored libs with own RUNPATH: patchelf --force-rpath '$ORIGIN'
  ▼
~/.pap/exe/<name>/…  ◄──symlink──  ~/.local/bin/<name>
```

- **Resolution** uses Arch's own mechanism: packages declare
  `provides=(libcurl.so=4-64)`, which maps 1:1 to the `SONAME` the ELF asks
  for. No `ldd` (it executes code), no `pacman -F`.
- **Bundled libraries win**: `.so` files shipped next to the source (bundle
  `lib/` dirs, the binary's own `$ORIGIN` RPATH entries, folder payloads)
  are staged as-is and never re-downloaded — the bundle is self-consistent
  (upstream kitty ships a patched libpython with private symbols the repo
  build lacks). Repo downloads are the fallback; a corrupt bundled copy
  falls back to the repo too.
- **Repositories**: official Arch repos (`core`, `extra`, `multilib`) plus
  `chaotic-aur` only — third-party repos from `pacman.conf` (cachyos,
  lizardbyte, ...) are ignored. Servers come from `~/.pap/mirrorlist` when it
  exists (a pap-only override for the official repos; chaotic-aur keeps its own
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
  (VERDEF). If the ELF needs a newer glibc than the host provides,
  you get a clear error before install completes.
- **`DT_RPATH`, not `RUNPATH`**: glibc does not inherit `RUNPATH` into
  transitive dependency lookups, but it does walk `DT_RPATH` up the loading
  chain. `patchelf --force-rpath` is what makes all N levels load from
  `libs/` — with plain `--set-rpath` only direct dependencies are isolated.
  The installed binary gets an **absolute** RPATH to its
  app dir; vendored libs with their own RUNPATH are rewritten to `$ORIGIN`
  so they stay inside `libs/`.
- **`~/.local/bin` holds symlinks**: the real binary lives under
  `~/.pap/exe/<name>/` and is linked into `~/.local/bin`. The kernel resolves
  the symlink for `/proc/self/exe`, so exe-relative lookups behave exactly as
  on PATH — with no launcher process and `argv[0]` untouched. `pap` never edits
  your shell rc unless you pass `--add-path`.

## Layout

```
~/.local/bin/<name>        symlink → ~/.pap/exe/<name>/… (on PATH)

$HOME/.pap/
├── apps/<name>/
│   ├── libs/             symlinks named by SONAME → store (per-app, saves space)
│   └── manifest.json     libs + locked package versions (repo/name: version) + source sha256
├── exe/<name>/
│   ├── bin/<name>        the patched binary (single-file installs; PATH symlink target)
│   ├── pkg/…             folder installs: source tree copied verbatim (symlink target inside)
│   └── lib/… share/…    vendored exe-relative resource trees (e.g. lib/kitty)
├── store/
│   └── <sha256>          content-addressed (sha256 of file bytes), shared, GC-able
├── pkgcache/             downloaded packages keyed by %FILENAME%
├── cache/repo/           repo .db/.files indexes (XDG ~/.cache/pap outside PAP_HOME)
├── mirrorlist            optional pap-only mirrors for core/extra/multilib
└── lock                  cross-process flock
```

## Usage

```sh
go build -o pap ./cmd/pap

pap install [--force] [--dry-run] [--quiet] [--add-path] [--exe <rel>] <path> [name]  # isolate an ELF, script, or app folder
# --exe pins the folder entrypoint explicitly (path relative to the folder);
# otherwise <folder>/<name> (or <folder>/bin/<name>) wins, scripts beat ELFs
pap reinstall <name>                 # reinstall from source, pinned to manifest versions
pap upgrade <name>                   # reinstall, re-resolving to current repo versions
pap list                             # installed apps
pap info <name>                      # manifest, source, pinned versions
pap uninstall <name>                 # remove app (only removes binaries pap placed) + GC
pap gc                               # drop store entries no app references
pap doctor                           # check bsdtar/patchelf + state dirs
pap version                          # print version
```

Example:

```console
$ pap install /usr/bin/curl curl
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

Env: `PAP_HOME` overrides `~/.pap` (isolation), `PAP_BIN_DIR` overrides the
bin dir. `pap install --dry-run` prints the resolve plan without changing
anything; `--force` swaps atomically via `.bak` so a failed overwrite never
destroys the previous install.

## Scope / limitations

- **glibc and the ELF loader are not isolated** — the host provides
  `libc.so.6`, `libm`, `libpthread`, `ld-linux`, etc. The VERNEED check is the
  safety net: host glibc must satisfy the binary's symbol requirements.
- **Every soname must map to a package that ships it** — packages declaring
  `.so=` provides are preferred; sonames no package declares (e.g.
  `libpython3.14.so.1.0`, which `python` ships without a provide) are looked up
  in the repos' file indexes.
- **`dlopen()` is partially covered** — plugin directories shipped under
  `usr/lib/` (ossl-modules, gconv, NSS, ...) are vendored with their
  relative layout, but absolute-path `dlopen` calls and plugins outside
  `usr/lib` are not rewritten and won't be found.
- **Exe-relative resources are vendored** — binaries that find data via
  `../lib/<app>` / `../share/<app>` relative to their own path (e.g. kitty's
  `/usr/lib/kitty`) get the host tree copied next to the installed copy
  (`exe/<name>/lib/...`), their bundled extensions' `.so` deps resolved too.
  Sibling trees composed at runtime (`%s/%s/kitty-extensions`) are included
  when the binary mentions their name (never inside system lib dirs).
  Companion executables next to the main binary (kitty's `kitten`) are
  vendored the same way — matched by name against the binary plus vendored
  resource contents — so runtime `exec` of siblings keeps working.
  App folders (`pap install ./myapp`) are copied verbatim under `exe/<name>/pkg/`.
- **GUI stacks are out of scope** for now.
- All state lives under `$HOME/.pap` — `apps/` and `store/` are created on
  first install (`gc`/`list` work from there too).

## Threat model

- `SHA256SUM` comes from the **mirror-served index**, not a signed
  database: it protects against accidental corruption / stale-mirror mixes
  (plus the `.PKGINFO` name/version/arch check), but a malicious mirror
  serving a consistent evil index + evil package would pass. `pacman` solves
  this with GPG `SigLevel`/keyring verification of `.db.sig` + `.pkg.sig`;
  `pap` does not verify signatures yet — use mirrors you trust for `pacman`
  itself. Full `.sig`-against-`/etc/pacman.d/gnupg` verification is future
  work.
- Repo indexes live under `~/.cache/pap` (or `$PAP_HOME/cache`), downloads
  use unpredictable temp names + `O_EXCL` + atomic rename; a cross-process
  `flock` on `~/.pap/lock` serializes concurrent `pap` runs.
- No `ldd` (never executes the target), no setuid propagation (copies drop
  setuid/setgid bits, store modes are `0644`/`0755` only).

## Internals

| Package             | Role                                                        |
|---------------------|-------------------------------------------------------------|
| `internal/elf`      | `DT_NEEDED`, VERNEED/VERDEF parsing (`debug/elf`)           |
| `internal/repo`     | pacman.conf/mirrorlist servers, gzip+zstd `.db` parse, sha256-pinned downloads |
| `internal/config`   | `~/.pap` paths (`PAP_HOME`), XDG cache, host architecture   |
| `internal/lock`     | cross-process `flock` on `~/.pap/lock`                      |
| `internal/resolver` | soname → package, arch-aware host-lib skip list             |
| `internal/store`    | content-addressed store, symlinks, GC                       |
| `internal/install`  | BFS install flow, `.PKGINFO` verify, compat check, patchelf |
| `internal/manifest` | per-app manifest + locked package versions + source hash    |

## Development

```sh
go vet ./...
go test ./...
PAP_HOME=$(mktemp -d) go test ./...  # isolated state
```
