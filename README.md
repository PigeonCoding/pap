# pap

Per-app library isolation for ELF binaries.

Give it an ELF, a script, or an app folder: it resolves every non-host
library recursively, downloads the exact packages from Arch mirrors, vendors
the `.so` files into a content-addressed store, and patches `RPATH` to a
private `libs/` folder.

State lives in `~/.pap` (`PAP_HOME` overrides it).

## Requirements

- libarchive 
- patchelf  # bsdtar + patchelf
- Go 1.27+ to build

## Usage

```sh
go build -o pap ./cmd/pap

pap install [--force] [--dry-run] [--quiet] [--add-path] [--exe <rel>] <path> [name]
pap reinstall <name>   # reinstall from source, pinned to manifest versions
pap upgrade <name>     # reinstall, re-resolving to current repo versions
pap list | pap info <name> | pap uninstall <name> | pap gc | pap doctor | pap version
```

Folder entrypoint: `<folder>/<name>` (or `<folder>/bin/<name>`), scripts beat
ELFs; `--exe <rel>` pins it explicitly. Every bundled ELF gets patched, so a
script main stays local too.

`PAP_BIN_DIR` overrides the bin dir. `--dry-run` prints the resolve plan
without changing anything; `--force` swaps atomically via `.bak`.

## Example

