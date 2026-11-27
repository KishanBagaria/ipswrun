package emulator

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// What a guest changes about a file other than its bytes: its mode, owner and dates, its
// extended attributes, and the locks on it. Only files under the home and the mounts can be
// changed, as on a real system where everything else is the sealed, read-only system volume.

const (
	eROFS        = 30
	eNAMETOOLONG = 63
	eNOTCAPABLE  = 107

	lockShared    = 1 // LOCK_SH
	lockExclusive = 2 // LOCK_EX
	lockNonblock  = 4 // LOCK_NB
	lockUnlock    = 8 // LOCK_UN

	xattrNoFollow   = 0x1 // XATTR_NOFOLLOW
	xattrCreate     = 0x2 // XATTR_CREATE
	xattrReplace    = 0x4 // XATTR_REPLACE
	xattrOptions    = 0x3F
	xattrMaxName    = 127                      // XATTR_MAXNAMELEN
	xattrMaxValue   = 1 << 20                  // what this emulator keeps of one attribute
	resourceFork    = "com.apple.ResourceFork" // the one attribute read and written at a position
	attrCmnModTime  = 0x400
	attrCmnAccTime  = 0x1000
	attrCmnFndrInfo = 0x4000
	finderInfo      = "com.apple.FinderInfo" // the attribute Finder info is, when it is not all zeros
	attrCmnOwnerID  = 0x8000
	attrCmnGroupID  = 0x10000
	attrCmnAccMask  = 0x20000
	attrCmnFlags    = 0x40000
	attrSetSupports = attrCmnModTime | attrCmnAccTime | attrCmnFndrInfo | attrCmnOwnerID | attrCmnGroupID | attrCmnAccMask | attrCmnFlags
)

// hostFile is a host file as the host knows it, whatever the guest calls it.
type hostFile struct{ dev, ino uint64 }

// hostFileOf is the host file a stat describes.
func hostFileOf(st *syscall.Stat_t) hostFile { return hostFile{uint64(st.Dev), st.Ino} }

// changeable is the file under the home or a mount that a path names, or why a call that changes
// a file cannot change it: ENOENT; for the system volume and what is made up beside it, EPERM as a
// user who does not own those files is told, or EROFS for root; or EPERM for the host files that
// are not the guest's (its own executable, the devices).
func (p *Process) changeable(path string, follow bool) (target, hostFile, int64) {
	t, ok := p.resolve(path)
	if follow {
		t, ok = p.followed(t, ok)
	}
	switch {
	case !ok:
		return t, hostFile{}, -eNOENT
	case t.root == nil && t.kind == hostFD:
		return t, hostFile{}, -ePERM
	case t.root == nil:
		return t, hostFile{}, p.systemVolume()
	}
	var st syscall.Stat_t
	if err := statInRoot(t.root, t.rel, follow, &st); err != nil {
		return t, hostFile{}, hostErrno(err)
	}
	return t, hostFileOf(&st), 0
}

// chflags gives a file under the home or a mount the flags it carries beside its mode, all of
// them at once, as chflags and setattrlist do. A user's flags (the low 16 bits) are kept and
// said back by stat and getattrlist, as measured on macOS 27.2.1: hidden, not to be dumped,
// opaque, and bits that name nothing. The system's are refused a user (EPERM), as measured.
// From the kernel's source and not measured: a user is refused of a file that is not its own.
// Nothing here acts on a flag, so those the kernel acts on stop the run: a file that may not be
// changed or only added to, one that is compressed, tracked or in a vault, and any of the
// system's set by root but the one dirhelper sets on the folders it makes, that a file may not
// be unlinked: root sets it and still removes the file, as measured in a virtual machine (macOS
// 27.2, with SIP off there; with it on, whether root still may was not measured).
func (p *Process) chflags(id hostFile, flags uint32, a []uint64) int64 {
	const user, acted = 0xffff, 0x2 | 0x4 | 0x20 | 0x40 | 0x80 // UF_SETTABLE; UF_IMMUTABLE, UF_APPEND, UF_COMPRESSED, UF_TRACKED, UF_DATAVAULT
	switch {
	case p.uid != 0 && p.m.owned(id).UID != p.uid:
		return -ePERM
	case flags&^user != 0 && p.uid != 0:
		return -ePERM
	case flags&^(user|flagNoUnlink) != 0 || flags&acted != 0:
		p.unimplemented(fmt.Sprintf("chflags to %#x, flags the kernel acts on", flags), a)
		return -eNOSYS
	}
	if flags == 0 {
		delete(p.m.flags, id)
	} else {
		p.m.flags[id] = flags
	}
	return 0
}

