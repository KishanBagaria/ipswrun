package emulator

import (
	"bytes"
	"github.com/KishanBagaria/ipswrun/dyldcache"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
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

func testMountsProcess(t *testing.T, exe string, mounts []Mount) *Process {
	t.Helper()
	opened, err := openMounts(mounts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeMounts(opened) })
	return &Process{
		m:       &Machine{clock: &clock{boot: time.Now()}}, // newClock needs the host library loaded
		mounts:  opened,
		cwd:     "/",
		exePath: exe,
		path:    GuestExe,
		fds:     map[uint64]*fd{0: {kind: stdioFD, host: 0}, 1: {kind: stdioFD, host: 1}, 2: {kind: stdioFD, host: 2}},
		inodes:  map[string]uint64{},
		cache:   &dyldcache.Cache{},
	}
}

// The guest can change what is under its home and nothing else that a host file backs.
func TestOnlyTheHomeCanBeChanged(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "guest")
	if err := os.WriteFile(exe, []byte("not run"), 0o666); err != nil {
		t.Fatal(err)
	}
	p := testMountsProcess(t, exe, []Mount{{Guest: GuestHome, Host: t.TempDir()}})
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

// A mount is a second place the guest can change, and the directories on the way to it exist.
func TestMounts(t *testing.T) {
	host := t.TempDir()
	p := testMountsProcess(t, "", []Mount{{Guest: "/var/db/fpsd", Host: host}, {Guest: GuestHome, Host: t.TempDir()}})
	if errno := p.doMkdir("/var/db/fpsd/adi", 0o700); errno != 0 {
		t.Fatalf("mkdir under the mount: %d", errno)
	}
	if n := p.doOpen("/var/db/fpsd/adi/adi.pb", 0x201, 0o600); n != 3 { // O_WRONLY|O_CREAT
		t.Fatalf("creating a file under the mount: %d", n)
	}
	if _, err := os.Stat(filepath.Join(host, "adi", "adi.pb")); err != nil {
		t.Errorf("the file did not reach the host: %v", err)
	}
	if above, ok := p.resolve("/var/db"); !ok || above.kind != dirFD || above.root != nil {
		t.Errorf("the directory above the mount resolved to %+v, %v", above, ok)
	}
	for _, bad := range [][]Mount{{{Guest: "/", Host: host}}, {{Guest: "var/db", Host: host}}, {{Guest: GuestHome + "/", Host: host}}, {{Guest: "/var/db", Host: host}, {Guest: "/var/./db", Host: host}}} {
		if CheckMounts(bad, GuestHome) == nil {
			t.Errorf("mounts %v were accepted", bad)
		}
	}
	if err := CheckMounts([]Mount{{Guest: "/Users", Host: host}, {Guest: "/var/db", Host: host}}, GuestHome); err != nil {
		t.Errorf("a mount above the home, and another: %v", err)
	}
	for name, errno := range map[string]int64{
		"mkdir beside the mount": p.doMkdir("/var/db/other", 0o700),
		"open beside the mount":  p.doOpen("/var/db/fpsdx", 0x201, 0o600),
	} {
		if errno >= 0 {
			t.Errorf("%s: %d, want an errno", name, errno)
		}
	}
}

// Pre-existing symlinks inside a mount or home cannot escape outside the mount's os.Root.
func TestSymlinkContainment(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	outside := filepath.Join(parent, "secret.txt")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "inside.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secret.txt", filepath.Join(home, "rel-escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "abs-escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside.txt", filepath.Join(home, "good-link")); err != nil {
		t.Fatal(err)
	}

	p := testMountsProcess(t, "", []Mount{{Guest: GuestHome, Host: home}})
	for _, bad := range []string{GuestHome + "/rel-escape", GuestHome + "/abs-escape"} {
		if n := p.doOpen(bad, 0, 0); n >= 0 {
			t.Errorf("open(%s) succeeded with fd %d, want error", bad, n)
		}
		if _, _, errno := p.describePath("stat", bad, true); errno == 0 {
			t.Errorf("stat(%s) succeeded, want error", bad)
		}
		if errno := p.doAccess(bad); errno == 0 {
			t.Errorf("access(%s) succeeded, want error", bad)
		}
	}
	// An internal relative symlink stays inside the mount and works.
	if n := p.doOpen(GuestHome+"/good-link", 0, 0); n < 0 {
		t.Errorf("open(good-link) failed: %d", n)
	} else {
		p.closeFD(uint64(n))
	}
	// Outside file must remain untouched.
	if data, err := os.ReadFile(outside); err != nil || string(data) != "top-secret" {
		t.Fatalf("outside file changed: %q (%v)", data, err)
	}
}