```sh
$ wget https://github.com/kovidgoyal/kitty/releases/download/v0.49.2/kitty-0.49.2-x86_64.txz
$ mkdir kitty
$ cd kitty
$ tar xvf ../kitty-0.49.2-x86_64.txz
$ ➜  probe ./pap/pap install --add-path ./kitty --exe bin/kitty kitty
Installing kitty from kitty (main executable: bin/kitty)
Direct NEEDED: [libpython3.14.so.1.0 libc.so.6]
Copying folder...

Resolving dependencies (recursive)...
  note: libpython3.14.so.1.0 is not declared in package metadata; loading repo file indexes (downloaded once, then cached)
  [local] libpython3.14.so.1.0 -> kitty/lib/libpython3.14.so.1.0
  [local] libslang-compiler.so.0.0.0.0 -> kitty/lib/libslang-compiler.so
  [ ok ] libstdc++.so.6 -> core/libstdc++ 16.2.1+r23+gd564253eb6c8-1
  [ ok ] libgcc_s.so.1 -> core/libgcc 16.2.1+r23+gd564253eb6c8-1
  [local] libbz2.so.1.0 -> kitty/lib/libbz2.so.1.0
  [local] libffi.so.8 -> kitty/lib/libffi.so.8
  [local] libncursesw.so.6 -> kitty/lib/libncursesw.so.6
  [ ok ] libpanelw.so.6 -> core/ncurses 6.6-2
  [local] libcrypto.so.3 -> kitty/lib/libcrypto.so.3
  [local] liblzma.so.5 -> kitty/lib/liblzma.so.5
  [ ok ] libsqlite3.so -> core/sqlite 3.53.4-1
  [local] libssl.so.3 -> kitty/lib/libssl.so.3
  [ ok ] libuuid.so.1 -> core/util-linux-libs 2.42.4-1
  [local] libz.so.1 -> kitty/lib/libz.so.1
  [local] libxxhash.so.0 -> kitty/lib/libxxhash.so.0
  [local] libcairo.so.2 -> kitty/lib/libcairo.so.2
  [local] libfreetype.so.6 -> kitty/lib/libfreetype.so.6
  [local] libharfbuzz.so.0 -> kitty/lib/libharfbuzz.so.0
  [local] libpng16.so.16 -> kitty/lib/libpng16.so.16
  [local] liblcms2.so.2 -> kitty/lib/liblcms2.so.2
  [local] libwayland-client.so.0 -> kitty/lib/libwayland-client.so.0
  [local] libxkbcommon.so.0 -> kitty/lib/libxkbcommon.so.0
  [ ok ] libdbus-1.so.3 -> core/dbus 1.16.2-1
  [ ok ] libX11.so.6 -> extra/libx11 1.8.13-2
  [ ok ] libXcursor.so.1 -> extra/libxcursor 1.2.3-1
  [local] libxkbcommon-x11.so.0 -> kitty/lib/libxkbcommon-x11.so.0
  [ ok ] libX11-xcb.so.1 -> extra/libx11 1.8.13-2
  [local] libexpat.so.1 -> kitty/lib/libexpat.so.1
  [local] libreadline.so.8 -> kitty/lib/libreadline.so.8
  [local] libbrotlicommon.so.1 -> kitty/lib/libbrotlicommon.so.1
  [ ok ] libfontconfig.so.1 -> extra/fontconfig 2:2.18.3-2
  [local] libpixman-1.so.0 -> kitty/lib/libpixman-1.so.0
  [local] libbrotlidec.so.1 -> kitty/lib/libbrotlidec.so.1
  [local] libiconv.so.2 -> kitty/lib/libiconv.so.2
  [local] libpcre2-8.so.0 -> kitty/lib/libpcre2-8.so.0
  [ ok ] libxcb.so.1 -> extra/libxcb 1.17.0-1
  [ ok ] libxcb-xkb.so.1 -> extra/libxcb 1.17.0-1
  [ ok ] libsystemd.so.0 -> core/systemd-libs 262-1
  [ ok ] libXrender.so.1 -> extra/libxrender 0.9.12-1
  [ ok ] libXfixes.so.3 -> extra/libxfixes 6.0.2-1
  [ ok ] libXau.so.6 -> extra/libxau 1.0.12-1
  [ ok ] libXdmcp.so.6 -> extra/libxdmcp 1.1.5-2

Patching vendored libs with their own RUNPATH...

Checking dependency compatibility...
  [repo-fallback] libncursesw.so.6: bundled copy incompatible, using repo build

Resolving dependencies (recursive)...
  [local] libpython3.14.so.1.0 -> kitty/lib/libpython3.14.so.1.0
  [local] libslang-compiler.so.0.0.0.0 -> kitty/lib/libslang-compiler.so
  [ ok ] libstdc++.so.6 -> core/libstdc++ 16.2.1+r23+gd564253eb6c8-1
  [ ok ] libgcc_s.so.1 -> core/libgcc 16.2.1+r23+gd564253eb6c8-1
  [local] libbz2.so.1.0 -> kitty/lib/libbz2.so.1.0
  [local] libffi.so.8 -> kitty/lib/libffi.so.8
  [ ok ] libncursesw.so.6 -> core/ncurses 6.6-2
  [ ok ] libpanelw.so.6 -> core/ncurses 6.6-2
  [local] libcrypto.so.3 -> kitty/lib/libcrypto.so.3
  [local] liblzma.so.5 -> kitty/lib/liblzma.so.5
  [ ok ] libsqlite3.so -> core/sqlite 3.53.4-1
  [local] libssl.so.3 -> kitty/lib/libssl.so.3
  [ ok ] libuuid.so.1 -> core/util-linux-libs 2.42.4-1
  [local] libz.so.1 -> kitty/lib/libz.so.1
  [local] libxxhash.so.0 -> kitty/lib/libxxhash.so.0
  [local] libcairo.so.2 -> kitty/lib/libcairo.so.2
  [local] libfreetype.so.6 -> kitty/lib/libfreetype.so.6
  [local] libharfbuzz.so.0 -> kitty/lib/libharfbuzz.so.0
  [local] libpng16.so.16 -> kitty/lib/libpng16.so.16
  [local] liblcms2.so.2 -> kitty/lib/liblcms2.so.2
  [local] libwayland-client.so.0 -> kitty/lib/libwayland-client.so.0
  [local] libxkbcommon.so.0 -> kitty/lib/libxkbcommon.so.0
  [ ok ] libdbus-1.so.3 -> core/dbus 1.16.2-1
  [ ok ] libX11.so.6 -> extra/libx11 1.8.13-2
  [ ok ] libXcursor.so.1 -> extra/libxcursor 1.2.3-1
  [local] libxkbcommon-x11.so.0 -> kitty/lib/libxkbcommon-x11.so.0
  [ ok ] libX11-xcb.so.1 -> extra/libx11 1.8.13-2
  [local] libexpat.so.1 -> kitty/lib/libexpat.so.1
  [local] libreadline.so.8 -> kitty/lib/libreadline.so.8
  [local] libbrotlicommon.so.1 -> kitty/lib/libbrotlicommon.so.1
  [ ok ] libfontconfig.so.1 -> extra/fontconfig 2:2.18.3-2
  [local] libpixman-1.so.0 -> kitty/lib/libpixman-1.so.0
  [local] libbrotlidec.so.1 -> kitty/lib/libbrotlidec.so.1
  [local] libiconv.so.2 -> kitty/lib/libiconv.so.2
  [local] libpcre2-8.so.0 -> kitty/lib/libpcre2-8.so.0
  [ ok ] libxcb.so.1 -> extra/libxcb 1.17.0-1
  [ ok ] libxcb-xkb.so.1 -> extra/libxcb 1.17.0-1
  [ ok ] libsystemd.so.0 -> core/systemd-libs 262-1
  [ ok ] libXrender.so.1 -> extra/libxrender 0.9.12-1
  [ ok ] libXfixes.so.3 -> extra/libxfixes 6.0.2-1
  [ ok ] libXau.so.6 -> extra/libxau 1.0.12-1
  [ ok ] libXdmcp.so.6 -> extra/libxdmcp 1.1.5-2

Patching vendored libs with their own RUNPATH...

Checking dependency compatibility...
Patching RPATH (113 bundled ELF(s))...

Done. Run: /home/user/.local/bin/kitty
```

## How it works

- Soname → package via `%PROVIDES%` (`.files` index fallback), no `ldd`.
- Downloads are version-pinned and verified (`SHA256SUM` + `.PKGINFO` name/version/arch).
- Bundled `.so` files win over repo downloads.
- `patchelf --force-rpath` sets an absolute `RPATH` to `apps/<name>/libs`
  (transitive deps need `RPATH`, not `RUNPATH`); vendored libs with their own
  `RUNPATH` are rewritten to `$ORIGIN`.
- Folders are copied verbatim under `exe/<name>/pkg/`; exe-relative
  resources (`../lib/<app>`, companions, plugins) are vendored alongside.
- `~/.local/bin/<name>` is a symlink to the real binary (scripts get a tiny
  launcher so `$0` resolves into the payload). Shell rc is never edited
  unless you pass `--add-path`.

```
~/.local/bin/<name>   →  ~/.pap/exe/<name>/… (on PATH)
~/.pap/apps/<name>/   libs/ (SONAME symlinks → store) + manifest.json
~/.pap/exe/<name>/    bin/<name> | pkg/… (folders) | lib/… share/… (resources)
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
