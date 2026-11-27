package emulator

import (
	pathpkg "path"
	"strings"
	"syscall"
)

// fileInfo is what the emulator knows about a file, however the guest asks: stat or getattrlist.
type fileInfo struct {
	dev   uint32
	mode  uint16
	size  int64
	ino   uint64
	mtime int64
	links uint16 // the names it has, if more than the one
	owner *Owner // if it is not the guests' user's (see Machine.owners)
	flags uint32 // those it carries beside its mode (see chflags)
	acl   bool   // it has an access control list (see acls.go)
}

// describe looks at a resolved path or an open descriptor. Paths with a pinned inode number
// report it; hostStat is consulted for a hostFD only. The inode number is remembered, so that
// fsgetpath can turn it back into the path.
func (p *Process) describe(kind fdKind, path string, size int64, inode *Inode, hostStat func(*syscall.Stat_t) error) (fileInfo, int64) {
	v := volumeOf(path)
	info := fileInfo{dev: v.dev, mode: modeFile, size: size, ino: p.inodes[path], mtime: fixedMtime}
	if path == v.mount {
		info.ino = rootInode
	}
	switch kind {
	case hostFD, stdioFD:
		var st syscall.Stat_t
		if err := hostStat(&st); err != nil {
			return fileInfo{}, hostErrno(err)
		}
		info.mode, info.size, info.mtime = uint16(st.Mode), st.Size, mtimeSeconds(&st)
		switch st.Mode & syscall.S_IFMT {
		case syscall.S_IFLNK:
			info.mode = modeLink // as a Mac's file system has every link, whatever the host's says
			fallthrough
		case syscall.S_IFREG:
			info.links = uint16(st.Nlink)
		}
		id := hostFileOf(&st)
		if p.m.fifos[id] { // a named pipe, which the host keeps as a file (see sysMkfifo)
			info.mode = syscall.S_IFIFO | info.mode&0o7777
		}
		if owner, ok := p.m.owners[id]; ok {
			info.owner = &owner
		}
		info.flags = p.m.flags[id]
		info.acl = p.m.acls[id] != nil
		if mode, ok := p.m.said[id]; ok {
			info.mode = info.mode&syscall.S_IFMT | mode
		}
		if info.ino == 0 {
			info.ino = st.Ino & 0xFFFFFFFF
		}
	case dirFD:
		info.mode, info.size = modeDir, 0
	}
	if inode != nil && kind != hostFD && kind != stdioFD { // as the release's volume has it
		info.mode = info.mode&syscall.S_IFMT | inode.Mode&0o7777
		info.owner = &Owner{inode.UID, inode.GID}
	}
	if info.ino == 0 {
		info.ino = crcInode(path)
	}
	if path != "" {
		p.m.saw(fileID{info.dev, info.ino}, path)
	}
	return info, 0
}

// describePath describes what a guest path names, following a final symbolic link or not. op is
// the system call, for the note when the path names nothing.
func (p *Process) describePath(op, path string, follow bool) (target, fileInfo, int64) {
	if n, ok := p.descriptorAt(path); ok { // what the descriptor is, followed or not
		f := p.fds[n]
		if f == nil {
			return target{}, fileInfo{}, -eBADF
		}
		info, errno := p.describeFD(f)
		return target{kind: f.kind, path: f.path}, info, errno
	}
	if link := p.rootLink(path); link != "" && !follow {
		t := target{kind: dirFD, path: pathpkg.Clean(path), link: link}
		t.inode = p.systemFile(p.absoluteLink(path)).LinkInode
		info, errno := p.describe(t.kind, t.path, 0, t.inode, nil)
		info.mode, info.size = linkMode(t.inode), int64(len(link))
		return t, info, errno
	}
	t, ok := p.resolve(path)
	if follow {
		t, ok = p.followed(t, ok)
	}
	if !ok {
		p.note("  ENOENT %s %s", op, path)
		return t, fileInfo{}, -eNOENT
	}
	if t.link != "" && !follow { // the link itself, not the directory it leads to
		t = target{kind: t.kind, path: p.absolute(path), link: t.link, inode: t.linkInode}
		if t.kind == hostFD { // it leads off the volume to a host file, and is none itself
			t.kind = dirFD
		}
		info, errno := p.describe(t.kind, t.path, 0, t.inode, nil)
		info.mode, info.size = linkMode(t.inode), int64(len(t.link))
		return t, info, errno
	}
	info, errno := p.describe(t.kind, t.path, t.fd().size(), t.inode, func(st *syscall.Stat_t) error {
		if t.root != nil {
			return statInRoot(t.root, t.rel, follow, st)
		}
		if follow {
			return syscall.Stat(t.host, st)
		}
		return syscall.Lstat(t.host, st)
	})
	return t, info, errno
}