// Allowed system directories list their entries instead of appearing empty.
func TestSystemDirListing(t *testing.T) {
	p := testMountsProcess(t, "", []Mount{{Guest: GuestHome, Host: t.TempDir()}})
	p.system = func(path string) (SystemFile, error) {
		switch path {
		case "/System/Library/FeatureFlags":
			return SystemFile{Dir: true, ReadDir: func() ([]string, error) { return []string{"Domain", "Global.plist"}, nil }}, nil
		case "/System/Library/FeatureFlags/Domain":
			return SystemFile{Dir: true, ReadDir: func() ([]string, error) { return []string{"CoreFoundation.plist"}, nil }}, nil
		case "/System/Library/FeatureFlags/Global.plist":
			return SystemFile{File: &fakeCacheFile{data: []byte("plist")}}, nil
		}
		return SystemFile{}, nil
	}
	fdn := p.doOpen("/System/Library/FeatureFlags", 0, 0)
	if fdn < 0 {
		t.Fatalf("open system dir: %d", fdn)
	}
	f := p.fds[uint64(fdn)]
	if errno := p.list(f); errno != 0 {
		t.Fatalf("list system dir: %d", errno)
	}
	want := []string{".", "..", "Domain", "Global.plist"}
	if !slices.Equal(f.names, want) {
		t.Fatalf("listed %v, want %v", f.names, want)
	}
	// ATTR_DIR_ENTRYCOUNT reflects the listed entries too.
	out, ok := packAttrs(&attrContext{fileInfo: fileInfo{mode: modeDir}, p: p, path: "/System/Library/FeatureFlags"}, [5]uint32{0, 0, 0x2, 0, 0}, 0)
	if !ok || le.Uint32(out[4:]) != 2 {
		t.Fatalf("ATTR_DIR_ENTRYCOUNT: ok=%v count=%d, want 2", ok, le.Uint32(out[4:]))
	}
}

type fakeCacheFile struct{ data []byte }

func (f *fakeCacheFile) Size() int64 { return int64(len(f.data)) }
func (f *fakeCacheFile) Pread(off, n int64) ([]byte, error) {
	if off >= int64(len(f.data)) {
		return nil, nil
	}
	return f.data[off:min(off+n, int64(len(f.data)))], nil
}

// A symbolic link of the system volume to a directory on the way to the shared cache is a link
// to lstat and readlink, as dyld's realpath walks it, and the directory to everything else.
func TestSystemLinkToCacheDirectory(t *testing.T) {
	p := newProcess(t)
	const link, real, text = "/System/Library/Frameworks/CoreServices.framework/Frameworks",
		"/System/Library/Frameworks/CoreServices.framework/Versions/A/Frameworks", "Versions/Current/Frameworks"
	p.system = func(path string) (SystemFile, error) {
		if path == link {
			return SystemFile{Dir: true, Real: real, Link: text}, nil
		}
		if path == real { // the directory itself is there: reading it as a link is EINVAL, not ENOENT
			return SystemFile{Dir: true}, nil
		}
		return SystemFile{}, nil
	}
	if _, info, errno := p.describePath("lstat", link, false); errno != 0 || info.mode != modeLink || info.size != int64(len(text)) {
		t.Fatalf("lstat: errno %d mode %#o size %d, want a link", errno, info.mode, info.size)
	}
	if target, info, errno := p.describePath("stat", link, true); errno != 0 || info.mode != modeDir || target.path != real {
		t.Fatalf("stat: errno %d mode %#o path %s, want the directory %s", errno, info.mode, target.path, real)
	}
	onMain(func() {
		buf := uint64(dataAddr)
		if n := p.doReadlink(link, buf, 64); n != int64(len(text)) || string(p.read(buf, uint64(n))) != text {
			t.Errorf("readlink: %d", n)
		}
		if n := p.doReadlink(real, buf, 64); n != -eINVAL {
			t.Errorf("readlink of the directory: %d, want EINVAL", n)
		}
	})
}

