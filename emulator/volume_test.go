package emulator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOneDataVolume(t *testing.T) {
	// The home and the paths of the data volume are one volume, as on a Mac: a file is moved
	// and linked between them (tests/kernel/files/datavolume.c). A mount that is neither is its own.
	home, tmp, other := t.TempDir(), t.TempDir(), t.TempDir()
	p := testMountsProcess(t, "", []Mount{
		{Guest: GuestHome, Host: home, Data: true},
		{Guest: "/private/tmp", Host: tmp, Data: true},
		{Guest: "/Volumes/other", Host: other},
	})
	if err := os.WriteFile(filepath.Join(tmp, "made"), []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := p.doRename("/private/tmp/made", GuestHome+"/moved"); r != 0 {
		t.Fatalf("a move from /private/tmp to the home: %d", r)
	}
	if held, err := os.ReadFile(filepath.Join(home, "moved")); err != nil || string(held) != "held" {
		t.Fatalf("the moved file: %q, %v", held, err)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "made")); err == nil {
		t.Error("the file is still where it was moved from")
	}
	if r := p.doLink(GuestHome+"/moved", "/private/tmp/linked", false); r != 0 {
		t.Fatalf("a second name in /private/tmp: %d", r)
	}
	a, _ := os.Stat(filepath.Join(home, "moved"))
	b, _ := os.Stat(filepath.Join(tmp, "linked"))
	if a == nil || b == nil || !os.SameFile(a, b) {
		t.Error("the second name is of another file")
	}
	for what, got := range map[string]int64{
		"a move to a mount that is its own volume": p.doRename(GuestHome+"/moved", "/Volumes/other/moved"),
		"a link to one":  p.doLink(GuestHome+"/moved", "/Volumes/other/linked", false),
		"a clone to one": p.clone(GuestHome+"/moved", "/Volumes/other/cloned", 0, nil),
	} {
		if got != -eXDEV {
			t.Errorf("%s: %d, want EXDEV", what, got)
		}
	}
}
