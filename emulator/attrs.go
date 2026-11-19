package emulator

import (
	"os"
	pathpkg "path"
	"strings"
	"syscall"
)

// fileInfo is what the emulator knows about a file, however the guest asks: stat or getattrlist.
type fileInfo struct {
	mode  uint16
	size  int64
	ino   uint64
	mtime int64
}

// describe looks at a resolved path or an open descriptor. Paths with a pinned inode number
// report it; hostStat is consulted for a hostFD only. The inode number is remembered, so that
// fsgetpath can turn it back into the path.
func (p *Process) describe(kind fdKind, path string, size int64, hostStat func(*syscall.Stat_t) error) (info fileInfo, errno int64) {
	ino := p.inodes[path]
	if path == volumeOf(path).mount {
		ino = rootInode
	}
	switch kind {
	case hostFD:
		var st syscall.Stat_t
		if err := hostStat(&st); err != nil {
			return fileInfo{}, hostErrno(err)
		}
		if ino == 0 {
			ino = st.Ino & 0xFFFFFFFF
		}
		info = fileInfo{uint16(st.Mode), st.Size, ino, mtimeSeconds(&st)}
	case dirFD:
		if ino == 0 {
			ino = crcInode(path)
		}
		info = fileInfo{modeDir, 0, ino, fixedMtime}
	default:
		if ino == 0 {
			ino = crcInode(path)
		}
		info = fileInfo{modeFile, size, ino, fixedMtime}
	}
	if path != "" {
		p.seen[fileID{volumeOf(path).dev, ino}] = path
	}
	return info, 0
}

// fileID identifies a file the way fsgetpath is asked for it.
type fileID struct {
	dev uint32
	ino uint64
}

const rootInode = 2 // of every volume's root directory

// attr is one attribute of getattrlist: its bit, its size (0: variable, returned through a
// reference) and its value. An attribute without a value is zeros, or an empty string.
type attr struct {
	bit   uint32
	size  int
	value func(c *attrContext) []byte
}

// absent is the size of an attribute APFS has no value for. A caller that asks which attributes
// were returned does not get it; any other gets zeros of the size its group's table gives.
const absent = -1

// attrContext is the file a getattrlist call describes.
type attrContext struct {
	fileInfo
	p    *Process
	path string // canonical guest path
	host string // host path, for a file under the home
}

func (c *attrContext) isDir() bool { return c.mode&syscall.S_IFMT == syscall.S_IFDIR }

func str(s string) []byte { return append([]byte(s), 0) }

func timespec(sec int64) []byte { return le64(uint64(sec), 0) }

var (
	attrTime  = func(c *attrContext) []byte { return timespec(c.mtime) }
	attrFSID  = func(c *attrContext) []byte { return le32(volumeOf(c.path).dev, fsTypeAPFS) }
	attrDevID = func(c *attrContext) []byte { return le32(volumeOf(c.path).dev) }
	attrSize  = func(c *attrContext) []byte { return le64(uint64(c.size)) }
	attrAlloc = func(c *attrContext) []byte { return le64(align(uint64(c.size), 4096)) }
	attrBlock = func(*attrContext) []byte { return le32(4096) }
	attrIno   = func(c *attrContext) []byte { return le64(c.ino) }
	attrPath  = func(c *attrContext) []byte { return str(c.path) }
	attrObjID = func(c *attrContext) []byte { return le32(uint32(c.ino), 0) }
	attrPar32 = func(c *attrContext) []byte { return le32(uint32(crcInode(pathpkg.Dir(c.path))), 0) }
)

const (
	fsTypeAPFS = 0x1a

	attrReturned   = 0x80000000 // ATTR_CMN_RETURNED_ATTRS
	attrVolInfo    = 0x80000000 // ATTR_VOL_INFO
	optNoFollow    = 0x1        // FSOPT_NOFOLLOW
	optFullSize    = 0x4        // FSOPT_REPORT_FULLSIZE
	optPackInvalid = 0x8        // FSOPT_PACK_INVAL_ATTRS
	optCmnExt      = 0x20       // FSOPT_ATTR_CMN_EXTENDED: the fork attributes are the extended common ones
	volumeBlocks   = 1 << 28
	volumeFree     = 1 << 27
	volumeObjects  = 1 << 20
)