// linkMode is the mode of a symbolic link: the volume's own for it, if it is one of a volume's.
func linkMode(inode *Inode) uint16 {
	if inode == nil {
		return modeLink
	}
	return syscall.S_IFLNK | inode.Mode&0o7777
}

func (p *Process) describeFD(f *fd) (fileInfo, int64) {
	return p.describe(f.kind, f.path, f.size(), f.inode, func(st *syscall.Stat_t) error { return syscall.Fstat(f.host, st) })
}

// fileID identifies a file the way fsgetpath is asked for it.
type fileID struct {
	dev uint32
	ino uint64
}

const rootInode = 2 // of every volume's root directory

// attr is one attribute of getattrlist: its bit, its size (0: variable, returned through a
// reference) and its value. An attribute without a value is zeros, or an empty string.
//
// A negative size is that of an attribute APFS has no value for. A caller that asks which
// attributes were returned does not get it; any other gets that many zeros.
type attr struct {
	bit   uint32
	size  int
	value func(c *attrContext) []byte
}

// attrContext is the file a getattrlist call describes.
type attrContext struct {
	fileInfo
	p    *Process
	path string // canonical guest path
}

// volume is the volume the file is on, as statfs names it.
func (c *attrContext) volume() volume { return c.p.volumeAt(c.path) }

func (c *attrContext) isDir() bool { return c.mode&syscall.S_IFMT == syscall.S_IFDIR }

// attrFinderInfo is a file's Finder info, which is its attribute com.apple.FinderInfo: nil if
// it has none, which is all zeros to those who ask.
func attrFinderInfo(c *attrContext) []byte {
	if c.p.m == nil {
		return nil
	}
	_, id, errno := c.p.changeable(c.path, true)
	if errno != 0 {
		return nil
	}
	return c.p.m.xattrs[id][finderInfo]
}

// always is the value of an attribute that is the same for every file.
func always(b []byte) func(*attrContext) []byte {
	b = b[:len(b):len(b)] // shared, so never appended to in place
	return func(*attrContext) []byte { return b }
}

var (
	attrTime  = func(c *attrContext) []byte { return le64(uint64(c.mtime), 0) } // a timespec
	attrFSID  = func(c *attrContext) []byte { return le32(c.dev, fsTypeAPFS) }
	attrDevID = func(c *attrContext) []byte { return le32(c.dev) }
	attrSize  = func(c *attrContext) []byte { return le64(uint64(c.size)) }
	attrAlloc = func(c *attrContext) []byte { return le64(align(uint64(c.size), volumeBlockSize)) }
	attrBlock = always(le32(volumeBlockSize))
	attrOne   = always(le32(1)) // link count
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

	volumeBlockSize = 4096
	volumeBlocks    = 1 << 28
	volumeFree      = 1 << 27
	volumeObjects   = 1 << 20
)

