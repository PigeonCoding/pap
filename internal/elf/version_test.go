package elf

import "testing"

func TestParseVerneedHost(t *testing.T) {
	vn, err := ParseVerneed("/usr/bin/curl")
	if err != nil {
		t.Fatal(err)
	}
	if len(vn) == 0 {
		t.Fatal("expected verneed entries")
	}
	found := false
	for _, v := range vn {
		if v.File == "libc.so.6" {
			for _, ver := range v.Versions {
				if ver.Name == "GLIBC_2.43" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected GLIBC_2.43 requirement, got %#v", vn)
	}
}

func TestParseVerdefHost(t *testing.T) {
	vd, err := ParseVerdef("/usr/lib/libc.so.6")
	if err != nil {
		t.Fatal(err)
	}
	if !vd["GLIBC_2.43"] || !vd["GLIBC_2.2.5"] {
		t.Fatalf("expected glibc version defs, got %d entries", len(vd))
	}
}