// The five attribute groups, in the order their values are packed.
var (
	commonAttrs = []attr{
		{0x1, 0, func(c *attrContext) []byte { return str(pathpkg.Base(c.path)) }}, // name
		{0x2, 4, attrDevID}, {0x4, 8, attrFSID},
		{0x8, 4, func(c *attrContext) []byte { // object type: VREG, VDIR or VLNK
			switch c.mode & syscall.S_IFMT {
			case syscall.S_IFDIR:
				return le32(2)
			case syscall.S_IFLNK:
				return le32(5)
			}
			return le32(1)
		}},
		{0x10, 4, func(*attrContext) []byte { return le32(fsTypeAPFS) }}, // object tag
		{0x20, 8, attrObjID}, {0x40, 8, attrObjID}, {0x80, 8, attrPar32},
		{0x100, 4, nil}, // script
		{0x200, 16, attrTime}, {0x400, 16, attrTime}, {0x800, 16, attrTime}, {0x1000, 16, attrTime},
		{0x2000, 16, nil}, {0x4000, absent, nil}, // backup time, Finder info
		{0x8000, 4, func(*attrContext) []byte { return le32(guestUID) }},
		{0x10000, 4, func(*attrContext) []byte { return le32(guestGID) }},
		{0x20000, 4, func(c *attrContext) []byte { return le32(uint32(c.mode)) }},
		{0x40000, 4, nil}, {0x80000, 4, nil}, {0x100000, 4, nil}, // flags, generation, document id
		{0x200000, 4, func(c *attrContext) []byte { // user access: what the owner bits allow
			return le32(uint32(c.mode) >> 6 & 7)
		}},
		{0x400000, absent, nil},                   // extended security
		{0x800000, 16, nil}, {0x1000000, 16, nil}, // owner and group UUIDs
		{0x2000000, 8, attrIno},
		{0x4000000, 8, func(c *attrContext) []byte { return le64(crcInode(pathpkg.Dir(c.path))) }},
		{0x8000000, 0, attrPath}, {0x10000000, 16, attrTime},
		{0x20000000, absent, nil}, {0x40000000, 4, nil}, // error (bulk only), data protection
	}
	volumeAttrs = []attr{
		{0x1, 4, func(*attrContext) []byte { return le32(fsTypeAPFS) }}, {0x2, 4, nil},
		{0x4, 8, func(*attrContext) []byte { return le64(volumeBlocks * 4096) }},
		{0x8, 8, func(*attrContext) []byte { return le64(volumeFree * 4096) }},
		{0x10, 8, func(*attrContext) []byte { return le64(volumeFree * 4096) }},
		{0x20, 8, func(*attrContext) []byte { return le64(4096) }}, {0x40, 8, func(*attrContext) []byte { return le64(4096) }},
		{0x80, 4, attrBlock},
		{0x100, 4, func(*attrContext) []byte { return le32(volumeObjects / 2) }}, {0x200, 4, nil}, {0x400, 4, nil},
		{0x800, 4, func(*attrContext) []byte { return le32(volumeObjects) }},
		{0x1000, 0, func(c *attrContext) []byte { return str(volumeOf(c.path).mount) }}, {0x2000, 0, func(*attrContext) []byte { return str("Macintosh HD") }},
		{0x4000, 4, nil}, {0x8000, 0, func(c *attrContext) []byte { return str(volumeOf(c.path).device) }}, {0x10000, absent, nil},
		// Capabilities, then which of them are valid: a case-preserving volume with persistent 64-bit
		// object ids, links, sparse files and attribute lists; nothing that renames atomically in
		// more than one step, so callers fall back to plain rename.
		{0x20000, 32, func(*attrContext) []byte { return le32(0x26E47, 0x2, 0, 0, 0xFFFFFFFF, 0xFFFFFFFF, 0, 0) }},
		{0x40000, 16, func(*attrContext) []byte { return []byte("ipswrun volume \x00") }},
		{0x80000, 4, nil}, {0x100000, 0, func(*attrContext) []byte { return str("apfs") }}, // extended mount flags, type name
		{0x200000, 4, nil}, {0x400000, 4, nil}, // subtype, owner
		{0x800000, 8, func(*attrContext) []byte { return le64((volumeBlocks - volumeFree) * 4096) }},
		{0x10000000, 8, nil}, {0x20000000, 8, nil}, // quota and reserved sizes
		{0x40000000, 40, nil}, // attributes that are valid, then those that are native: set in init
	}
	dirAttrs = []attr{
		{0x1, 4, func(*attrContext) []byte { return le32(1) }},
		{0x2, 4, func(c *attrContext) []byte {
			entries, _ := os.ReadDir(c.host)
			return le32(uint32(len(entries)))
		}},
		{0x4, 4, nil}, {0x8, 8, nil}, {0x10, 4, attrBlock}, {0x20, 8, nil},
	}
	fileAttrs = []attr{
		{0x1, 4, func(*attrContext) []byte { return le32(1) }}, {0x2, 8, attrSize}, {0x4, 8, attrAlloc}, {0x8, 4, attrBlock},
		{0x10, absent, nil}, {0x20, 4, nil}, {0x200, 8, attrSize}, {0x400, 8, attrAlloc}, {0x1000, 8, nil}, {0x2000, 8, nil},
	}
	extendedAttrs = []attr{
		{0x4, 0, func(c *attrContext) []byte { return str(strings.TrimPrefix(c.path, "/")) }}, {0x8, 8, attrAlloc}, {0x10, 8, attrIno},
		{0x20, 0, attrPath}, {0x40, 4, attrDevID}, {0x80, 8, attrFSID}, {0x100, 8, nil}, {0x200, 8, nil}, {0x400, 8, nil},
		{0x800, 8, nil}, {0x1000, 4, nil},
	}
)

