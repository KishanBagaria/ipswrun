package emulator

import (
	"errors"
	"hash/crc32"
	pathpkg "path"
	"strings"
	"syscall"

	"github.com/KishanBagaria/ipswrun/dyldcache"
)

// Guest errno values (Darwin's).
const (
	ePERM   = 1
	eNOENT  = 2
	eSRCH   = 3
	eBADF   = 9
	eNOMEM  = 12
	eACCES  = 13
	eFAULT  = 14
	eISDIR  = 21
	eINVAL  = 22
	eNOTTY  = 25
	eSPIPE  = 29
	eNOTSUP = 45
	eNOSYS  = 78
	eNOATTR = 93
)

type fdKind uint8

const (
	hostFD  fdKind = iota // a real descriptor: stdio, or a file under the temporary home
	dirFD                 // a directory that exists only as a name
	dataFD                // read-only bytes
	cacheFD               // a file of the dyld shared cache
)

type fd struct {
	kind  fdKind
	host  int
	data  []byte
	cache dyldcache.File
	path  string // canonical; stdio descriptors have none
	pos   int64
}

func (f *fd) size() int64 {
	if f.kind == cacheFD {
		return f.cache.Size()
	}
	return int64(len(f.data))
}

// pread returns up to n bytes at off, or a negated errno.
func (f *fd) pread(off, n uint64) ([]byte, int64) {
	switch f.kind {
	case hostFD:
		data := hostBuffer(n)
		got, err := syscall.Pread(f.host, data, int64(off))
		if err != nil {
			return nil, hostErrno(err)
		}
		return data[:got], 0
	case cacheFD:
		data, err := f.cache.Pread(int64(min(off, 1<<62)), int64(min(n, 1<<40)))
		if err != nil {
			stopf("read %s: %v", f.path, err)
		}
		return data, 0
	case dataFD:
		off = min(off, uint64(len(f.data)))
		return f.data[off:min(off+min(n, 1<<40), uint64(len(f.data)))], 0
	}
	return nil, -eISDIR
}

// target is what a guest path resolves to.
type target struct {
	kind  fdKind
	path  string // canonical
	host  string
	data  []byte
	cache dyldcache.File
}

// fd is the descriptor for a target that is not a host file.
func (t target) fd() *fd { return &fd{kind: t.kind, data: t.data, cache: t.cache, path: t.path} }

// hostErrno is the negated errno of a failed host call.
func hostErrno(err error) int64 {
	var errno syscall.Errno
	if errors.As(err, &errno) && errno != 0 {
		return -int64(errno)
	}
	return -eINVAL
}

func closeHost(fd int) { syscall.Close(fd) }

// canonical is the normalized guest path with the cryptex symlink of a real system followed, so a
// file has one identity (and one inode number) however the guest names it.
func canonical(path string) string {
	path = pathpkg.Clean(path)
	if path == cryptexLink || strings.HasPrefix(path, cryptexLink+"/") {
		path = cryptex + path[len(cryptexLink):]
	}
	return path
}

func crcInode(path string) uint64 { return uint64(crc32.ChecksumIEEE([]byte(path)) & 0xFFFFFF) }

// resolve maps a guest path to what backs it. Only the temporary home, the runner and three
// device nodes ever name host files; everything else is served from memory or does not exist.
func (p *Process) resolve(path string) (target, bool) {
	path = canonical(path)
	t, ok := p.lookup(path)
	t.path = path
	return t, ok
}

func (p *Process) lookup(path string) (target, bool) {
	switch {
	case path == GuestExe:
		return target{kind: hostFD, host: p.runnerPath}, true
	case path == guestDyld:
		return target{kind: dataFD, data: p.dyld}, true
	}
	if f, ok := p.cachePaths[path]; ok {
		return target{kind: cacheFD, cache: f}, true
	}
	switch {
	case path == GuestHome || strings.HasPrefix(path, GuestHome+"/"):
		return target{kind: hostFD, host: p.home + path[len(GuestHome):]}, true
	case path == "/dev/null" || path == "/dev/urandom" || path == "/dev/random":
		return target{kind: hostFD, host: path}, true
	case path == p.cacheDir || strings.HasPrefix(p.cacheDir, path+"/"):
		return target{kind: dirFD}, true
	}
	switch path {
	case "/", "/Users", "/System", "/System/Library", "/usr", "/usr/lib", "/private", "/private/var", "/var", "/tmp", "/private/tmp", "/emu":
		return target{kind: dirFD}, true
	}
	if p.system != nil {
		f, err := p.system(path)
		if err != nil {
			stopf("%v", err)
		}
		if f.Dir {
			return target{kind: dirFD}, true
		}
		if f.Found {
			return target{kind: dataFD, data: f.Data}, true
		}
	}
	return target{}, false
}

