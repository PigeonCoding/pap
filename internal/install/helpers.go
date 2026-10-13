package install

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"pap/internal/config"
	"pap/internal/elf"
	"pap/internal/repo"
)

// Helper executables: launcher scripts often run helper binaries by bare
// name (`exec electron35 ...`), which soname resolution never sees. pap
// scans executable scripts for such references, vendors the providing
// packages' binaries (+ their resources and libraries) into the exe payload,
// and puts the payload bin dirs on PATH via the BinDir launcher.
//
// Discovery is deliberately conservative: only executable non-ELF files are
// scanned, only command-position tokens count, anything resolvable on the
// host stays on the host, and anything already shipped in the payload is
// left alone. Whatever survives all three filters is fetched.

var (
	execLeadRe   = regexp.MustCompile(`(?:^\s*|[;&|(){]\s*)(exec|command)\s+(.*)$`)
	subshellRe   = regexp.MustCompile(`\$\(\s*([A-Za-z][A-Za-z0-9._+-]*)`)
	backquoteRe  = regexp.MustCompile("`\\s*([A-Za-z][A-Za-z0-9._+-]*)")
	envLeadRe    = regexp.MustCompile(`(?:^|[;&|]\s*)env\s+(\S+)`)
	envShebangRe = regexp.MustCompile(`^#!\s*\S*env\s+([A-Za-z][A-Za-z0-9._+-]*)`)
	execTokenRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._+-]{0,63}$`)
)

// execRefBlocklist are shell words that must never count as helper names,
// even if a repo package happened to ship a same-named binary.
var execRefBlocklist = map[string]bool{
	"sh": true, "bash": true, "dash": true, "ksh": true, "zsh": true,
	"ash": true, "exec": true, "command": true, "env": true,
}

// scanExecRefs returns candidate helper-executable names referenced by
// executable scripts under root, sorted and deduplicated.
func scanExecRefs(root string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !validExecRef(name) || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.Mode().Perm()&0111 == 0 || fi.Size() > 1<<20 {
			return nil
		}
		if hasELFMagic(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil // binary blob, not a script
		}
		for _, line := range strings.Split(string(data), "\n") {
			for _, name := range execRefsInLine(line) {
				add(name)
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func validExecRef(name string) bool {
	if execRefBlocklist[name] {
		return false
	}
	return execTokenRe.MatchString(name)
}

// execRefsInLine extracts helper candidates from one script line: `exec` /
// `command` targets, $(...) and backquote command positions, `env` targets
// and env-style shebangs. Comment lines never count.
func execRefsInLine(line string) []string {
	var out []string
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "#!") {
		if m := envShebangRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, m[1])
		}
		return out
	}
	if strings.HasPrefix(trimmed, "#") {
		return nil
	}
	if m := execLeadRe.FindStringSubmatch(line); m != nil {
		if cmd := execCommandToken(m[2]); cmd != "" {
			out = append(out, cmd)
		}
	}
	for _, m := range subshellRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	for _, m := range backquoteRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	if m := envLeadRe.FindStringSubmatch(line); m != nil {
		if !strings.Contains(m[1], "=") {
			out = append(out, m[1])
		}
	}
	return out
}

// execCommandToken resolves the executed command after exec/command,
// skipping flags (and the argv0 name after -a). Query modes
// (command -v/-V) resolve nothing.
func execCommandToken(rest string) string {
	skipArg0 := false
	for _, f := range strings.Fields(rest) {
		if skipArg0 {
			skipArg0 = false
			continue
		}
		if strings.HasPrefix(f, "-") && len(f) > 1 {
			if f == "--" {
				continue
			}
			if strings.HasPrefix(f, "--") {
				continue
			}
			if strings.ContainsAny(f, "vV") {
				return "" // command -v/-V: existence query, not a need
			}
			if strings.Contains(f, "a") {
				skipArg0 = true // exec -a NAME: next token is argv[0]
			}
			continue
		}
		return f
	}
	return ""
}

// helperPlan is one helper package to vendor: the executables it serves plus
// where to download it from (srcIdx nil = live default).
type helperPlan struct {
	names  []string
	pkg    repo.PkgInfo
	srcIdx *repo.Index
	snap   string
	ver    string // effective version (HelperLocked pin or provider)
	pinned bool
}

// planHelpers resolves missing helper executables referenced by the payload
// scripts to repo packages. No downloads: safe for dry runs.
func planHelpers(scanRoot string, scope *archiveScope, opts InstallOptions) []helperPlan {
	pins := opts.HelperLocked
	byPkg := map[string]*helperPlan{}
	var order []string
	for _, name := range scanExecRefs(scanRoot) {
		if payloadHasBin(scanRoot, name) {
			continue
		}
		if _, err := exec.LookPath(name); err == nil {
			continue // host provides it
		}
		provs, srcIdx, snap := binProvidersForHelper(name, scope)
		if len(provs) == 0 {
			continue
		}
		p := provs[0]
		key := p.Repo + "/" + p.Name
		ver, pinned := p.Version, false
		if pins != nil {
			if want, ok := pins[key]; ok && want != "" && want != ver {
				ver, pinned = want, true
			}
		}
		if hp, ok := byPkg[key]; ok {
			hp.names = append(hp.names, name)
			continue
		}
		byPkg[key] = &helperPlan{names: []string{name}, pkg: p, srcIdx: srcIdx, snap: snap, ver: ver, pinned: pinned}
		order = append(order, key)
	}
	out := make([]helperPlan, 0, len(order))
	for _, k := range order {
		out = append(out, *byPkg[k])
	}
	return out
}

// binProvidersForHelper resolves a helper executable name to packages,
// live-first (snapshot-first under PreferArchive for era consistency).
func binProvidersForHelper(name string, scope *archiveScope) ([]repo.PkgInfo, *repo.Index, string) {
	if live := repo.Default().ProvidersForBin(name); len(live) > 0 {
		if scope.preferArchive {
			if arch, idx, snap := scope.archiveBinProviders(name); len(arch) > 0 {
				return arch, idx, snap
			}
		}
		return live, nil, ""
	}
	if arch, idx, snap := scope.archiveBinProviders(name); len(arch) > 0 {
		return arch, idx, snap
	}
	return nil, nil, ""
}

// payloadHasBin reports whether the payload already ships an executable of
// that basename under usr/bin or usr/sbin.
func payloadHasBin(payloadRoot, name string) bool {
	for _, cls := range []string{"bin", "sbin"} {
		if st, err := os.Stat(filepath.Join(payloadRoot, "usr", cls, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// helperResult is what vendorHelpers staged into the payload.
type helperResult struct {
	used     map[string]string // repo/name -> downloaded version (manifest Packages)
	pins     map[string]string // repo/name -> version (manifest Helpers)
	needed   []string          // extra DT_NEEDED entries for the BFS
	pathDirs []string          // payload-relative helper bin dirs present
}

// vendorHelpers downloads each planned helper package and stages its
// executables, resource trees and directly-needed shared objects into the
// staged payload. Discovery repeats (new scripts can reference more
// helpers), bounded to three rounds.
func vendorHelpers(stagedRoot string, scope *archiveScope, local map[string]string, opts InstallOptions) (helperResult, error) {
	res := helperResult{used: map[string]string{}, pins: map[string]string{}}
	seenPkgs := map[string]bool{}
	var addedELFs []string
	var helperLibRoots []string
	skip := func(soname string) bool { return skipLib(opts, soname) }
	for round := 0; round < 3; round++ {
		plans := planHelpers(stagedRoot, scope, opts)
		progress := false
		for _, plan := range plans {
			key := plan.pkg.Repo + "/" + plan.pkg.Name
			if seenPkgs[key] {
				continue
			}
			seenPkgs[key] = true
			added, libRoots, ver, derr := vendorOneHelper(plan, stagedRoot, opts)
			if derr != nil {
				opts.printf("  [warn] helper %s: %v; binaries referencing it may fail at runtime\n",
					strings.Join(plan.names, ","), derr)
				continue
			}
			res.used[key], res.pins[key] = ver, ver
			addedELFs = append(addedELFs, added...)
			helperLibRoots = append(helperLibRoots, libRoots...)
			opts.printf("  [helper] %s -> %s/%s %s\n", strings.Join(plan.names, ","),
				plan.pkg.Repo, plan.pkg.Name, ver)
			progress = true
		}
		if !progress {
			break
		}
	}
	// Helper-private libraries win over repo downloads, like app bundles.
	for k, v := range buildLocalLibs(helperLibRoots) {
		if _, ok := local[k]; !ok {
			local[k] = v
		}
	}
	seen := map[string]bool{}
	for _, p := range addedELFs {
		for _, n := range neededFromFile(p, skip) {
			if !seen[n] {
				seen[n] = true
				res.needed = append(res.needed, n)
			}
		}
	}
	for _, cls := range []string{"bin", "sbin"} {
		if st, err := os.Stat(filepath.Join(stagedRoot, "usr", cls)); err == nil && st.IsDir() {
			res.pathDirs = append(res.pathDirs, filepath.Join("usr", cls))
		}
	}
	return res, nil
}

// downloadHelperPkg fetches the helper package at the planned (or pinned)
// version: exact-version live-or-archive lookup, falling back to the
// resolving snapshot index when that misses.
func downloadHelperPkg(plan helperPlan, opts InstallOptions) ([]byte, string, error) {
	data, info, err := repo.Default().DownloadAppPackage(plan.pkg.Name, plan.ver)
	if err == nil {
		return data, info.Version, nil
	}
	if plan.srcIdx != nil {
		if data, derr := plan.srcIdx.Download(plan.pkg); derr == nil {
			return data, plan.pkg.Version, nil
		}
	}
	return nil, "", err
}

// vendorOneHelper stages one helper package's executables, resource trees
// and directly-needed shared objects. It returns staged ELF paths and the
// staged private lib roots for local indexing.
func vendorOneHelper(plan helperPlan, stagedRoot string, opts InstallOptions) (added []string, libRoots []string, ver string, err error) {
	data, ver, err := downloadHelperPkg(plan, opts)
	if err != nil {
		return nil, nil, "", err
	}
	work, err := os.MkdirTemp("", "pap-helper-*")
	if err != nil {
		return nil, nil, "", err
	}
	defer os.RemoveAll(work)
	pkgFile := filepath.Join(work, "helper.pkg.tar.zst")
	if err := os.WriteFile(pkgFile, data, 0644); err != nil {
		return nil, nil, "", err
	}
	tmp := filepath.Join(work, "pkg")
	if err := os.MkdirAll(tmp, 0755); err != nil {
		return nil, nil, "", err
	}
	if out, xerr := exec.Command("bsdtar", "-xf", pkgFile, "-C", tmp).CombinedOutput(); xerr != nil {
		// Same tolerance as dependency extraction: links pointing outside
		// the extracted set warn but rarely matter for the staged subset.
		if s := strings.TrimSpace(string(out)); s != "" {
			opts.printf("  [warn] bsdtar %s: %s\n", plan.pkg.Name, s)
		}
	}
	skip := func(soname string) bool { return skipLib(opts, soname) }
	// 1. Executables directly under usr/bin and usr/sbin.
	var exeNames []string
	for _, cls := range []string{"bin", "sbin"} {
		srcDir := filepath.Join(tmp, "usr", cls)
		entries, rerr := os.ReadDir(srcDir)
		if rerr != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			src := filepath.Join(srcDir, e.Name())
			fi, serr := os.Lstat(src)
			if serr != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0111 == 0 {
				continue
			}
			dst := filepath.Join(stagedRoot, "usr", cls, e.Name())
			if _, exists := os.Lstat(dst); exists == nil {
				continue // app payload (or an earlier helper) wins
			}
			if serr := stagePayloadFile(src, dst); serr != nil {
				return nil, nil, "", serr
			}
			exeNames = append(exeNames, e.Name())
			if info, perr := elf.Parse(dst); perr == nil {
				if merr := elf.MachineMismatch(info.Machine, config.Arch); merr != nil {
					return nil, nil, "", fmt.Errorf("helper %s from %s/%s: %w", e.Name(), plan.pkg.Repo, plan.pkg.Name, merr)
				}
				if cerr := checkELFHost(dst, info, opts.ForceLibc); cerr != nil {
					return nil, nil, "", fmt.Errorf("helper %s: %w", e.Name(), cerr)
				}
				added = append(added, dst)
			}
		}
	}
	// 2. Resource trees under usr/lib and usr/share matching the helper
	// executables or the package name (either direction: electron35 ships
	// usr/lib/electron35; a lib dir may also drop its own suffix).
	tokens := append(append([]string{plan.pkg.Name}, exeNames...), plan.names...)
	seenFirst := map[string]bool{}
	for _, cls := range []string{"lib", "share"} {
		srcParent := filepath.Join(tmp, "usr", cls)
		entries, rerr := os.ReadDir(srcParent)
		if rerr != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seenFirst[cls+"/"+e.Name()] {
				continue
			}
			matched := false
			for _, tok := range tokens {
				if tok == "" {
					continue
				}
				if e.Name() == tok || strings.HasPrefix(e.Name(), tok) || strings.HasPrefix(tok, e.Name()) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			seenFirst[cls+"/"+e.Name()] = true
			dst := filepath.Join(stagedRoot, "usr", cls, e.Name())
			if _, exists := os.Stat(dst); exists == nil {
				continue
			}
			if cerr := copyTree(filepath.Join(srcParent, e.Name()), dst); cerr != nil {
				return nil, nil, "", cerr
			}
			if cls == "lib" {
				libRoots = append(libRoots, dst)
			}
			// Resource trees carry their own ELFs (e.g. electron's real
			// binary and private libs): their DT_NEEDED must join
			// resolution exactly like top-level helpers'.
			for _, p := range listFolderELFs(dst) {
				if info, perr := elf.Parse(p); perr == nil {
					if merr := elf.MachineMismatch(info.Machine, config.Arch); merr != nil {
						return nil, nil, "", fmt.Errorf("helper resource %s from %s/%s: %w", relTo(stagedRoot, p), plan.pkg.Repo, plan.pkg.Name, merr)
					}
					if cerr := checkELFHost(p, info, opts.ForceLibc); cerr != nil {
						return nil, nil, "", fmt.Errorf("helper resource %s: %w", relTo(stagedRoot, p), cerr)
					}
					added = append(added, p)
				}
			}
		}
	}
	// 3. Top-level shared objects the helper executables need directly, so
	// the exact shipped build wins over a repo download.
	needSet := map[string]bool{}
	for _, p := range added {
		for _, n := range neededFromFile(p, skip) {
			needSet[n] = true
		}
	}
	for soname := range needSet {
		src := filepath.Join(tmp, "usr", "lib", soname)
		if _, serr := os.Lstat(src); serr != nil {
			continue
		}
		dst := filepath.Join(stagedRoot, "usr", "lib", soname)
		if _, exists := os.Lstat(dst); exists == nil {
			continue
		}
		if serr := stagePayloadFile(src, dst); serr != nil {
			return nil, nil, "", serr
		}
		if _, perr := elf.Parse(dst); perr == nil {
			added = append(added, dst)
		}
	}
	return added, libRoots, ver, nil
}

// stagePayloadFile copies one helper file into the staged payload,
// dereferencing symlinks (the loader only needs the bytes under the
// soname/exe name) and preserving the mode like copyFile (setuid dropped).
func stagePayloadFile(src, dst string) error {
	real := src
	if fi, err := os.Lstat(src); err != nil {
		return err
	} else if fi.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(src)
		if err != nil {
			return err
		}
		real = target
	}
	data, err := os.ReadFile(real)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	mode := os.FileMode(0755)
	if fi, err := os.Stat(real); err == nil {
		mode = fi.Mode().Perm() & 0777
		if mode == 0 {
			mode = 0755
		}
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// rewriteStagedRefs rewrites absolute /usr/{bin,sbin,lib,share} references
// in staged scripts to the installed payload paths, for locations actually
// present in the payload (app and helper resource dirs and executables).
// Without it, `exec electron35 /usr/lib/vesktop/app.asar` would resolve the
// asar against the host instead of the payload.
func rewriteStagedRefs(stagedRoot, name string, opts InstallOptions) error {
	exePayload := filepath.Join(config.ExeDir, name, "pkg")
	type rule struct {
		old string
		re  *regexp.Regexp
		new string
	}
	var rules []rule
	addRule := func(old, new string) {
		rules = append(rules, rule{
			old: old,
			re:  regexp.MustCompile(regexp.QuoteMeta(old) + `($|[/"' ` + "\t" + `\n])`),
			new: new,
		})
	}
	for _, cls := range []string{"lib", "share"} {
		dir := filepath.Join(stagedRoot, "usr", cls)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			addRule(filepath.Join("/usr", cls, e.Name()),
				filepath.Join(exePayload, "usr", cls, e.Name()))
		}
	}
	for _, cls := range []string{"bin", "sbin"} {
		dir := filepath.Join(stagedRoot, "usr", cls)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			addRule(filepath.Join("/usr", cls, e.Name()),
				filepath.Join(exePayload, "usr", cls, e.Name()))
		}
	}
	if len(rules) == 0 {
		return nil
	}
	// Longest prefix first, so /usr/bin/electron35 wins over /usr/bin/electron.
	sort.Slice(rules, func(i, j int) bool { return len(rules[i].old) > len(rules[j].old) })
	// Variable-indirect refs (e.g. electron35's `exec /usr/lib/${name}/electron`
	// with name=electron35): the literal rules above never match a `$`, so
	// rewrite the /usr/<cls>/$ prefix itself to the payload. The variable
	// part is preserved, so ${name} still expands to the staged dir name.
	type varRule struct {
		old, new string
		label    string
	}
	var varRules []varRule
	for _, cls := range []string{"lib", "share", "bin", "sbin"} {
		if st, err := os.Stat(filepath.Join(stagedRoot, "usr", cls)); err != nil || !st.IsDir() {
			continue
		}
		old := filepath.Join("/usr", cls) + "/$"
		varRules = append(varRules, varRule{
			old:   old,
			new:   filepath.Join(exePayload, "usr", cls) + "/$",
			label: filepath.Join("/usr", cls) + "/$VAR",
		})
	}
	return filepath.WalkDir(stagedRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.Mode().Perm()&0111 == 0 || fi.Size() > 1<<20 {
			return nil
		}
		if hasELFMagic(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		out := data
		changed := []string{}
		for _, r := range rules {
			if !r.re.Match(out) {
				continue
			}
			changed = append(changed, r.old)
			out = r.re.ReplaceAll(out, []byte(r.new+"$1"))
		}
		for _, vr := range varRules {
			if !strings.Contains(string(out), vr.old) {
				continue
			}
			changed = append(changed, vr.label)
			out = []byte(strings.ReplaceAll(string(out), vr.old, vr.new))
		}
		if len(changed) == 0 {
			return nil
		}
		mode := fi.Mode().Perm() & 0777
		if err := os.WriteFile(path, out, mode); err != nil {
			return err
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		rel := relTo(stagedRoot, path)
		opts.printf("  [rewrite] %s: %s -> payload\n", rel, strings.Join(changed, ", "))
		return nil
	})
}
