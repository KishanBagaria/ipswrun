package emulator

import (
	"errors"
	"hash/crc32"
	"os"
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
	eNOTDIR = 20
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
	cacheFD               // a file of the build read by range: of the dyld shared cache or the system volume
)

type fd struct {
	kind  fdKind
	host  int
	data  []byte
	cache dyldcache.File
	path  string // canonical; stdio descriptors have none
	pos   int64
	names []string // of a directory being listed, read on the first getdirentries
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
	kind     fdKind
	path     string // canonical
	host     string
	writable bool // under the home: the guest may create, change, rename and remove it
	data     []byte
	cache    dyldcache.File
}

// fd is the descriptor for a target that is not a host file.
func (t target) fd() *fd { return &fd{kind: t.kind, data: t.data, cache: t.cache, path: t.path} }

// hostErrno is the negated errno of a failed host call.
func hostErrno(err error) int64 {
	var errno syscall.Errno
	if errors.As(err, &errno) && errno != 0 {
		return -guestErrno(errno)
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
		return target{kind: hostFD, host: p.home + path[len(GuestHome):], writable: true}, true
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
		if f.File != nil {
			return target{kind: cacheFD, cache: f.File}, true
		}
	}
	return target{}, false
}

// statBuf is a struct stat64.
func statBuf(i fileInfo) []byte {
	b := make([]byte, 144)
	le.PutUint32(b[0:], i.dev)
	le.PutUint16(b[4:], i.mode)
	le.PutUint16(b[6:], 1)
	le.PutUint64(b[8:], i.ino)
	le.PutUint32(b[16:], guestUID)
	le.PutUint32(b[20:], guestGID)
	for at := 32; at < 96; at += 16 { // access, modification, change and birth times
		le.PutUint64(b[at:], uint64(i.mtime))
	}
	le.PutUint64(b[96:], uint64(i.size))
	le.PutUint64(b[104:], align(uint64(i.size), volumeBlockSize)/512)
	le.PutUint32(b[112:], volumeBlockSize)
	return b
}

const (
	fixedMtime = 1700000000
	modeDir    = syscall.S_IFDIR | 0o755
	modeFile   = syscall.S_IFREG | 0o755
)

// putStat writes the struct stat64 of a file that was described without an error.
func (p *Process) putStat(buf uint64, info fileInfo, errno int64) int64 {
	if errno == 0 {
		p.write(buf, statBuf(info))
	}
	return errno
}