// statBuf is a struct stat64.
func statBuf(mode uint16, size int64, ino uint64, mtime int64) []byte {
	b := make([]byte, 144)
	le.PutUint32(b[0:], deviceID)
	le.PutUint16(b[4:], mode)
	le.PutUint16(b[6:], 1)
	le.PutUint64(b[8:], ino)
	le.PutUint32(b[16:], guestUID)
	le.PutUint32(b[20:], guestGID)
	for i := 32; i < 96; i += 16 { // access, modification, change and birth times
		le.PutUint64(b[i:], uint64(mtime))
	}
	le.PutUint64(b[96:], uint64(size))
	le.PutUint64(b[104:], align(uint64(size), 4096)/512)
	le.PutUint32(b[112:], 4096)
	return b
}

const (
	fixedMtime = 1700000000
	modeDir    = syscall.S_IFDIR | 0o755
	modeFile   = syscall.S_IFREG | 0o755
)

// stat writes the struct stat64 of a resolved path or an open descriptor. Paths with a pinned
// inode number report it; hostStat is consulted for a hostFD only.
func (p *Process) stat(buf uint64, kind fdKind, path string, size int64, hostStat func(*syscall.Stat_t) error) int64 {
	ino := p.inodes[path]
	switch kind {
	case hostFD:
		var st syscall.Stat_t
		if err := hostStat(&st); err != nil {
			return hostErrno(err)
		}
		if ino == 0 {
			ino = st.Ino & 0xFFFFFFFF
		}
		p.write(buf, statBuf(uint16(st.Mode), st.Size, ino, st.Mtimespec.Sec))
	case dirFD:
		p.write(buf, statBuf(modeDir, 0, crcInode(path), fixedMtime))
	default:
		if ino == 0 {
			ino = crcInode(path)
		}
		p.write(buf, statBuf(modeFile, size, ino, fixedMtime))
	}
	return 0
}

func (p *Process) doStat(path string, buf uint64, follow bool) int64 {
	t, ok := p.resolve(path)
	if !ok {
		p.note("  ENOENT stat %s", path)
		return -eNOENT
	}
	return p.stat(buf, t.kind, t.path, t.fd().size(), func(st *syscall.Stat_t) error {
		if follow {
			return syscall.Stat(t.host, st)
		}
		return syscall.Lstat(t.host, st)
	})
}

func sysStat64(p *Process, a []uint64) int64  { return p.doStat(p.cstr(a[0]), a[1], true) }
func sysLstat64(p *Process, a []uint64) int64 { return p.doStat(p.cstr(a[0]), a[1], false) }

func sysFstatat64(p *Process, a []uint64) int64 {
	return p.doStat(p.at(a[0], a[1]), a[2], a[3]&0x20 == 0)
}

func sysFstat64(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	return p.stat(a[1], f.kind, f.path, f.size(), func(st *syscall.Stat_t) error { return syscall.Fstat(f.host, st) })
}

func (p *Process) doOpen(path string, flags, mode uint64) int64 {
	t, ok := p.resolve(path)
	if !ok {
		p.note("  ENOENT open %s", path)
		return -eNOENT
	}
	n := p.nextFD
	switch t.kind {
	case hostFD:
		hostFlags := int(flags & 3)
		for _, m := range []struct {
			guest uint64
			host  int
		}{{0x200, syscall.O_CREAT}, {0x400, syscall.O_TRUNC}, {0x800, syscall.O_EXCL}, {0x8, syscall.O_APPEND}, {0x100000, syscall.O_DIRECTORY}, {0x100, syscall.O_NOFOLLOW}} {
			if flags&m.guest != 0 {
				hostFlags |= m.host
			}
		}
		h, err := syscall.Open(t.host, hostFlags|syscall.O_CLOEXEC, uint32(mode&0o777))
		if err != nil {
			p.note("  open %s failed: %v", path, err)
			return hostErrno(err)
		}
		p.fds[n] = &fd{kind: hostFD, host: h, path: t.path}
	default:
		p.fds[n] = t.fd()
	}
	p.nextFD++
	p.note("  open %s -> %d", path, n)
	return int64(n)
}

