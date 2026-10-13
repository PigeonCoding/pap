package install

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pap/internal/config"
)

func TestExecRefsInLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{`exec electron35 /usr/lib/vesktop/app.asar "$@"`, []string{"electron35"}},
		{`  exec -a foo bar --flag`, []string{"bar"}},
		{`exec -c foo`, []string{"foo"}},
		{`command foo --bar`, []string{"foo"}},
		{`command -v foo || fallback`, nil}, // query mode: no need
		{`x=$(helper-tool --flag)`, []string{"helper-tool"}},
		{"y=`other_helper arg`", []string{"other_helper"}},
		{`FOO=$(/usr/bin/tool)`, nil}, // absolute path, not a bare name
		{`env electron --app`, []string{"electron"}},
		{`env FOO=1 bar`, nil}, // VAR= assignment, command position unknown
		{`#!/usr/bin/env python3`, []string{"python3"}},
		{`#!/bin/sh`, nil},
		{`# exec commented-out`, nil},
		{`#exec commented-out`, nil},
		{`echo hello world`, nil},
		{`mkdir -p "$dir" && helper2`, nil}, // mid-line bare words don't count
		{`foo; exec second --x`, []string{"second"}},
		{`A=1 exec third`, nil}, // assignment prefix: conservative skip
		{`exec`, nil},
		{``, nil},
	} {
		got := execRefsInLine(tc.line)
		if !slices.Equal(got, tc.want) {
			t.Errorf("execRefsInLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestValidExecRef(t *testing.T) {
	for _, bad := range []string{"", "sh", "bash", "exec", "env", "-flag", "/abs/path", "with space", "has$dollar", strings.Repeat("a", 65)} {
		if validExecRef(bad) {
			t.Errorf("validExecRef(%q) = true, want false", bad)
		}
	}
	for _, ok := range []string{"electron35", "ehelper", "libfoo", "a", "x-y_z.1"} {
		if !validExecRef(ok) {
			t.Errorf("validExecRef(%q) = false, want true", ok)
		}
	}
}

func TestScanExecRefs(t *testing.T) {
	dir := t.TempDir()
	// +x script with refs (incl. dupes and comments).
	if err := os.WriteFile(filepath.Join(dir, "run.sh"),
		[]byte("#!/bin/sh\n# exec commented\nexec alpha --x\nexec alpha\ny=$(beta)\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// Non-executable script: ignored.
	if err := os.WriteFile(filepath.Join(dir, "lib.sh"), []byte("#!/bin/sh\nexec gamma\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// +x binary blob with NUL: ignored.
	if err := os.WriteFile(filepath.Join(dir, "blob"), []byte("#!/bin/sh\nexec delta\x00more"), 0755); err != nil {
		t.Fatal(err)
	}
	// +x data without refs.
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("plain text\n"), 0755); err != nil {
		t.Fatal(err)
	}
	got := scanExecRefs(dir)
	if !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("scanExecRefs = %v, want [alpha beta]", got)
	}
}

func TestRewriteStagedRefs(t *testing.T) {
	isolate(t) // config.ExeDir points at throwaway state for expected paths
	staged := t.TempDir()
	for _, d := range []string{"usr/bin", "usr/lib/myapp", "usr/lib/myapp-extensions", "usr/share/myapp"} {
		if err := os.MkdirAll(filepath.Join(staged, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nexec helper /usr/lib/myapp/app.asar \"$@\"\nDATA=/usr/share/myapp/d\n"
	if err := os.WriteFile(filepath.Join(staged, "usr", "bin", "myapp"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "usr", "bin", "notes.txt"), []byte("see /usr/lib/myapp/x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var opts InstallOptions
	if err := rewriteStagedRefs(staged, "myapp", opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(staged, "usr", "bin", "myapp"))
	wantPayload := filepath.Join(config.ExeDir, "myapp", "pkg")
	for _, want := range []string{
		"exec helper " + filepath.Join(wantPayload, "usr", "lib", "myapp", "app.asar"),
		"DATA=" + filepath.Join(wantPayload, "usr", "share", "myapp", "d"),
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("rewritten script missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "helper /usr/lib") || strings.Contains(string(got), "DATA=/usr/") {
		t.Errorf("absolute payload ref survived:\n%s", got)
	}
	// Non-executable files are untouched.
	plain, _ := os.ReadFile(filepath.Join(staged, "usr", "bin", "notes.txt"))
	if !strings.Contains(string(plain), "/usr/lib/myapp/x") {
		t.Errorf("non-executable file was rewritten:\n%s", plain)
	}
	// Boundary: the extensions dir keeps its own name (longest-prefix rule
	// rewrites it to its own payload path, never truncated to myapp's).
	script2 := "#!/bin/sh\nexec \"$DIR/helper2\" --res /usr/lib/myapp-extensions/r\n"
	if err := os.WriteFile(filepath.Join(staged, "usr", "bin", "other"), []byte(script2), 0755); err != nil {
		t.Fatal(err)
	}
	if err := rewriteStagedRefs(staged, "myapp", opts); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(filepath.Join(staged, "usr", "bin", "other"))
	if !strings.Contains(string(got2), filepath.Join(wantPayload, "usr", "lib", "myapp-extensions", "r")) {
		t.Errorf("extensions dir rewritten wrong:\n%s", got2)
	}
	// Variable-indirect refs (electron35 style): the $VAR segment is kept,
	// only the /usr/<cls>/ prefix moves into the payload.
	script3 := "#!/usr/bin/bash\nname=myapp\nabr=$myapp\nexec /usr/lib/${name}/electron \"${flags[@]}\" \"$@\"\nexec /usr/lib/$name/electron\n"
	if err := os.WriteFile(filepath.Join(staged, "usr", "bin", "varref"), []byte(script3), 0755); err != nil {
		t.Fatal(err)
	}
	if err := rewriteStagedRefs(staged, "myapp", opts); err != nil {
		t.Fatal(err)
	}
	got3, _ := os.ReadFile(filepath.Join(staged, "usr", "bin", "varref"))
	for _, want := range []string{
		filepath.Join(wantPayload, "usr", "lib") + "/${name}/electron",
		filepath.Join(wantPayload, "usr", "lib") + "/$name/electron",
	} {
		if !strings.Contains(string(got3), want) {
			t.Errorf("variable ref not rewritten, missing %q:\n%s", want, got3)
		}
	}
	if strings.Contains(string(got3), "exec /usr/lib/$") {
		t.Errorf("variable host prefix survived:\n%s", got3)
	}
}