// A path of the data volume is read through to the release's template: a directory is given
// what the template has in it once, and what a guest then removes stays removed, for the next
// run too.
func TestDataVolume(t *testing.T) {
	template := map[string]TemplateEntry{
		".":                   {Dir: true, Names: []string{"Preferences", "zone"}},
		"Preferences":         {Dir: true, Names: []string{"Logging", "a.plist"}},
		"Preferences/Logging": {Dir: true},
		"Preferences/a.plist": {Size: 5, Inode: &Inode{Mode: 0o640}, Read: func() ([]byte, error) { return []byte("plist"), nil }},
		"zone":                {Link: "Preferences/Logging"},
	}
	asked := 0
	host, record := t.TempDir(), filepath.Join(t.TempDir(), "filled")
	mount := Mount{Guest: "/Library", Host: host, Record: record, Template: func(rel string) (TemplateEntry, error) {
		asked++
		return template[rel], nil
	}}
	p := testMountsProcess(t, "", []Mount{mount, {Guest: GuestHome, Host: t.TempDir()}})
	if _, err := os.Lstat(filepath.Join(host, "Preferences")); err == nil {
		t.Fatal("the template was copied before any path led into the volume")
	}
	p.resolve("/Library/Preferences/a.plist")
	if data, err := os.ReadFile(filepath.Join(host, "Preferences", "a.plist")); err != nil || string(data) != "plist" {
		t.Errorf("the template's file: %q, %v", data, err)
	}
	if st, err := os.Stat(filepath.Join(host, "Preferences", "a.plist")); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("the template's file has mode %v (%v)", st.Mode(), err)
	}
	if link, err := os.Readlink(filepath.Join(host, "zone")); err != nil || link != "Preferences/Logging" {
		t.Errorf("the template's link: %q, %v", link, err)
	}
	if st, err := os.Stat(filepath.Join(host, "Preferences", "Logging")); err != nil || !st.IsDir() {
		t.Errorf("the template's directory: %v", err)
	}
	// Removed, a file of the template's does not come back, and nothing is asked twice.
	if errno := p.doUnlink("/Library/Preferences/a.plist", false); errno != 0 {
		t.Fatalf("unlink: %d", errno)
	}
	before := asked
	if n := p.doOpen("/Library/Preferences/a.plist", 0, 0); n != -eNOENT {
		t.Errorf("opening what was removed: %d", n)
	}
	if asked != before {
		t.Errorf("the template was asked %d more times about directories it had filled", asked-before)
	}
	// Nor in a run after this one, which reads what was filled.
	again := testMountsProcess(t, "", []Mount{mount, {Guest: GuestHome, Host: t.TempDir()}})
	before = asked
	if n := again.doOpen("/Library/Preferences/a.plist", 0, 0); n != -eNOENT || asked != before {
		t.Errorf("in the next run: open %d, the template asked %d times", n, asked-before)
	}
	// The root's links to /private are links, and lead there.
	if _, info, errno := p.describePath("lstat", "/var", false); errno != 0 || info.mode != modeLink {
		t.Errorf("lstat /var: mode %o errno %d", info.mode, errno)
	}
	if got := p.absolute("/var/db/x"); got != "/private/var/db/x" {
		t.Errorf("/var/db/x is %s", got)
	}
}
