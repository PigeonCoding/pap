# TODO — design issues and fixes

## Correctness (will bite us)

- [x] **Hard-fail unresolved sonames** — resolve loop currently only warns and
  `continue`s; install must abort if any `DT_NEEDED` can't be satisfied instead
  of producing a broken app (`internal/install/install.go`).
- [x] **Hash-only store keys** — `store.Store()` keys `<sha256>-<filename>`,
  so identical content under `libcap.so`/`libcap.so.2`/`libcap.so.2.78` is
  stored 3×. Store as `<sha256>` only; the symlink name carries the SONAME
  (`internal/store/store.go`).
- [x] **Compat check must walk `DT_NEEDED` for file existence** — VERNEED is
  empty for unversioned-symbol libs, so a missing lib is invisible to the
  check. Existence check via recursive `DT_NEEDED`, VERNEED only for versions
  (`internal/install/compat.go`).
- [x] **Pin all downloads to the mirror that served the `.db`** — per-file
  mirror fallback can assemble one app from two mirror snapshots
  (`internal/repo/repo.go`).
- [x] **Read repo set from `pacman.conf`** — hardcoded `core, extra,
  multilib` ignores custom repos (system runs CachyOS v3; already saw
  `ncurses 6.6-2` resolved vs installed `6.6-2.1`)
  (`internal/repo/repo.go`).
- [x] **`.db` cache TTL** — currently never refetched unless gzip-invalid;
  stale index 404s once mirrors prune old versions. Add expiry (ETag /
  If-Modified-Since or mtime TTL) (`internal/repo/repo.go`).
- [x] **Use `%FILENAME%` from `.db`** instead of reconstructing
  `name-version-arch` for download URLs (epoch/arch quirks)
  (`internal/repo/repo.go`).
- [x] **Support non-`=` provides** — `provideToSoname` skips bare
  `libfoo.so.1` provides; accept them (`internal/repo/repo.go`).
- [x] **Retry alternate providers** — when the chosen package doesn't contain
  the requested soname, try the next candidate from the index before failing
  (`internal/repo/repo.go`, `internal/install/install.go`).

## Isolation completeness

- [x] **Patchelf every vendored lib** — a lib's own `DT_RUNPATH` takes
  precedence over the exe's `DT_RPATH` and leaks to system libs. Set
  `--force-rpath --set-rpath '$ORIGIN'` on each extracted lib too
  (`internal/install/install.go`).
- [x] **Remove `libstdc++`/`libgcc_s` from the skip list** — unlike glibc
  they are isolatable; currently C++ binaries from newer toolchains fail
  VERNEED with no remedy (`internal/resolver/resolver.go`).
- [x] **Vendor plugin dirs from packages** — top-level `usr/lib/` filename
  filter drops `ossl-modules/`, gconv, NSS subdirs even when shipped
  (`internal/install/install.go`).

## Security

- [x] **Verify SHA256 from `.db` desc** — currently only `.PKGINFO`
  self-consistency (attacker-controlled). Check the archive against the
  checksum recorded in the repo index (`internal/repo/repo.go`,
  `internal/install/install.go`).
- [x] **Download timeouts** — a hanging mirror blocks the install forever;
  use an `http.Client` with timeout (`internal/repo/repo.go`).

## Robustness / UX

- [x] **Read the lock on reinstall** — `manifest.json` records exact
  versions but nothing consumes them; add `reinstall --locked` and an
  `upgrade` command (`internal/manifest/manifest.go`, `cmd/pkg`).
- [x] **Package download cache** — `pkgCache` is in-memory per run; persist
  downloaded packages (keyed by `%FILENAME%`) so reinstalls don't re-fetch
  bytes already in the store.
- [x] **Atomic store writes** — `os.WriteFile` straight to the final
  hash-named path: crash leaves a truncated file reused forever. Write temp +
  `rename` (`internal/store/store.go`).
- [x] **GC/install race** — GC scanning `apps/*/libs` can unlink a store file
  written but not yet linked by a concurrent install; reference-count or
  defer GC windows (`internal/store/store.go`).
- [x] **Transactionality** — failure after lib extraction but before manifest
  write leaves a half-install; stage in temp dir and rename app dir into
  place; clean up on error (`internal/install/install.go`).
- [x] **Refuse silent overwrite** — installing an existing app name must
  require `--force` (`internal/install/install.go`).
- [x] **Preserve existing RPATH** — `--set-rpath` overwrites legitimate
  paths; append/preserve original entries (`internal/install/install.go`).
- [x] **Wrapper fallback when patchelf fails** — packed/UPX/compressed
  binaries: generate a shell wrapper with `LD_LIBRARY_PATH` instead of
  aborting (`internal/install/install.go`).
- [x] **Handle setuid and moved binaries** — glibc ignores RPATH for setuid;
  copying a patched binary elsewhere breaks `$ORIGIN`. Detect and warn (or
  document) (`internal/install/install.go`).
- [x] **Friendly errors for non-ELF inputs** — scripts (shebang), musl,
  non-ELF: detect and either wrap (script passthrough) or explain, instead
  of a raw parser error (`internal/install/install.go`).

## Refactor / decide early

- [x] **Factor the BFS+stage step** out of `InstallElf` before implementing
  `install-pkg` — resolution logic will otherwise diverge into two paths
  (`internal/install/install.go`).
- [x] **Decouple store/app paths** — GC derives `apps/` via
  `filepath.Dir(StoreDir)`; make both explicit from a single config root
  (`internal/store/store.go`, `internal/install/install.go`).
- [x] **Arch-parameterize** — `$arch`/`usr/lib` hardcoded to x86_64; thread
  architecture through repo index and extraction.
- [x] **Multi-arch ELF detection** — reject (or handle) i686/musl binaries
  explicitly instead of resolving against x86_64.

## Fix order

1. Hard-fail unresolved sonames
2. Hash-only store keys + atomic renames
3. SHA256 verify from `.db`
4. `.db` TTL + mirror pinning
5. Per-lib RPATH patch
6. Read the lock on reinstall