// at resolves a path argument relative to a directory descriptor.
func (p *Process) at(dirfd, path uint64) string {
	s := p.cstr(path)
	if s == "" {
		return "/nonexistent/empty-path"
	}
	if !strings.HasPrefix(s, "/") {
		dir := "/"
		if f := p.fds[dirfd&0xFFFFFFFF]; f != nil && f.path != "" {
			dir = f.path
		}
		if !strings.HasSuffix(dir, "/") {
			dir += "/"
		}
		s = dir + s
	}
	return s
}

func sysOpen(p *Process, a []uint64) int64   { return p.doOpen(p.cstr(a[0]), a[1], a[2]) }
func sysOpenat(p *Process, a []uint64) int64 { return p.doOpen(p.at(a[0], a[1]), a[2], a[3]) }

func sysGuardedOpen(p *Process, a []uint64) int64 { return p.doOpen(p.cstr(a[0]), a[3], a[4]) }

func sysGuardedClose(p *Process, a []uint64) int64 { return sysClose(p, a[:1]) }
func sysGuardedWrite(p *Process, a []uint64) int64 { return sysWrite(p, []uint64{a[0], a[2], a[3]}) }
func sysGuardedPwrite(p *Process, a []uint64) int64 {
	return sysPwrite(p, []uint64{a[0], a[2], a[3], a[4]})
}

func sysClose(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	delete(p.fds, a[0])
	if f.kind == hostFD && a[0] > 2 {
		closeHost(f.host)
	}
	return 0
}

// hostBuffer bounds a guest-supplied length before it becomes a host allocation.
func hostBuffer(n uint64) []byte { return make([]byte, min(n, 1<<30)) }

func sysRead(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	var data []byte
	if f.kind == hostFD { // the host keeps the position
		data = hostBuffer(a[2])
		n, err := syscall.Read(f.host, data)
		if err != nil {
			return hostErrno(err)
		}
		data = data[:n]
	} else {
		var errno int64
		if data, errno = f.pread(uint64(max(f.pos, 0)), a[2]); errno != 0 {
			return errno
		}
		f.pos += int64(len(data))
	}
	p.write(a[1], data)
	return int64(len(data))
}

func sysPread(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	data, errno := f.pread(a[3], a[2])
	if errno != 0 {
		return errno
	}
	p.write(a[1], data)
	return int64(len(data))
}

func sysWrite(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil || f.kind != hostFD {
		return -eBADF
	}
	data := p.read(a[1], a[2])
	if a[0] == 1 || a[0] == 2 {
		text := strings.ToValidUTF8(string(data), "�")
		if a[0] == 1 {
			p.stdout.WriteString(text)
		} else {
			p.stderr.WriteString(text)
		}
		if p.trace {
			syscall.Write(2, []byte(text))
		}
		return int64(a[2])
	}
	n, err := syscall.Write(f.host, data)
	if err != nil {
		return hostErrno(err)
	}
	return int64(n)
}

func sysPwrite(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil || f.kind != hostFD {
		return -eBADF
	}
	n, err := syscall.Pwrite(f.host, p.read(a[1], a[2]), int64(a[3]))
	if err != nil {
		return hostErrno(err)
	}
	return int64(n)
}

func sysLseek(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	off := int64(a[1])
	switch f.kind {
	case hostFD:
		pos, err := syscall.Seek(f.host, off, int(int32(a[2])))
		if err != nil {
			return hostErrno(err)
		}
		return pos
	case dataFD:
		switch a[2] {
		case 0:
			f.pos = off
		case 1:
			f.pos += off
		default:
			f.pos = int64(len(f.data)) + off
		}
		return f.pos
	}
	return -eSPIPE
}

