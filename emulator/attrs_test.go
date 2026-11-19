package emulator

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func testFile(mode uint16) *attrContext {
	return &attrContext{fileInfo: fileInfo{mode: mode, size: 5000, ino: 77, mtime: fixedMtime}, p: &Process{}, path: "/Users/emu/dir/name.txt"}
}

// reference follows an attribute reference at off to the bytes it points at.
func reference(t *testing.T, buf []byte, off int) []byte {
	t.Helper()
	start := off + int(int32(le.Uint32(buf[off:])))
	end := start + int(le.Uint32(buf[off+4:]))
	if start < off+8 || end > len(buf) {
		t.Fatalf("reference at %d points at %d..%d of %d", off, start, end, len(buf))
	}
	return buf[start:end]
}

func TestPackAttrsLayout(t *testing.T) {
	// Returned set, name, object type, modification time and full path; a file's total size.
	want := [5]uint32{attrReturned | 0x1 | 0x8 | 0x400 | 0x8000000, 0, 0x2, 0x2, 0}
	out, ok := packAttrs(testFile(syscall.S_IFREG|0o644), want, 0)
	if !ok {
		t.Fatal("known attributes were refused")
	}
	if int(le.Uint32(out)) != len(out) || len(out)%4 != 0 {
		t.Fatalf("length field %d, buffer %d", le.Uint32(out), len(out))
	}
	// A file has no directory attributes, and says so.
	if got := le32(want[0], 0, 0, 0x2, 0); !bytes.Equal(out[4:24], got) {
		t.Fatalf("returned set % x, want % x", out[4:24], got)
	}
	if name := reference(t, out, 24); string(name) != "name.txt\x00" {
		t.Fatalf("name %q", name)
	}
	if kind := le.Uint32(out[32:]); kind != 1 {
		t.Fatalf("object type %d, want VREG", kind)
	}
	if sec := le.Uint64(out[36:]); sec != fixedMtime {
		t.Fatalf("modification time %d", sec)
	}
	if path := reference(t, out, 52); string(path) != "/Users/emu/dir/name.txt\x00" {
		t.Fatalf("path %q", path)
	}
	if size := le.Uint64(out[60:]); size != 5000 {
		t.Fatalf("total size %d", size)
	}
}

func TestPackAttrsDirectory(t *testing.T) {
	out, ok := packAttrs(testFile(syscall.S_IFDIR|0o755), [5]uint32{attrReturned | 0x8, 0, 0x1, 0x2, 0}, 0)
	if !ok {
		t.Fatal("known attributes were refused")
	}
	if got := le32(attrReturned|0x8, 0, 0x1, 0, 0); !bytes.Equal(out[4:24], got) {
		t.Fatalf("returned set % x, want % x", out[4:24], got)
	}
	if kind := le.Uint32(out[24:]); kind != 2 {
		t.Fatalf("object type %d, want VDIR", kind)
	}
}

// Finder info is something APFS has no value for: left out for a caller that asks what was
// returned, zeros for one that does not or that wants invalid attributes packed.
func TestPackAttrsAbsent(t *testing.T) {
	const finderInfo, devID = 0x4000, 0x2
	for _, c := range []struct {
		common  uint32
		options uint64
		size    int
	}{{attrReturned | finderInfo | devID, 0, 4 + 20 + 4}, {finderInfo | devID, 0, 4 + 32 + 4}, {attrReturned | finderInfo | devID, optPackInvalid, 4 + 20 + 32 + 4}} {
		out, ok := packAttrs(testFile(syscall.S_IFREG), [5]uint32{c.common}, c.options)
		if !ok || len(out) != c.size {
			t.Errorf("common %#x options %#x: %d bytes (ok %v), want %d", c.common, c.options, len(out), ok, c.size)
		}
	}
}

func TestPackAttrsUnknown(t *testing.T) {
	if _, ok := packAttrs(testFile(syscall.S_IFREG), [5]uint32{0, 0, 0, 0x100, 0}, 0); ok { // ATTR_FILE_FORKLIST
		t.Fatal("an attribute the kernel refuses was accepted")
	}
	// The extended common attributes only exist when the option asks for them.
	if _, ok := packAttrs(testFile(syscall.S_IFREG), [5]uint32{0, 0, 0, 0, 0x10}, 0); ok {
		t.Fatal("fork attribute accepted without FSOPT_ATTR_CMN_EXTENDED")
	}
	if out, ok := packAttrs(testFile(syscall.S_IFREG), [5]uint32{0, 0, 0, 0, 0x10}, optCmnExt); !ok || le.Uint64(out[4:]) != 77 {
		t.Fatal("link id missing with FSOPT_ATTR_CMN_EXTENDED")
	}
}

func TestVolumes(t *testing.T) {
	root, pre := volumeOf("/Users/emu"), volumeOf(preboot+"/Cryptexes/OS")
	if root.dev == pre.dev || root.mount != "/" || pre.mount != preboot || volumeOf(preboot+"x").dev != root.dev {
		t.Fatalf("volumes %+v %+v", root, pre)
	}
}

// The guest can change what is under its home and nothing else that a host file backs.
func TestOnlyTheHomeCanBeChanged(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "guest")
	if err := os.WriteFile(exe, []byte("not run"), 0o666); err != nil {
		t.Fatal(err)
	}
	p := &Process{home: t.TempDir(), exePath: exe, fds: map[uint64]*fd{}, nextFD: 3}
	if n := p.doOpen(GuestHome+"/file", 0x201, 0o644); n != 3 { // O_WRONLY|O_CREAT
		t.Fatalf("creating a file under the home: %d", n)
	}
	for name, errno := range map[string]int64{
		"open to truncate": p.doOpen(GuestExe, 0x401, 0), // O_WRONLY|O_TRUNC
		"unlink":           p.doUnlink(GuestExe, false),
		"rename over":      p.doRename(GuestHome+"/file", GuestExe),
		"rename away":      p.doRename(GuestExe, GuestHome+"/exe"),
		"unlink a device":  p.doUnlink("/dev/null", false),
	} {
		if errno >= 0 {
			t.Errorf("%s: %d, want an errno", name, errno)
		}
	}
	if data, err := os.ReadFile(exe); err != nil || string(data) != "not run" {
		t.Errorf("the guest executable is now %q (%v)", data, err)
	}
	if n := p.doOpen(GuestExe, 0, 0); n != 4 {
		t.Errorf("opening the guest executable to read: %d", n)
	}
	if errno := p.doRename(GuestHome+"/file", GuestHome+"/renamed"); errno != 0 {
		t.Errorf("rename under the home: %d", errno)
	}
}