// The five attribute groups, in the order their values are packed.
var (
	commonAttrs = []attr{
		{0x1, 0, func(c *attrContext) []byte { return str(pathpkg.Base(c.path)) }}, // name
		{0x2, 4, attrDevID}, {0x4, 8, attrFSID},
		{0x8, 4, func(c *attrContext) []byte { // object type: VREG, VDIR, VLNK or VFIFO
			switch c.mode & syscall.S_IFMT {
			case syscall.S_IFDIR:
				return le32(2)
			case syscall.S_IFLNK:
				return le32(5)
			case syscall.S_IFIFO:
				return le32(7)
			}
			return le32(1)
		}},
		{0x10, 4, always(le32(fsTypeAPFS))}, // object tag
		{0x20, 8, attrObjID}, {0x40, 8, attrObjID}, {0x80, 8, attrPar32},
		{0x100, 4, nil}, // script
		{0x200, 16, attrTime}, {0x400, 16, attrTime}, {0x800, 16, attrTime}, {0x1000, 16, attrTime},
		{0x2000, 16, nil}, {0x4000, -32, attrFinderInfo}, // backup time, and Finder info if it is set
		{0x8000, 4, func(c *attrContext) []byte { return le32(c.p.m.ownerOf(c.fileInfo).UID) }},
		{0x10000, 4, func(c *attrContext) []byte { return le32(c.p.m.ownerOf(c.fileInfo).GID) }},
		{0x20000, 4, func(c *attrContext) []byte { return le32(uint32(c.mode)) }},
		{0x40000, 4, func(c *attrContext) []byte { return le32(c.flags) }}, // flags
		{0x80000, 4, nil}, {0x100000, 4, nil}, // generation, document id
		{0x200000, 4, func(c *attrContext) []byte { // user access: what the owner bits allow
			return le32(uint32(c.mode) >> 6 & 7)
		}},
		{0x400000, -8, func(c *attrContext) []byte { // extended security (a reference): none
			if c.acl {
				c.p.unimplemented("getattrlist of the security of a file that has an access control list", nil)
			}
			return nil
		}},
		{0x800000, 16, nil}, {0x1000000, 16, nil}, // owner and group UUIDs
		{0x2000000, 8, attrIno},
		{0x4000000, 8, func(c *attrContext) []byte { return le64(crcInode(pathpkg.Dir(c.path))) }},
		{0x8000000, 0, attrPath}, {0x10000000, 16, attrTime},
		{0x20000000, -4, nil}, {0x40000000, 4, nil}, // error (bulk only), data protection
	}
	volumeAttrs = []attr{
		{0x1, 4, always(le32(fsTypeAPFS))}, {0x2, 4, nil},
		{0x4, 8, always(le64(volumeBlocks * volumeBlockSize))},
		{0x8, 8, always(le64(volumeFree * volumeBlockSize))},
		{0x10, 8, always(le64(volumeFree * volumeBlockSize))},
		{0x20, 8, always(le64(volumeBlockSize))}, {0x40, 8, always(le64(volumeBlockSize))},
		{0x80, 4, attrBlock},
		{0x100, 4, always(le32(volumeObjects / 2))}, {0x200, 4, nil}, {0x400, 4, nil},
		{0x800, 4, always(le32(volumeObjects))},
		// Where it is mounted, its name, how it is mounted and from what: statfs's own answers.
		{0x1000, 0, func(c *attrContext) []byte { return str(c.volume().mount) }},
		{0x2000, 0, func(c *attrContext) []byte { return str(c.volume().name) }},
		{0x4000, 4, func(c *attrContext) []byte { return le32(c.volume().flags) }},
		{0x8000, 0, func(c *attrContext) []byte { return str(c.volume().device) }}, {0x10000, -8, nil},
		// Capabilities, then which of them are valid: a case-preserving volume with persistent 64-bit
		// object ids, links, sparse files and attribute lists; nothing that renames atomically in
		// more than one step, so callers fall back to plain rename.
		{0x20000, 32, always(le32(0x26E47, 0x2, 0, 0, 0xFFFFFFFF, 0xFFFFFFFF, 0, 0))},
		{0x40000, 16, func(c *attrContext) []byte { return []byte(c.volume().uuid) }}, // its own, as each volume of a Mac's has
		{0x80000, 4, nil}, {0x100000, 0, always(str("apfs"))}, // extended mount flags, type name
		{0x200000, 4, nil}, {0x400000, 4, nil}, // subtype, owner
		{0x800000, 8, always(le64((volumeBlocks - volumeFree) * volumeBlockSize))},
		{0x10000000, 8, nil}, {0x20000000, 8, nil}, // quota and reserved sizes
		{0x40000000, 40, func(*attrContext) []byte { return supportedAttrs }},
	}
	dirAttrs = []attr{
		{0x1, 4, attrOne},
		{0x2, 4, func(c *attrContext) []byte {
			if t, ok := c.p.resolve(c.path); ok {
				if names, ok := c.p.dirNames(t); ok {
					return le32(uint32(len(names)))
				}
			}
			return le32(0)
		}},
		{0x4, 4, nil}, {0x8, 8, nil}, {0x10, 4, attrBlock}, {0x20, 8, nil},
	}
	fileAttrs = []attr{
		{0x1, 4, attrOne}, {0x2, 8, attrSize}, {0x4, 8, attrAlloc}, {0x8, 4, attrBlock},
		{0x10, -4, nil}, {0x20, 4, nil}, {0x200, 8, attrSize}, {0x400, 8, attrAlloc}, {0x1000, 8, nil}, {0x2000, 8, nil},
	}
	extendedAttrs = []attr{
		{0x4, 0, func(c *attrContext) []byte { return str(strings.TrimPrefix(c.path, "/")) }}, {0x8, 8, attrAlloc}, {0x10, 8, attrIno},
		{0x20, 0, attrPath}, {0x40, 4, attrDevID}, {0x80, 8, attrFSID}, {0x100, 8, nil}, {0x200, 8, nil}, {0x400, 8, nil},
		{0x800, 8, nil}, {0x1000, 4, nil},
	}
)