func sysFtruncate(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil || f.kind != hostFD {
		return -eBADF
	}
	if err := syscall.Ftruncate(f.host, int64(a[1])); err != nil {
		return hostErrno(err)
	}
	return 0
}

func sysAccess(p *Process, a []uint64) int64 {
	path := p.cstr(a[0])
	t, ok := p.resolve(path)
	if ok && t.kind == hostFD {
		var st syscall.Stat_t
		ok = syscall.Stat(t.host, &st) == nil
	}
	if !ok {
		p.note("  ENOENT access %s", path)
		return -eNOENT
	}
	return 0
}

func sysUnlink(p *Process, a []uint64) int64 {
	t, ok := p.resolve(p.cstr(a[0]))
	if !ok || t.kind != hostFD {
		return -eNOENT
	}
	if err := syscall.Unlink(t.host); err != nil {
		return hostErrno(err)
	}
	return 0
}

func sysMkdir(p *Process, a []uint64) int64 {
	t, ok := p.resolve(p.cstr(a[0]))
	if !ok || t.kind != hostFD {
		return -eACCES
	}
	if err := syscall.Mkdir(t.host, uint32(a[1]&0o777)); err != nil {
		return hostErrno(err)
	}
	return 0
}

func sysRename(p *Process, a []uint64) int64 {
	from, ok1 := p.resolve(p.cstr(a[0]))
	to, ok2 := p.resolve(p.cstr(a[1]))
	if !ok1 || !ok2 || from.kind != hostFD || to.kind != hostFD {
		return -eACCES
	}
	if err := syscall.Rename(from.host, to.host); err != nil {
		return hostErrno(err)
	}
	return 0
}

func sysFcntl(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	switch cmd, arg := a[1], a[2]; cmd {
	case 50: // F_GETPATH
		path := "/dev/fd"
		if f.path != "" {
			path = f.path
		}
		p.write(arg, append([]byte(path), 0))
	case 0, 67: // F_DUPFD, F_DUPFD_CLOEXEC
		n := p.nextFD
		p.nextFD++
		if f.kind == hostFD {
			h, err := syscall.Dup(f.host)
			if err != nil {
				return hostErrno(err)
			}
			p.fds[n] = &fd{kind: hostFD, host: h, path: f.path}
		} else {
			p.fds[n] = f // shares the offset, like a real duplicate
		}
		return int64(n)
	case 3: // F_GETFL
		return 2
	case 97: // F_ADDFILESIGS_RETURN: the signature covers the file up to the blob
		p.write(arg, p.read(arg+8, 8))
	case 7, 92: // F_GETLK / F_OFD_GETLK: nobody else holds locks
		p.write(arg+20, []byte{2, 0})
	}
	p.note("  fcntl %d cmd %d", a[0], a[1])
	return 0
}

func sysDup(p *Process, a []uint64) int64 { return sysFcntl(p, []uint64{a[0], 0, 0}) }

func sysStatfs64(p *Process, a []uint64) int64 {
	b := make([]byte, 2168)
	le.PutUint32(b[0:], 4096)   // f_bsize
	le.PutUint32(b[4:], 1<<20)  // f_iosize
	le.PutUint64(b[8:], 1<<28)  // f_blocks
	le.PutUint64(b[16:], 1<<27) // f_bfree
	le.PutUint64(b[24:], 1<<27) // f_bavail
	le.PutUint64(b[32:], 1<<20) // f_files
	le.PutUint64(b[40:], 1<<19) // f_ffree
	le.PutUint32(b[48:], deviceID)
	le.PutUint32(b[52:], 0x1a)
	copy(b[72:], "apfs")
	copy(b[88:], "/")
	copy(b[1112:], "/dev/disk1")
	p.write(a[1], b)
	return 0
}

func sysGetattrlist(p *Process, a []uint64) int64 {
	p.note("  getattrlist %s", p.cstr(a[0]))
	return -eNOENT
}

func sysFsgetpath(p *Process, a []uint64) int64 {
	for path, ino := range p.inodes {
		if ino == a[3] {
			p.write(a[0], append([]byte(path), 0))
			return int64(len(path) + 1)
		}
	}
	return -eNOENT
}

func sysShmOpen(p *Process, a []uint64) int64 {
	p.note("  shm_open %s", p.cstr(a[0]))
	return -eNOENT
}
