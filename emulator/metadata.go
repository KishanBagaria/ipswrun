package emulator

import (
	"maps"
	"os"
	"slices"
	"syscall"
	"time"
)

// What a guest changes about a file other than its bytes: its mode, owner and dates, its
// extended attributes, and the locks on it. Only files under the home and the mounts can be
// changed, as on a real system where everything else is the sealed, read-only system volume.

const (
	eROFS        = 30
	eNAMETOOLONG = 63

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
	attrCmnOwnerID  = 0x8000
	attrCmnGroupID  = 0x10000
	attrCmnAccMask  = 0x20000
	attrSetSupports = attrCmnModTime | attrCmnAccTime | attrCmnOwnerID | attrCmnGroupID | attrCmnAccMask
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
	return hostCall(t.root.Chmod(t.rel, fileMode(a[1])))
}

func sysFchmod(p *Process, a []uint64) int64 {
	f, _, errno := p.changeableFD(a[0])
	if errno != 0 {
		return errno
	}
	return hostCall(syscall.Fchmod(f.host, uint32(a[1]&0o7777)))
}

// chown can only leave a file as it is: every file belongs to the guests' user, which is all that
// can be said of the host files the home and the mounts are. Giving one away is refused, as the
// kernel refuses it to anyone but root; root could, but the emulator has no owner to keep.
func (p *Process) chown(uid, gid uint32, a []uint64) int64 {
	if (uid == ^uint32(0) || uid == p.m.user.uid) && (gid == ^uint32(0) || gid == p.m.user.gid) {
		return 0
	}
	if p.uid != 0 {
		return -ePERM
	}
	p.unimplemented("chown to an owner other than the user", a)
	return -eNOSYS
}

// chownPath is chown and lchown, which follows a final symbolic link or not.
func (p *Process) chownPath(a []uint64, follow bool) int64 {
	if _, _, errno := p.changeable(p.cstr(a[0]), follow); errno != 0 {
		return errno
	}
	return p.chown(uint32(a[1]), uint32(a[2]), a)
}

func sysChown(p *Process, a []uint64) int64  { return p.chownPath(a, true) }
func sysLchown(p *Process, a []uint64) int64 { return p.chownPath(a, false) }

func sysFchown(p *Process, a []uint64) int64 {
	if _, _, errno := p.changeableFD(a[0]); errno != 0 {
		return errno
	}
	return p.chown(uint32(a[1]), uint32(a[2]), a)
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
	t, _, errno := p.changeable(path, options&optNoFollow == 0)
	if errno != 0 {
		return errno
	}
	values := p.read(buf, min(size, 64))
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
	uid, gid := ^uint32(0), ^uint32(0)
	if common&attrCmnOwnerID != 0 {
		uid = le.Uint32(value(4))
	}
	if common&attrCmnGroupID != 0 {
		gid = le.Uint32(value(4))
	}
	if errno := p.chown(uid, gid, a); errno != 0 {
		return errno
	}
	if common&attrCmnAccMask != 0 {
		if err := t.root.Chmod(t.rel, fileMode(uint64(le.Uint32(value(4))))); err != nil {
			return hostErrno(err)
		}
	}
	if !mtime.IsZero() || !atime.IsZero() {
		return hostCall(t.root.Chtimes(t.rel, atime, mtime))
	}
	return 0
}

func sysSetattrlist(p *Process, a []uint64) int64 {
	return p.setattrlist(p.cstr(a[0]), a[1], a[2], a[3], a[4], a)
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