// supportedAttrs is the volume saying which attributes are valid, then which are native: all the
// emulator knows.
var supportedAttrs []byte

func init() {
	set := [5]uint32{attrReturned, attrVolInfo}
	for i, group := range [][]attr{commonAttrs, volumeAttrs, dirAttrs, fileAttrs} {
		for _, a := range group {
			set[i] |= a.bit
		}
	}
	supportedAttrs = append(le32(set[:]...), le32(set[:]...)...)
}

// packAttrs is the attribute buffer for one file: its length, the attributes that were returned
// (if asked for), the fixed-size values, then what the references point at. ok is false when the
// list names an attribute the emulator does not know.
func packAttrs(c *attrContext, want [5]uint32, options uint64) (out []byte, ok bool) {
	groups := [5][]attr{commonAttrs}
	ignore := [5]uint32{attrReturned, attrVolInfo} // bits that are not attributes
	if want[1] != 0 {
		groups[1] = volumeAttrs
	}
	if c.isDir() { // a file has no directory attributes and a directory no file attributes
		groups[2], ignore[3] = dirAttrs, ^uint32(0)
	} else {
		groups[3], ignore[2] = fileAttrs, ^uint32(0)
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
		known := ignore[g]
		for _, a := range group {
			known |= a.bit
			if want[g]&a.bit == 0 {
				continue
			}
			if a.size < 0 {
				var value []byte
				if a.value != nil {
					value = a.value(c)
				}
				switch {
				case want[0]&attrReturned == 0 || options&optPackInvalid != 0:
					fixed = append(append(fixed, value...), make([]byte, -a.size-len(value))...)
					returned[g] |= a.bit
				case value != nil:
					// Whether a caller that asks which attributes it was given is given one that
					// APFS has only for some files was not measured.
					c.p.unimplemented("getattrlist of Finder info that is set, asked with which attributes are returned", nil)
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
		if unknown := want[g] &^ known; unknown != 0 {
			c.p.note("  getattrlist: unknown attribute %#x in group %d", unknown, g)
			return nil, false
		}
	}
	for _, r := range references { // the offset from the reference itself, and the length
		le.PutUint32(fixed[r.at:], uint32(len(fixed)-r.at+len(variable)))
		le.PutUint32(fixed[r.at+4:], uint32(len(r.data)))
		variable = append(variable, append(r.data, 0, 0, 0)[:align(uint64(len(r.data)), 4)]...)
	}
	out = make([]byte, 4)
	if want[0]&attrReturned != 0 {
		returned[0] |= attrReturned
		out = append(out, le32(returned[:]...)...)
	}
	out = append(append(out, fixed...), variable...)
	le.PutUint32(out, uint32(len(out)))
	return out, true
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
	n := min(uint64(len(out)), size)
	if options&optFullSize == 0 {
		le.PutUint32(out, uint32(n))
	}
	p.write(buf, out[:n])
	return 0
}

func (p *Process) getattrPath(path string, a []uint64) int64 {
	t, info, errno := p.describePath("getattrlist", path, a[3]&optNoFollow == 0)
	if errno != 0 {
		return errno
	}
	return p.getattr(&attrContext{info, p, t.path}, a)
}

func sysGetattrlist(p *Process, a []uint64) int64 { return p.getattrPath(p.cstr(a[0]), a[1:]) }
func sysGetattrlistat(p *Process, a []uint64) int64 {
	return p.atPath(a[0], a[1], func(path string) int64 { return p.getattrPath(path, a[2:]) })
}

func sysFgetattrlist(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	info, errno := p.describeFD(f)
	if errno != 0 {
		return errno
	}
	return p.getattr(&attrContext{info, p, f.path}, a[1:])
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
		t, info, errno := p.describePath("getattrlistbulk", pathpkg.Join(f.path, f.names[f.pos]), false)
		if errno != 0 {
			continue // gone since the directory was read
		}
		entry, ok := packAttrs(&attrContext{info, p, t.path}, want, a[4])
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