// sysChflags is chflags(path, flags), and sysFchflags the same of an open file.
func sysChflags(p *Process, a []uint64) int64 {
	_, id, errno := p.changeable(p.cstr(a[0]), true)
	if errno != 0 {
		return errno
	}
	return p.chflags(id, uint32(a[1]), a)
}

func sysFchflags(p *Process, a []uint64) int64 {
	_, id, errno := p.changeableFD(a[0])
	if errno != 0 {
		return errno
	}
	return p.chflags(id, uint32(a[1]), a)
}

// systemVolume is why a file of the system volume cannot be changed: its files are root's, so a
// user is refused for not owning them before the volume is found to be read-only.
func (p *Process) systemVolume() int64 {
	if p.uid != 0 {
		return -ePERM
	}
	return -eROFS
}

// changeableFD is changeable for an open descriptor.
func (p *Process) changeableFD(n uint64) (*fd, hostFile, int64) {
	f := p.fds[n]
	if f == nil {
		return nil, hostFile{}, -eBADF
	}
	switch _, writable := p.writableTarget(f.path); {
	case f.streams(): // a pipe or a socket is no file, as measured of fchmod and fchmod_extended
		return f, hostFile{}, -eINVAL
	case f.kind == hostFD && f.path != "" && writable:
	case f.kind == hostFD || f.kind == stdioFD:
		return f, hostFile{}, -ePERM
	default:
		return f, hostFile{}, p.systemVolume()
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(f.host, &st); err != nil {
		return f, hostFile{}, hostErrno(err)
	}
	return f, hostFileOf(&st), 0
}

// fileMode is a mode's permission bits as Go says them.
func fileMode(mode uint64) os.FileMode {
	m := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

func sysChmod(p *Process, a []uint64) int64 {
	t, _, errno := p.changeable(p.cstr(a[0]), true)
	if errno != 0 {
		return errno
	}
	return p.chmodAt(t, fileMode(a[1]))
}

// chmodAt changes the permissions of a file under the home or a mount, which are from then on
// what the host's file has, whatever a template said of it before.
func (p *Process) chmodAt(t target, mode os.FileMode) int64 {
	if err := t.root.Chmod(t.rel, mode); err != nil {
		return hostErrno(err)
	}
	var st syscall.Stat_t
	if statInRoot(t.root, t.rel, true, &st) == nil {
		delete(p.m.said, hostFileOf(&st))
	}
	return 0
}

// chmodFD is chmodAt of an open file.
func (p *Process) chmodFD(host int, mode uint32) int64 {
	if err := syscall.Fchmod(host, mode); err != nil {
		return hostErrno(err)
	}
	var st syscall.Stat_t
	if syscall.Fstat(host, &st) == nil {
		delete(p.m.said, hostFileOf(&st))
	}
	return 0
}

// sysFchmodat is fchmodat(directory, path, mode, flags): chmod of a path from a directory, of
// the link itself if asked (AT_SYMLINK_NOFOLLOW).
func sysFchmodat(p *Process, a []uint64) int64 {
	const noFollow, flags = 0x20, 0x3FFF // every flag there is for a path from a directory
	switch {
	case a[3]&^flags != 0:
		return -eINVAL
	case a[3]&^noFollow != 0:
		p.unimplemented("fchmodat with flags other than AT_SYMLINK_NOFOLLOW", a)
		return -eNOSYS
	}
	return p.atPath(a[0], a[1], func(path string) int64 {
		t, _, errno := p.changeable(path, a[3]&noFollow == 0)
		if errno != 0 {
			return errno
		}
		return p.chmodAt(t, fileMode(a[2]))
	})
}

// sysFchmodExtended is fchmod_extended(descriptor, user, group, mode, security), which sets
// whichever of them it is given: KAUTH_UID_NONE and KAUTH_GID_NONE (-101) are no user and no
// group, all ones no mode, and nothing no security. gzip and cp call it with what they found of
// the file they copied, and a library that sets a file's access control list (see acls.go) with
// that alone.
func sysFchmodExtended(p *Process, a []uint64) int64 {
	change, errno := p.readExtended(a)
	if errno != 0 {
		return errno
	}
	f, id, errno := p.changeableFD(a[0])
	if errno != 0 {
		return errno
	}
	return p.chmodExtended(id, a, change, func(mode uint32) int64 { return p.chmodFD(f.host, mode) })
}

// sysChmodExtended is chmod_extended(path, user, group, mode, security): the same of a path.
func sysChmodExtended(p *Process, a []uint64) int64 {
	change, errno := p.readExtended(a)
	if errno != 0 {
		return errno
	}
	t, id, errno := p.changeable(p.cstr(a[0]), true)
	if errno != 0 {
		return errno
	}
	return p.chmodExtended(id, a, change, func(mode uint32) int64 { return p.chmodAt(t, fileMode(uint64(mode))) })
}

// extended is the security chmod_extended and fchmod_extended were given.
type extended struct {
	list           *acl
	remove, noList bool
}

func (p *Process) readExtended(a []uint64) (extended, int64) {
	list, remove, noList, errno := p.readFilesec(a[4])
	return extended{list, remove, noList}, errno
}

// chmodExtended sets what chmod_extended and fchmod_extended were given of a host file: all of
// it or none, as the kernel asks leave for the whole before it changes anything. A user or
// group of all ones is no "leave it" but a user, which a user may not give a file to (EPERM, as
// measured). From the source and not measured: a list is its owner's and root's to set (EPERM).
func (p *Process) chmodExtended(id hostFile, a []uint64, change extended, chmod func(mode uint32) int64) int64 {
	const none = 0xFFFFFF9B
	uid, gid, mode := uint32(a[1]), uint32(a[2]), uint32(a[3])
	owner := p.m.owned(id)
	next := owner
	if uid != none {
		next.UID = uid
	}
	if gid != none {
		next.GID = gid
	}
	lists := change.list != nil || change.remove || change.noList
	switch {
	case next != owner && p.uid != 0:
		return -ePERM
	case lists && p.uid != 0 && owner.UID != p.uid:
		return -ePERM
	case change.noList && p.m.acls[id] != nil:
		p.unimplemented("a security record that says it has no list, given for a file that has one", a)
		return -eNOSYS
	case next != owner && (uid == ^uint32(0) || gid == ^uint32(0)):
		p.unimplemented("root giving a file to the user or group that is all ones", a)
		return -eNOSYS
	}
	if mode != ^uint32(0) {
		if errno := chmod(mode & 0o7777); errno != 0 {
			return errno
		}
	}
	p.m.own(id, next)
	if change.list != nil || change.remove {
		p.m.setACL(id, change.list)
	}
	return 0
}

func sysFchmod(p *Process, a []uint64) int64 {
	f, _, errno := p.changeableFD(a[0])
	if errno != 0 {
		return errno
	}
	return p.chmodFD(f.host, uint32(a[1]&0o7777))
}

// ownerOf is who a file belongs to: the guests' user, which is all that can be said of the host
// files the home and the mounts are, unless a process that is another user made it, or root gave
// it away.
func (m *Machine) ownerOf(i fileInfo) Owner {
	if i.owner != nil {
		return *i.owner
	}
	return Owner{m.user.uid, m.user.gid}
}

// owned is ownerOf for a host file.
func (m *Machine) owned(id hostFile) Owner {
	if owner, ok := m.owners[id]; ok {
		return owner
	}
	return Owner{m.user.uid, m.user.gid}
}

// own records whom a host file belongs to.
func (m *Machine) own(id hostFile, owner Owner) {
	if owner == (Owner{m.user.uid, m.user.gid}) {
		delete(m.owners, id)
	} else {
		m.owners[id] = owner
	}
}

// created is a process having made a file or a directory: it is the process's user's, and of
// the group of the directory it is in, as on a Mac's file system.
func (p *Process) created(t target) {
	var st, dir syscall.Stat_t
	if statInRoot(t.root, t.rel, false, &st) != nil {
		return
	}
	owner := Owner{p.uid, p.m.user.gid}
	if statInRoot(t.root, filepath.Dir(t.rel), true, &dir) == nil {
		owner.GID = p.m.owned(hostFileOf(&dir)).GID
	}
	p.m.own(hostFileOf(&st), owner)
	p.inheritACL(t, &st)
}

// chown gives a file to a user and a group, either left as it is if all ones. Leaving both is
// anyone's to ask. Anything else is root's: a Mac's kernel also lets an owner give a file to
// another group of its own, and which groups a user is in is not kept here.
func (p *Process) chown(id hostFile, uid, gid uint32, a []uint64) int64 {
	owner := p.m.owned(id)
	was := owner
	if uid != ^uint32(0) {
		owner.UID = uid
	}
	if gid != ^uint32(0) {
		owner.GID = gid
	}
	switch {
	case owner == was:
		return 0
	case p.uid != 0:
		return -ePERM
	}
	p.m.own(id, owner)
	return 0
}

// chownPath is chown and lchown, which follows a final symbolic link or not.
func (p *Process) chownPath(a []uint64, follow bool) int64 {
	return p.chownAt(p.cstr(a[0]), a[1], a[2], follow, a)
}

func (p *Process) chownAt(path string, uid, gid uint64, follow bool, a []uint64) int64 {
	_, id, errno := p.changeable(path, follow)
	if errno != 0 {
		return errno
	}
	return p.chown(id, uint32(uid), uint32(gid), a)
}

// sysFchownat is fchownat(directory, path, user, group, flags): chown of a path from a
// directory, of the link itself if asked (AT_SYMLINK_NOFOLLOW).
func sysFchownat(p *Process, a []uint64) int64 {
	const noFollow = 0x20
	if a[4]&^noFollow != 0 {
		p.unimplemented("fchownat with flags other than AT_SYMLINK_NOFOLLOW", a)
		return -eNOSYS
	}
	return p.atPath(a[0], a[1], func(path string) int64 { return p.chownAt(path, a[2], a[3], a[4]&noFollow == 0, a) })
}

func sysChown(p *Process, a []uint64) int64  { return p.chownPath(a, true) }
func sysLchown(p *Process, a []uint64) int64 { return p.chownPath(a, false) }

func sysFchown(p *Process, a []uint64) int64 {
	_, id, errno := p.changeableFD(a[0])
	if errno != 0 {
		return errno
	}
	return p.chown(id, uint32(a[1]), uint32(a[2]), a)
}

// times reads the access and modification times of utimes, two struct timevals; with none it is
// now, by the machine's clock.
func (p *Process) times(addr uint64) (atime, mtime time.Time) {
	if addr == 0 {
		now := p.m.clock.now()
		return now, now
	}
	b := p.read(addr, 32)
	at := func(b []byte) time.Time {
		return time.Unix(int64(le.Uint64(b)), int64(int32(le.Uint32(b[8:])))*1000)
	}
	return at(b), at(b[16:])
}

func sysUtimes(p *Process, a []uint64) int64 {
	t, _, errno := p.changeable(p.cstr(a[0]), true)
	if errno != 0 {
		return errno
	}
	atime, mtime := p.times(a[1])
	return hostCall(t.root.Chtimes(t.rel, atime, mtime))
}

func sysFutimes(p *Process, a []uint64) int64 {
	f, _, errno := p.changeableFD(a[0])
	if errno != 0 {
		return errno
	}
	atime, mtime := p.times(a[1])
	return hostCall(syscall.Futimes(f.host, []syscall.Timeval{syscall.NsecToTimeval(atime.UnixNano()), syscall.NsecToTimeval(mtime.UnixNano())}))
}

// setattrlist sets the common attributes a file's dates, owner and mode are, in the order of
// their bits. Any other attribute stops the run as unimplemented: the emulator has nowhere to
// keep it.
func (p *Process) setattrlist(path string, list, buf, size, options uint64, a []uint64) int64 {
	b := p.read(list, 24)
	if le.Uint16(b) != 5 {
		return -eINVAL
	}
	common := le.Uint32(b[4:])
	if common&^attrSetSupports != 0 || slices.ContainsFunc(b[8:24], func(c byte) bool { return c != 0 }) {
		p.unimplemented("setattrlist of attributes the emulator cannot keep", a)
		return -eNOSYS
	}
	t, id, errno := p.changeable(path, options&optNoFollow == 0)
	if errno != 0 {
		return errno
	}
	values := p.read(buf, min(size, 128))
	value := func(n int) []byte {
		if len(values) < n {
			panic(fault{"setattrlist: buffer too short"})
		}
		v := values[:n]
		values = values[n:]
		return v
	}
	timespec := func() time.Time {
		v := value(16)
		return time.Unix(int64(le.Uint64(v)), int64(le.Uint64(v[8:])))
	}
	var mtime, atime time.Time // zero: left as they are
	if common&attrCmnModTime != 0 {
		mtime = timespec()
	}
	if common&attrCmnAccTime != 0 {
		atime = timespec()
	}
	// Finder info is the file's attribute of that name: 32 bytes, and none if they are all zeros.
	if common&attrCmnFndrInfo != 0 {
		if len(values) < 32 {
			return -eINVAL
		}
		if info := slices.Clone(value(32)); slices.ContainsFunc(info, func(c byte) bool { return c != 0 }) {
			if p.m.xattrs[id] == nil {
				p.m.xattrs[id] = map[string][]byte{}
			}
			p.m.xattrs[id][finderInfo] = info
		} else {
			delete(p.m.xattrs[id], finderInfo)
		}
	}
	uid, gid := ^uint32(0), ^uint32(0)
	if common&attrCmnOwnerID != 0 {
		uid = le.Uint32(value(4))
	}
	if common&attrCmnGroupID != 0 {
		gid = le.Uint32(value(4))
	}
	if errno := p.chown(id, uid, gid, a); errno != 0 {
		return errno
	}
	if common&attrCmnAccMask != 0 {
		if errno := p.chmodAt(t, fileMode(uint64(le.Uint32(value(4))))); errno != 0 {
			return errno
		}
	}
	if common&attrCmnFlags != 0 {
		if errno := p.chflags(id, le.Uint32(value(4)), a); errno != 0 {
			return errno
		}
	}
	switch {
	case mtime.IsZero() && atime.IsZero():
		return 0
	case options&optNoFollow != 0:
		return hostCall(linkTimes(t, atime, mtime))
	}
	return hostCall(t.root.Chtimes(t.rel, atime, mtime))
}

// linkTimes sets the dates of what a name is, a symbolic link's own if it is one; a zero time is
// left as it is.
func linkTimes(t target, atime, mtime time.Time) error {
	dir, name, err := t.parent()
	if err != nil {
		return err
	}
	defer dir.Close()
	spec := func(at time.Time) unix.Timespec {
		if at.IsZero() {
			return unix.Timespec{Nsec: utimeOmit}
		}
		return unix.NsecToTimespec(at.UnixNano())
	}
	return unix.UtimesNanoAt(int(dir.Fd()), name, []unix.Timespec{spec(atime), spec(mtime)}, unix.AT_SYMLINK_NOFOLLOW)
}

func sysSetattrlist(p *Process, a []uint64) int64 {
	return p.setattrlist(p.cstr(a[0]), a[1], a[2], a[3], a[4], a)
}

// sysSetattrlistat is setattrlistat(directory, path, &list, buffer, size, options). Told to keep
// beneath the directory (FSOPT_RESOLVE_BENEATH, as cp asks), a whole path or one that begins by
// going up is ENOTCAPABLE; one that goes up further in, or that ends in a link to be followed,
// stops as unimplemented, for where it would lead has not been worked out.
func sysSetattrlistat(p *Process, a []uint64) int64 {
	const beneath = 0x1000
	name := p.cstr(a[1])
	return p.atPath(a[0], a[1], func(path string) int64 {
		if a[5]&beneath != 0 {
			switch clean := filepath.Clean(name); {
			case strings.HasPrefix(clean, "/"), clean == "..", strings.HasPrefix(clean, "../"):
				return -eNOTCAPABLE
			case strings.Contains(name, ".."):
				p.unimplemented("setattrlistat beneath a directory by a path that goes up", a)
				return -eNOSYS
			case a[5]&optNoFollow == 0:
				if t, info, errno := p.describePath("setattrlistat", path, false); errno == 0 && (info.mode&syscall.S_IFMT == syscall.S_IFLNK || t.link != "") {
					p.unimplemented("setattrlistat beneath a directory through a symbolic link", a)
					return -eNOSYS
				}
			}
		}
		return p.setattrlist(path, a[2], a[3], a[4], a[5], a)
	})
}

func sysFsetattrlist(p *Process, a []uint64) int64 {
	f, _, errno := p.changeableFD(a[0])
	if errno != 0 {
		return errno
	}
	return p.setattrlist(f.path, a[1], a[2], a[3], a[4], a)
}

// ---- extended attributes ----

// The extended attributes of the home's and the mounts' files are kept by the machine, by host
// file, for the run: a host's own attributes are not the same on every host (Linux names them in
// namespaces, and some of its file systems have none). The files of the system volume have none.

// xattrFile is what an extended-attribute call acts on: the host file, or why it cannot be
// changed (see changeable).
func (p *Process) xattrFile(path uint64, fdn int64, options uint64) (hostFile, int64) {
	if options&^xattrOptions != 0 {
		return hostFile{}, -eINVAL
	}
	if fdn >= 0 {
		_, id, errno := p.changeableFD(uint64(fdn))
		return id, errno
	}
	_, id, errno := p.changeable(p.cstr(path), options&xattrNoFollow == 0)
	return id, errno
}

// xattrName reads an attribute's name.
func (p *Process) xattrName(addr uint64) (string, int64) {
	name := p.cstr(addr)
	switch {
	case name == "":
		return "", -eINVAL
	case len(name) > xattrMaxName:
		return "", -eNAMETOOLONG
	}
	return name, 0
}

// readOnly says whether an errno of xattrFile only means the file is not one whose attributes can
// change: it can still be read, and has none.
func readOnly(errno int64) bool { return errno == -eROFS || errno == -ePERM }

func (p *Process) getxattr(path uint64, fdn int64, a []uint64) int64 {
	value, size, position, options := a[2], a[3], a[4], a[5]
	name, errno := p.xattrName(a[1])
	if errno != 0 {
		return errno
	}
	id, errno := p.xattrFile(path, fdn, options)
	if readOnly(errno) {
		return -eNOATTR
	}
	if errno != 0 {
		return errno
	}
	if position != 0 && name != resourceFork {
		return -eINVAL
	}
	v, ok := p.m.xattrs[id][name]
	if !ok {
		return -eNOATTR
	}
	v = v[min(position, uint64(len(v))):]
	if value == 0 {
		return int64(len(v))
	}
	if size < uint64(len(v)) {
		return -eRANGE
	}
	p.write(value, v)
	return int64(len(v))
}

func (p *Process) setxattr(path uint64, fdn int64, a []uint64) int64 {
	value, size, position, options := a[2], a[3], a[4], a[5]
	name, errno := p.xattrName(a[1])
	if errno != 0 {
		return errno
	}
	id, errno := p.xattrFile(path, fdn, options)
	if errno != 0 {
		return errno
	}
	switch {
	case position != 0 && name != resourceFork, options&xattrCreate != 0 && options&xattrReplace != 0:
		return -eINVAL
	case size > xattrMaxValue || position > xattrMaxValue:
		return -eNOMEM
	case name == finderInfo && size != 32:
		return -eRANGE
	}
	attrs := p.m.xattrs[id]
	old, exists := attrs[name]
	switch {
	case exists && options&xattrCreate != 0:
		return -eEXIST
	case !exists && options&xattrReplace != 0:
		return -eNOATTR
	}
	v := slices.Clone(p.read(value, size))
	if position != 0 { // into the resource fork as it is
		fork := slices.Clone(old)
		if end := int(position + size); len(fork) < end {
			fork = append(fork, make([]byte, end-len(fork))...)
		}
		copy(fork[position:], v)
		v = fork
	}
	if attrs == nil {
		attrs = map[string][]byte{}
		p.m.xattrs[id] = attrs
	}
	attrs[name] = v
	return 0
}

func (p *Process) listxattr(path uint64, fdn int64, a []uint64) int64 {
	buf, size, options := a[1], a[2], a[3]
	id, errno := p.xattrFile(path, fdn, options)
	if readOnly(errno) {
		return 0
	}
	if errno != 0 {
		return errno
	}
	var names []byte
	for _, name := range slices.Sorted(maps.Keys(p.m.xattrs[id])) {
		names = append(names, str(name)...)
	}
	if buf == 0 {
		return int64(len(names))
	}
	if size < uint64(len(names)) {
		return -eRANGE
	}
	p.write(buf, names)
	return int64(len(names))
}

func (p *Process) removexattr(path uint64, fdn int64, a []uint64) int64 {
	name, errno := p.xattrName(a[1])
	if errno != 0 {
		return errno
	}
	id, errno := p.xattrFile(path, fdn, a[2])
	if errno != 0 {
		return errno
	}
	if _, ok := p.m.xattrs[id][name]; !ok {
		return -eNOATTR
	}
	delete(p.m.xattrs[id], name)
	return 0
}

func sysGetxattr(p *Process, a []uint64) int64     { return p.getxattr(a[0], -1, a) }
func sysFgetxattr(p *Process, a []uint64) int64    { return p.getxattr(0, int64(a[0]), a) }
func sysSetxattr(p *Process, a []uint64) int64     { return p.setxattr(a[0], -1, a) }
func sysFsetxattr(p *Process, a []uint64) int64    { return p.setxattr(0, int64(a[0]), a) }
func sysListxattr(p *Process, a []uint64) int64    { return p.listxattr(a[0], -1, a) }
func sysFlistxattr(p *Process, a []uint64) int64   { return p.listxattr(0, int64(a[0]), a) }
func sysRemovexattr(p *Process, a []uint64) int64  { return p.removexattr(a[0], -1, a) }
func sysFremovexattr(p *Process, a []uint64) int64 { return p.removexattr(0, int64(a[0]), a) }

// forgetFile drops what the machine keeps of a host file that no longer exists: of the one a guest
// path named before it was removed or replaced, if that was its last name.
func (p *Process) forgetFile(t target) {
	var st syscall.Stat_t
	if statInRoot(t.root, t.rel, false, &st) == nil && st.Nlink <= 1 {
		delete(p.m.xattrs, hostFileOf(&st))
		delete(p.m.owners, hostFileOf(&st))
		delete(p.m.fifos, hostFileOf(&st))
		delete(p.m.fifoPipes, hostFileOf(&st)) // those who have it open go on with it
		delete(p.m.sockets, hostFileOf(&st))
		delete(p.m.said, hostFileOf(&st))
		delete(p.m.flags, hostFileOf(&st))
		delete(p.m.acls, hostFileOf(&st))
	}
}

// ---- locks ----

// flock is the host's own: guests sharing a file share the host's locks on it, as processes of
// one real system do, and so do processes of the host. Only a file of the host's has a host
// descriptor to lock; flock of another is not supported. A lock that has to be waited for is asked of the host again whenever
// a guest gives one up, since the host's flock is never left to block the one engine.
func sysFlock(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	op := int(a[1])
	switch {
	case f == nil:
		return -eBADF
	case f.kind != hostFD:
		return -eNOTSUP
	case op&lockUnlock != 0:
		err := syscall.Flock(f.host, syscall.LOCK_UN)
		p.m.retryLocks()
		return hostCall(err)
	case op&^lockNonblock != lockShared && op&^lockNonblock != lockExclusive:
		return -eINVAL
	}
	err := tryLock(f.host, op)
	if err == syscall.EWOULDBLOCK && op&lockNonblock == 0 {
		p.wait(waitLock, uint64(op)<<32|uint64(uint32(f.host)), time.Time{}, result{})
		return 0
	}
	return hostCall(err)
}

func tryLock(host, op int) error {
	how := syscall.LOCK_SH
	if op&lockExclusive != 0 {
		how = syscall.LOCK_EX
	}
	return syscall.Flock(host, how|syscall.LOCK_NB)
}

// retryLocks gives the threads waiting for a lock the ones the host now gives them.
func (m *Machine) retryLocks() {
	for _, q := range m.procs {
		for _, t := range q.threads {
			if !q.exited && t.state == waiting && t.kind == waitLock {
				if err := tryLock(int(int32(t.key)), int(t.key>>32)); err != syscall.EWOULDBLOCK {
					t.answer = &result{}
					if err != nil {
						t.answer = &result{uint64(-hostErrno(err)), true}
					}
					t.wake()
				}
			}
		}
	}
}