// absentSizes are the sizes of the attributes that are absent, by group: Finder info, extended
// security (a reference) and the bulk error; the encodings a volume used; a file's clump size.
var absentSizes = [5]map[uint32]int{{0x4000: 32, 0x400000: 8, 0x20000000: 4}, {0x10000: 8}, nil, {0x10: 4}, nil}

// The volume says which attributes it supports: all the emulator knows.
func init() {
	set := [5]uint32{attrReturned, attrVolInfo}
	for i, group := range [][]attr{commonAttrs, volumeAttrs, dirAttrs, fileAttrs} {
		for _, a := range group {
			set[i] |= a.bit
		}
	}
	supported := append(le32(set[:]...), le32(set[:]...)...)
	volumeAttrs[len(volumeAttrs)-1].value = func(*attrContext) []byte { return supported }
}

// packAttrs is the attribute buffer for one file: its length, the attributes that were returned
// (if asked for), the fixed-size values, then what the references point at. ok is false when the
// list names an attribute the emulator does not know.
func packAttrs(c *attrContext, want [5]uint32, options uint64) (out []byte, ok bool) {
	groups := [5][]attr{commonAttrs, nil, nil, nil, nil}
	if want[1] != 0 {
		groups[1] = volumeAttrs
	}
	if c.isDir() { // a file has no directory attributes and a directory no file attributes
		groups[2] = dirAttrs
	} else {
		groups[3] = fileAttrs
	}
	if options&optCmnExt != 0 {
		groups[4] = extendedAttrs
	}
	type reference struct {
		at   int
		data []byte
	}
	var fixed, variable []byte
	var references []reference
	var returned [5]uint32
	for g, group := range groups {
		known := uint32(0)
		for _, a := range group {
			known |= a.bit
			if want[g]&a.bit == 0 {
				continue
			}
			if a.size == absent {
				if want[0]&attrReturned == 0 || options&optPackInvalid != 0 {
					fixed = append(fixed, make([]byte, absentSizes[g][a.bit])...)
					returned[g] |= a.bit
				}
				continue
			}
			returned[g] |= a.bit
			var value []byte
			if a.value != nil {
				value = a.value(c)
			}
			if a.size == 0 {
				references = append(references, reference{len(fixed), value})
				fixed = append(fixed, make([]byte, 8)...)
				continue
			}
			fixed = append(fixed, append(value, make([]byte, a.size)...)[:a.size]...)
		}
		skipped := map[int]uint32{0: attrReturned, 1: attrVolInfo}[g]
		if g == 2 && !c.isDir() || g == 3 && c.isDir() {
			skipped = ^uint32(0)
		}
		if want[g]&^known&^skipped != 0 {
			c.p.note("  getattrlist: unknown attribute %#x in group %d", want[g]&^known&^skipped, g)
			return nil, false
		}
	}
	head := 4
	if want[0]&attrReturned != 0 {
		returned[0] |= attrReturned
		head += 20
	}
	for _, r := range references {
		at := head + r.at
		le.PutUint32(fixed[r.at:], uint32(head+len(fixed)+len(variable)-at))
		le.PutUint32(fixed[r.at+4:], uint32(len(r.data)))
		variable = append(variable, append(r.data, 0, 0, 0)[:align(uint64(len(r.data)), 4)]...)
	}
	out = le32(uint32(head + len(fixed) + len(variable)))
	if head > 4 {
		out = append(out, le32(returned[:]...)...)
	}
	return append(append(out, fixed...), variable...), true
}

