# TODO — design issues and fixes

## Correctness (will bite us)

- [x] **Fix `--force` failure destroying the previous install** — `placeBinary`
  runs before `publish` (`internal/install/install.go:251`, `:260`); on failure
  the rollback removes the new binary but the old one was already overwritten
  by `moveFile`. `publish` also does `os.RemoveAll(final)` before `rename`
  (`install.go:355`) — rename to `.bak`, swap, then delete.
- [x] **Make `SkipLibs` arch-aware and complete** — hardcoded x86_64 list
  (`internal/resolver/resolver.go:9`): `ld-linux-aarch64.so.1` missing so
  aarch64 support never works; `libutil/libcrypt/libnsl/libanl/libmvec/
  libthread_db/libnss_*` missing → unresolvable sonames hard-fail. Derive the
  host-provided set dynamically from the host `ld-linux` → `libc.so.6` NEEDED
  closure instead of a hand-written list.
- [x] **Fix `Index.Load` races** — `parseDb` writes `fileMap` unlocked and the
  `loaded` check (`internal/repo/repo.go:193`) is non-atomic; `Load` returns
  `nil` even when every repo failed (`repo.go:227`), yielding misleading
  "no package provides" errors — fail fast on an empty index. Missing optional
  repos (no `multilib`) print errors on every install (`repo.go:219`).
- [x] **Add a cross-process lock** — concurrent `pkg` runs race on `publish`,
  stage pruning, and `atomicWrite`'s fixed `.tmp` name
  (`internal/repo/repo.go:548`). `flock` on `~/.pap/lock` covers all of it.
- [x] **Make config injectable** — package-level `config` vars + the
  `repo.Default()` singleton make the install path untestable. Support
  `PAP_HOME` (or pass a config struct) and give `Index` a constructor.

## Security

- [x] **Move the repo cache out of `/tmp`** — `filepath.Join(os.TempDir(),
  "arch-repo-cache")` (`internal/repo/repo.go:70`) is predictable and
  world-writable: index planting / symlink attacks via fixed `.tmp` names.
  Use XDG `~/.cache/pap` + `os.CreateTemp`.
- [x] **Package signature verification** — SHA256 comes from the mirror-served
  index; pacman would check SigLevel/keyring. Verify `.sig` against
  `/etc/pacman.d/keyring`, or document the threat model explicitly.
- [x] **Preserve file modes** — `store.Store` chmods everything 0755
  (`internal/store/store.go:51`) and `copyFile` ignores the source mode
  (`internal/install/install.go:742`).

## Tests / CI

- [x] **De-host the existing tests** — `internal/elf/version_test.go:10`
  hardcodes `/usr/bin/curl` and `GLIBC_2.43`; fails on any other machine.
  Ship fixtures (tiny checked-in ELFs) or `t.Skip`.
- [x] **Table tests for the pure logic** — `provideToSoname`, `descField`,
  `parseProvides`, `pkgURL`/`orderedServers`, `store.GC`/`ScanUsed`,
  `manifest`, `validName`: all currently untested and I/O-free.
- [x] **End-to-end install test** — serve a synthetic gzip `.db` + fake package
  from `httptest` and run the full flow (blocked on the injectable-config
  refactor above).
- [x] **Add LICENSE; CI removed per request** — `go vet ./...` + `go test ./...`
  run locally (GitHub Actions workflow removed).

## Docs / naming

- [x] **Fix README drift** — the diagram says
  `patchelf --force-rpath '$ORIGIN/libs'` (`README.md:26`) but the code sets an
  absolute path (`internal/install/install.go:233`); add a Requirements
  section listing `bsdtar` + `patchelf`; drop `install-pkg` from usage until it
  exists.
- [x] **Implement or hide `install-pkg`** — `internal/install/install.go:371`
  returns "not yet implemented" while `cmd/pkg/main.go:59` advertises it.
- [x] **Unify naming** — module `probe`, binary `pkg`, state `~/.pap`, rc
  comment `pap`. Pick one name for module path, binary, and state dir.

## Performance / UX

- [x] **Parallelize downloads** — the BFS resolves and downloads sequentially
  (`internal/install/install.go:120-203`); add a worker pool + progress
  output. `Download` returns the whole package as `[]byte`
  (`internal/repo/repo.go:497`) and `copyFile` reads whole files — stream to
  disk instead (packages reach hundreds of MB).
- [x] **Stop editing shell rc silently** — `ensurePath` appends to
  `~/.zshrc`/`.bashrc`/`.profile` (`internal/install/install.go:707`) without
  consent, and its `.local/bin` substring match false-positives. Make it opt-in
  (flag or prompt).
- [x] **Safer uninstall** — remove `~/.local/bin/<name>` only if we placed it
  (record it in the manifest), and run GC afterwards.
- [x] **Manifest source hash** — record the source ELF's sha256
  (`internal/install/install.go:245`) so reinstall warns on drift.
- [x] **Locked reinstall fallback** — when the repo dropped a locked version
  (`install.go:153`) offer `upgrade` instead of failing outright.
- [x] **UX extras** — preflight check for `patchelf`/`bsdtar` (friendly error
  or `pkg doctor`), `--dry-run`, `--version`, `pkg info <name>`, sizes/versions
  in `list`, and an injected `io.Writer` for output (replace scattered
  `fmt.Printf`/stderr writes) to support `--quiet`.

## Fix order

1. `--force` rollback / atomic `publish`
2. Dynamic host-lib skip list (unblocks aarch64)
3. `Index.Load` locking + fail-fast + injectable config
4. Repo cache out of `/tmp` + cross-process flock
5. Test fixtures + CI
6. README/naming cleanup
