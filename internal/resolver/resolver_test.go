package resolver

import (
	"testing"
)

func TestBuildSkipLibsArchAware(t *testing.T) {
	x86 := BuildSkipLibs("x86_64")
	if !x86["ld-linux-x86-64.so.2"] || !x86["libc.so.6"] {
		t.Fatalf("x86_64 skips missing loader/libc: %v", x86)
	}
	arm := BuildSkipLibs("aarch64")
	if !arm["ld-linux-aarch64.so.1"] {
		t.Fatalf("aarch64 skips missing loader: %v", arm)
	}
	for _, s := range []string{"libutil.so.1", "libcrypt.so.1", "libnsl.so.1", "libmvec.so.1", "libthread_db.so.1"} {
		if !x86[s] {
			t.Errorf("x86_64 skips missing %s", s)
		}
	}
	// Isolatable libs must NOT be skipped.
	for _, s := range []string{"libstdc++.so.6", "libgcc_s.so.1"} {
		if x86[s] {
			t.Errorf("must not skip %s", s)
		}
	}
}

func TestSkipNSSWildcard(t *testing.T) {
	for _, s := range []string{"libnss_files.so.2", "libnss_dns.so.2", "libnss_myhostname.so.2"} {
		if !Skip(s) {
			t.Errorf("Skip(%q) = false, want true", s)
		}
	}
	if Skip("libcurl.so.4") {
		t.Error("Skip(libcurl.so.4) = true, want false")
	}
}