func (p *Process) doStat(path string, buf uint64, follow bool) int64 {
	_, info, errno := p.describePath("stat", path, follow)
	return p.putStat(buf, info, errno)
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
	info, errno := p.describeFD(f)
	return p.putStat(a[1], info, errno)
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
		if t.path == GuestExe && flags&(3|0x200|0x400) != 0 { // the user's file: not written, created or truncated
			return -eACCES
		}
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
		pos, err := syscall.Seek(f.host, off, hostWhence(int(int32(a[2]))))
		if err != nil {
			return hostErrno(err)
		}
		return pos
	case dataFD, cacheFD:
		switch a[2] {
		case 0:
			f.pos = off
		case 1:
			f.pos += off
		default:
			f.pos = f.size() + off
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
	return hostCall(syscall.Ftruncate(f.host, int64(a[1])))
}

func (p *Process) doAccess(path string) int64 {
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

func sysAccess(p *Process, a []uint64) int64    { return p.doAccess(p.cstr(a[0])) }
func sysFaccessat(p *Process, a []uint64) int64 { return p.doAccess(p.at(a[0], a[1])) }

// hostPath resolves a guest path to the host file behind it. ok says that it is under the home:
// only those can be changed, never the runner or a device.
func (p *Process) hostPath(path string) (host string, ok bool) {
	t, _ := p.resolve(path)
	return t.host, t.writable
}

// hostCall reports the result of a host file operation the way a system call does.
func hostCall(err error) int64 {
	if err != nil {
		return hostErrno(err)
	}
	return 0
}

func (p *Process) doUnlink(path string, dir bool) int64 {
	host, ok := p.hostPath(path)
	if !ok {
		return -eNOENT
	}
	if dir {
		return hostCall(syscall.Rmdir(host))
	}
	return hostCall(syscall.Unlink(host))
}

func sysUnlink(p *Process, a []uint64) int64 { return p.doUnlink(p.cstr(a[0]), false) }
func sysRmdir(p *Process, a []uint64) int64  { return p.doUnlink(p.cstr(a[0]), true) }
func sysUnlinkat(p *Process, a []uint64) int64 {
	return p.doUnlink(p.at(a[0], a[1]), a[2]&0x80 != 0) // AT_REMOVEDIR
}

func (p *Process) doMkdir(path string, mode uint64) int64 {
	host, ok := p.hostPath(path)
	if !ok {
		return -eACCES
	}
	return hostCall(syscall.Mkdir(host, uint32(mode&0o777)))
}

func sysMkdir(p *Process, a []uint64) int64   { return p.doMkdir(p.cstr(a[0]), a[1]) }
func sysMkdirat(p *Process, a []uint64) int64 { return p.doMkdir(p.at(a[0], a[1]), a[2]) }

func (p *Process) doRename(from, to string) int64 {
	hostFrom, ok1 := p.hostPath(from)
	hostTo, ok2 := p.hostPath(to)
	if !ok1 || !ok2 {
		return -eACCES
	}
	return hostCall(syscall.Rename(hostFrom, hostTo))
}

func sysRename(p *Process, a []uint64) int64 { return p.doRename(p.cstr(a[0]), p.cstr(a[1])) }

func sysRenameat(p *Process, a []uint64) int64 { return p.doRename(p.at(a[0], a[1]), p.at(a[2], a[3])) }

// sysRenameatx is renameat with flags (swap, exclusive), which nothing has needed yet.
func sysRenameatx(p *Process, a []uint64) int64 {
	if a[4] != 0 {
		return -eNOTSUP
	}
	return sysRenameat(p, a)
}

func sysTruncate(p *Process, a []uint64) int64 {
	host, ok := p.hostPath(p.cstr(a[0]))
	if !ok {
		return -eACCES
	}
	return hostCall(syscall.Truncate(host, int64(a[1])))
}

// doReadlink serves the links a guest makes under its home; nothing else is a symbolic link.
func (p *Process) doReadlink(path string, buf, size uint64) int64 {
	host, ok := p.hostPath(path)
	if !ok {
		return -eINVAL
	}
	out := hostBuffer(size)
	n, err := syscall.Readlink(host, out)
	if err != nil {
		return hostErrno(err)
	}
	p.write(buf, out[:n])
	return int64(n)
}

func sysReadlink(p *Process, a []uint64) int64   { return p.doReadlink(p.cstr(a[0]), a[1], a[2]) }
func sysReadlinkat(p *Process, a []uint64) int64 { return p.doReadlink(p.at(a[0], a[1]), a[2], a[3]) }

// list reads the names in an open directory, once. Only directories under the home have any.
func (p *Process) list(f *fd) int64 {
	if f.names != nil {
		return 0
	}
	names := []string{".", ".."}
	switch f.kind {
	case hostFD:
		host, _ := p.hostPath(f.path)
		entries, err := os.ReadDir(host)
		if err != nil {
			return -eNOTDIR
		}
		for _, e := range entries {
			names = append(names, e.Name())
		}
	case dirFD:
	default:
		return -eNOTDIR
	}
	f.names = names
	return 0
}

// sysGetdirentries64 lists a directory under the home; the directories that exist only as names
// are empty. A buffer of 1024 bytes or more ends in flags, where 1 says the listing is complete.
func sysGetdirentries64(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	if errno := p.list(f); errno != 0 {
		return errno
	}
	size := a[2]
	if size >= 1024 {
		size -= 4
	}
	var out []byte
	for ; f.pos < int64(len(f.names)); f.pos++ {
		name := f.names[f.pos]
		entry := make([]byte, align(uint64(21+len(name)+1), 8))
		if uint64(len(out)+len(entry)) > size {
			break
		}
		kind := byte(syscall.DT_DIR)
		child := pathpkg.Join(f.path, name)
		if host, ok := p.hostPath(child); ok && f.pos >= 2 {
			var st syscall.Stat_t
			if syscall.Lstat(host, &st) == nil {
				kind = byte(st.Mode >> 12) // DT_* are the file type bits of a mode
			}
		}
		le.PutUint64(entry[0:], crcInode(child)|1)
		le.PutUint64(entry[8:], uint64(f.pos+1))
		le.PutUint16(entry[16:], uint16(len(entry)))
		le.PutUint16(entry[18:], uint16(len(name)))
		entry[20] = kind
		copy(entry[21:], name)
		out = append(out, entry...)
	}
	p.write(a[1], out)
	if a[2] >= 1024 {
		p.write(a[1]+a[2]-4, le32(uint32(truth(f.pos == int64(len(f.names))))))
	}
	if a[3] != 0 {
		p.write(a[3], le64(uint64(f.pos)))
	}
	return int64(len(out))
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
		p.write(arg, str(path))
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
	case 105: // F_GETSIGSINFO: files of the build are platform binaries, the guest's own are not
		p.write(arg+12, le32(uint32(truth(f.kind != hostFD))))
	case 7, 92: // F_GETLK / F_OFD_GETLK: nobody else holds locks
		p.write(arg+20, []byte{2, 0})
	}
	p.note("  fcntl %d cmd %d", a[0], a[1])
	return 0
}

func sysDup(p *Process, a []uint64) int64 { return sysFcntl(p, []uint64{a[0], 0, 0}) }

// volume is a mounted file system. There are two, as on a real system, because callers that list
// the mounts (CarbonCore) insist that each has its own device id.
type volume struct {
	dev           uint32
	mount, device string
}

const preboot = "/System/Volumes/Preboot"

// volumes are the mounts, the root last.
var volumes = []volume{{deviceID + 1, preboot, "/dev/disk1s2"}, {deviceID, "/", "/dev/disk1"}}

// volumeOf is the volume a canonical guest path is on.
func volumeOf(path string) volume {
	for _, v := range volumes {
		if path == v.mount || strings.HasPrefix(path, v.mount+"/") {
			return v
		}
	}
	return volumes[len(volumes)-1]
}

func (p *Process) statfs(path string, buf uint64) int64 {
	v := volumeOf(path)
	b := make([]byte, 2168)
	le.PutUint32(b[0:], volumeBlockSize)  // f_bsize
	le.PutUint32(b[4:], 1<<20)            // f_iosize
	le.PutUint64(b[8:], volumeBlocks)     // f_blocks
	le.PutUint64(b[16:], volumeFree)      // f_bfree
	le.PutUint64(b[24:], volumeFree)      // f_bavail
	le.PutUint64(b[32:], volumeObjects)   // f_files
	le.PutUint64(b[40:], volumeObjects/2) // f_ffree
	le.PutUint32(b[48:], v.dev)
	le.PutUint32(b[52:], fsTypeAPFS)
	copy(b[72:], "apfs")
	copy(b[88:], v.mount)
	copy(b[1112:], v.device)
	p.write(buf, b)
	return 0
}

func sysStatfs64(p *Process, a []uint64) int64 {
	path := p.cstr(a[0])
	t, ok := p.resolve(path)
	if !ok {
		p.note("  ENOENT statfs %s", path)
		return -eNOENT
	}
	return p.statfs(t.path, a[1])
}

func sysFstatfs64(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	return p.statfs(f.path, a[1])
}

// pathLimits are the answers of pathconf and fpathconf, the same for every file: an APFS volume
// that preserves case without being sensitive to it.
var pathLimits = map[uint64]int64{1: 32767, 4: 255, 5: 1024, 6: 512, 7: 200112, 8: 200112, 10: 255, 11: 0, 12: 1, 18: 64, 24: 1024, 26: 64, 27: 4096}

func sysPathconf(p *Process, a []uint64) int64 {
	if v, ok := pathLimits[a[1]]; ok {
		return v
	}
	return -eINVAL
}

// sysFsgetpath turns a volume and an inode number back into a path: of a volume's root, or of a
// file the guest has looked at before.
func sysFsgetpath(p *Process, a []uint64) int64 {
	path, ok := p.seen[fileID{le.Uint32(p.read(a[2], 8)), a[3]}]
	if !ok || uint64(len(path)) >= a[1] {
		return -eNOENT
	}
	p.write(a[0], str(path))
	return int64(len(path) + 1)
}

func sysShmOpen(p *Process, a []uint64) int64 {
	p.note("  shm_open %s", p.cstr(a[0]))
	return -eNOENT
}