// attrList reads the guest's struct attrlist.
func (p *Process) attrList(addr uint64) (want [5]uint32, ok bool) {
	raw := p.read(addr, 24)
	for i := range want {
		want[i] = le.Uint32(raw[4+4*i:])
	}
	return want, le.Uint16(raw) == 5
}

// getattr answers getattrlist for one file. A buffer that is too small gets what fits.
func (p *Process) getattr(c *attrContext, a []uint64) int64 {
	list, buf, size, options := a[0], a[1], a[2], a[3]
	want, ok := p.attrList(list)
	if !ok || want[1] != 0 && want[1]&attrVolInfo == 0 || want[4] != 0 && options&optCmnExt == 0 {
		return -eINVAL
	}
	p.note("  getattrlist %s %#x options %#x", c.path, want, options)
	out, ok := packAttrs(c, want, options)
	if !ok {
		return -eINVAL
	}
	if options&optFullSize == 0 {
		le.PutUint32(out, uint32(min(uint64(len(out)), size)))
	}
	p.write(buf, out[:min(uint64(len(out)), size)])
	return 0
}

func (p *Process) getattrPath(path string, a []uint64) int64 {
	t, ok := p.resolve(path)
	if !ok {
		p.note("  ENOENT getattrlist %s", path)
		return -eNOENT
	}
	info, errno := p.describe(t.kind, t.path, t.fd().size(), func(st *syscall.Stat_t) error {
		if a[3]&optNoFollow != 0 {
			return syscall.Lstat(t.host, st)
		}
		return syscall.Stat(t.host, st)
	})
	if errno != 0 {
		return errno
	}
	return p.getattr(&attrContext{fileInfo: info, p: p, path: t.path, host: t.host}, a)
}

func sysGetattrlist(p *Process, a []uint64) int64   { return p.getattrPath(p.cstr(a[0]), a[1:]) }
func sysGetattrlistat(p *Process, a []uint64) int64 { return p.getattrPath(p.at(a[0], a[1]), a[2:]) }

func sysFgetattrlist(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	info, errno := p.describe(f.kind, f.path, f.size(), func(st *syscall.Stat_t) error { return syscall.Fstat(f.host, st) })
	if errno != 0 {
		return errno
	}
	host, _ := p.hostPath(f.path)
	return p.getattr(&attrContext{fileInfo: info, p: p, path: f.path, host: host}, a[1:])
}

// sysGetattrlistbulk returns the attributes of as many of a directory's entries as fit, and how
// many those were; zero ends the listing.
func sysGetattrlistbulk(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	want, ok := p.attrList(a[1])
	if !ok || want[0]&attrReturned == 0 || want[1] != 0 {
		return -eINVAL
	}
	if errno := p.list(f); errno != 0 {
		return errno
	}
	f.pos = max(f.pos, 2) // "." and ".." are not entries here
	var out []byte
	count := int64(0)
	for ; f.pos < int64(len(f.names)); f.pos++ {
		t, _ := p.resolve(pathpkg.Join(f.path, f.names[f.pos]))
		info, errno := p.describe(t.kind, t.path, 0, func(st *syscall.Stat_t) error { return syscall.Lstat(t.host, st) })
		if errno != 0 {
			continue // gone since the directory was read
		}
		entry, ok := packAttrs(&attrContext{fileInfo: info, p: p, path: t.path, host: t.host}, want, a[4])
		if !ok {
			return -eINVAL
		}
		if uint64(len(out)+len(entry)) > a[3] {
			break
		}
		out = append(out, entry...)
		count++
	}
	p.write(a[2], out)
	return count
}
