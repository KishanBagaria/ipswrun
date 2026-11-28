package emulator

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClone(t *testing.T) {
	// A clone is a file of its own with what the first held, its mode, its date and its
	// attributes, and the owner it was given (tests/kernel/clone.c prints the same natively).
	p := newProcess(t)
	onMain(func() {
		home := p.mounts[0].root.Name()
		first := filepath.Join(home, "first")
		if err := os.WriteFile(first, []byte("held"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chmod(first, 0o754)
		os.Chtimes(first, time.Time{}, time.Unix(2000, 0))
		_, id, errno := p.changeable(GuestHome+"/first", true)
		if errno != 0 {
			t.Fatalf("the first file: errno %d", -errno)
		}
		p.m.xattrs[id] = map[string][]byte{"note": []byte("kept")}
		p.m.own(id, Owner{0, 0})

		if r := p.clone(GuestHome+"/first", GuestHome+"/copy", 0, nil); r != 0 {
			t.Fatalf("clone: %d", r)
		}
		st, err := os.Stat(filepath.Join(home, "copy"))
		held, _ := os.ReadFile(filepath.Join(home, "copy"))
		if err != nil || string(held) != "held" || st.Mode().Perm() != 0o754 || st.ModTime().Unix() != 2000 {
			t.Fatalf("the copy: %q %v %v", held, st, err)
		}
		_, copied, _ := p.changeable(GuestHome+"/copy", true)
		if copied == id || string(p.m.xattrs[copied]["note"]) != "kept" || p.m.owned(copied) != (Owner{0, 0}) {
			t.Fatalf("the copy's attributes %v, owner %v", p.m.xattrs[copied], p.m.owned(copied))
		}
		p.m.xattrs[copied]["note"][0] = 'K'
		if string(p.m.xattrs[id]["note"]) != "kept" {
			t.Fatal("the copy's attribute is the first's")
		}
		for what, c := range map[string]struct {
			to    string
			flags uint64
			want  int64
		}{
			"over a file":          {"/copy", 0, -eEXIST},
			"a flag there is not":  {"/other", 0x8000, -eINVAL},
			"not the owner's copy": {"/mine", cloneNoOwnerCopy, 0},
		} {
			if got := p.clone(GuestHome+"/first", GuestHome+c.to, c.flags, nil); got != c.want {
				t.Errorf("%s: %d, want %d", what, got, c.want)
			}
		}
		if _, mine, _ := p.changeable(GuestHome+"/mine", true); p.m.owned(mine) != (Owner{p.m.user.uid, p.m.user.gid}) {
			t.Errorf("a copy made without its owner is %v's", p.m.owned(mine))
		}
	})
}
