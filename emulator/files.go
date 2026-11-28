package emulator

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/KishanBagaria/ipswrun/dyldcache"
)

// Guest errno values (Darwin's).
const (
	ePERM     = 1
	eINTR     = 4
	eNOENT    = 2
	eSRCH     = 3
	eIO       = 5
	eBADF     = 9
	eNOMEM    = 12
	eACCES    = 13
	eFAULT    = 14
	eEXIST    = 17
	eMFILE    = 24
	eXDEV     = 18
	eNOTDIR   = 20
	eISDIR    = 21
	eINVAL    = 22
	eNOTTY    = 25
	eSPIPE    = 29
	eLOOP     = 62
	eRANGE    = 34
	eAGAIN    = 35
	eALREADY  = 37
	ePIPE     = 32
	eNOTSUP   = 45
	eTIMEDOUT = 60
	eNOSYS    = 78
	eNOATTR   = 93
)

type fdKind uint8

const (
	hostFD   fdKind = iota // a real descriptor: a file under the home or a mount, the guest exe, or a device
	stdioFD                // guest stdin (0), stdout (1) or stderr (2), even when duplicated
	dirFD                  // a directory that exists only as a name, or on the system volume
	dataFD                 // read-only bytes, or shm_open shared memory
	cacheFD                // a file of the build read by range: of the dyld shared cache or the system volume
	pipeFD                 // one end of a pipe (see pipes.go)
	socketFD               // one of a pair of connected sockets (see sockets.go)
	ttyFD                  // an end of a terminal (see terminals.go)
	semFD                  // a semaphore that has a name (see semaphores.go)
)

type fd struct {
	kind    fdKind
	host    int
	data    []byte
	cache   dyldcache.File
	readDir func() ([]string, error) // of a system-volume directory
	path    string                   // canonical; stdio descriptors have none
	inode   *Inode                   // of a file or directory of the system volume: its owner and permissions there
	pos     int64
	names   []string        // of a directory being listed, read on the first getdirentries
	kinds   map[string]byte // of a host directory's names, the kind the host's listing has by each
	// shared, of a descriptor from shm_open, is the memory it names; data is then what it holds.
	shared *sharedMemory
	// Of an end of a pipe: the pipe, the flags fcntl reads of it, the writing end's among them,
	// and whether a write with nobody to read it is to raise no signal.
	pipe      *pipe
	sock      *socket // of a socket: the socket, with the same flags
	flags     uint32
	noSigpipe bool
	kq        *userKqueue     // of a kqueue: what it watches (see kqueue.go)
	necp      *necpSession    // of a session with the kernel's network policy (see necp.go)
	state     *status         // of what is no pipe and no socket: what fcntl reads and sets of it (see fileflags.go)
	refs      int             // the descriptors it is, in every process: a duplicate and a child's are the same end
	both      bool            // of a named pipe's: it was opened to read and to write
	sem       *namedSemaphore // of a semaphore's: which
	own       bool            // of a terminal's: it was opened as /dev/tty, the session's own
	dead      bool            // of a terminal's: the terminal was taken from it when its session's leader ended
	tty       *terminal       // of an end of a terminal: which, and
	first     bool            // that it is the first end, the one that is typed at
}

func (f *fd) size() int64 {
	if f.kind == cacheFD {
		return f.cache.Size()
	}
	return int64(len(f.bytes()))
}

// bytes are what a descriptor of bytes holds: the shared memory it names, if it names any.
func (f *fd) bytes() []byte {
	if f.shared != nil {
		return f.shared.data
	}
	return f.data
}

// pread returns up to n bytes of a file at off, or a negated errno.
func (p *Process) pread(f *fd, off, n uint64) ([]byte, int64) {
	switch f.kind {
	case hostFD:
		p.m.beforeRead(f.host)
		data := hostBuffer(n)
		got, err := syscall.Pread(f.host, data, int64(off))
		if err != nil {
			return nil, hostErrno(err)
		}
		return data[:got], 0
	case cacheFD:
		data, err := away(p.m.clock, func() ([]byte, error) { return f.cache.Pread(int64(min(off, 1<<62)), int64(min(n, 1<<40))) })
		if err != nil {
			stopf("read %s: %v", f.path, err)
		}
		return data, 0
	case dataFD:
		data := f.bytes()
		off = min(off, uint64(len(data)))
		return data[off:min(off+min(n, 1<<40), uint64(len(data)))], 0
	}
	return nil, -eISDIR
}

// target is what a guest path resolves to.
type target struct {
	kind    fdKind
	path    string // canonical
	host    string
	root    *os.Root // under the home or a mount: every host access stays inside it, and the guest may change it
	dataOn  uint64   // and that is the home or a path of the data volume: the host's device it is kept on, plus one (see volume.go)
	rel     string   // relative path inside root ("." for the mount root itself)
	data    []byte
	cache   dyldcache.File
	readDir func() ([]string, error)
	link    string // of a symbolic link on the system volume to the directory it is (see SystemFile.Link)
	// Of what the system volume has: its owner and permissions there, and those of the link.
	inode, linkInode *Inode
	// leads, of a symbolic link under the home or a mount to a path that is not in the same host
	// directory, is the guest path it leads to (see leadsOut).
	leads string
}

// fd is the descriptor for a target that is not a host file.
func (t target) fd() *fd {
	return &fd{kind: t.kind, data: t.data, cache: t.cache, readDir: t.readDir, path: t.path, inode: t.inode}
}

// hostErrno is the negated errno of a failed host call.
func hostErrno(err error) int64 {
	var errno syscall.Errno
	if errors.As(err, &errno) && errno != 0 {
		return -guestErrno(errno)
	}
	if err != nil && strings.Contains(err.Error(), "escapes") {
		return -eACCES
	}
	return -eINVAL
}

// mounted is an open host directory mounted at a canonical guest directory.
type mounted struct {
	guest  string
	root   *os.Root
	dataOn uint64 // of the home or a path of the data volume: the host's device it is kept on, plus one
	// Of a path of the data volume (see data.go): what the release's template has there, the
	// directories that have been given it, and the file that is kept of which.
	template  func(rel string) (TemplateEntry, error)
	populated map[string]bool
	record    *os.File
	// kept is who the template says owns what earlier runs filled in, by path, with the host's
	// number for the file it was said of.
	kept map[string]kept
}

func openMounts(mounts []Mount) ([]mounted, error) {
	out := make([]mounted, 0, len(mounts))
	for _, m := range mounts {
		host, err := filepath.EvalSymlinks(m.Host)
		if err != nil {
			closeMounts(out)
			return nil, err
		}
		root, err := os.OpenRoot(host)
		if err != nil {
			closeMounts(out)
			return nil, err
		}
		mt := mounted{guest: canonical(m.Guest), root: root, template: m.Template}
		if m.Data {
			var st syscall.Stat_t
			if err := syscall.Stat(host, &st); err != nil {
				root.Close()
				closeMounts(out)
				return nil, err
			}
			mt.dataOn = uint64(st.Dev) + 1
		}
		if err := mt.openRecord(m.Record); err != nil {
			root.Close()
			closeMounts(out)
			return nil, err
		}
		out = append(out, mt)
	}
	slices.SortStableFunc(out, func(a, b mounted) int { return len(b.guest) - len(a.guest) })
	return out, nil
}

func closeMounts(mounts []mounted) {
	for _, m := range mounts {
		if m.root != nil {
			m.root.Close()
		}
		if m.record != nil {
			m.record.Close()
		}
	}
}

// leadsOut finds, on the way to a path of a mount, the symbolic link that leads out of the
// mount's host directory, if there is one: a link to a path from the root, as a new Mac's
// /var/db/timezone/zoneinfo is to /usr/share/zoneinfo.default, or one that climbs out. The host
// follows no such link, as the path is the guest's and not its own. It says the guest path the
// whole leads to, and whether the link is the path's last part. A link that stays inside is the
// host's to follow, and is written out here too only when another after it leads out.
func (m mounted) leadsOut(rel string) (to string, last, ok bool) {
	if _, err := m.root.Stat(rel); err == nil || errors.Is(err, fs.ErrNotExist) {
		return "", false, false
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		at := pathpkg.Join(parts[:i+1]...)
		fi, err := m.root.Lstat(at)
		if err != nil {
			return "", false, false
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		text, err := m.root.Readlink(at)
		if err != nil {
			return "", false, false
		}
		if !strings.HasPrefix(text, "/") {
			text = pathpkg.Join(m.guest, pathpkg.Dir(at), text)
		}
		return pathpkg.Join(append([]string{text}, parts[i+1:]...)...), i == len(parts)-1, true
	}
	return "", false, false
}

// followed is what a path resolves to with a last link that leads out of its mount followed.
func (p *Process) followed(t target, ok bool) (target, bool) {
	for links := 0; ok && t.leads != "" && links < 16; links++ {
		t, ok = p.resolve(t.leads)
	}
	// A link under the home or a mount, which the host follows with nothing said: what it
	// leads to, and what that leads to if it is a link too, is reached by the way to it, which
	// may be closed, as measured (a link to a file in a directory since closed). Asked only
	// where some file's mode or owner is not the host's own.
	if ok && t.root != nil && len(p.m.said)+len(p.m.owners) != 0 && p.uid != 0 {
		for link, at, links := t.path, t, 0; links < 8 && at.root != nil; links++ {
			text, err := at.root.Readlink(at.rel)
			if err != nil {
				break
			}
			to, closed := p.leadsBehind(link, text)
			if closed {
				p.blocked = eACCES
				return t, false
			}
			var there bool
			link = to
			if at, there = p.lookup(to); !there {
				break
			}
		}
	}
	return t, ok
}

func (m mounted) rel(path string) string {
	if path == m.guest {
		return "."
	}
	return strings.TrimPrefix(path[len(m.guest):], "/")
}

func openInRoot(root *os.Root, rel string, flags int, mode uint32) (int, error) {
	f, err := root.OpenFile(rel, flags, os.FileMode(mode))
	if err != nil {
		return -1, err
	}
	defer f.Close()
	h, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return -1, err
	}
	syscall.CloseOnExec(h)
	return h, nil
}

func statInRoot(root *os.Root, rel string, follow bool, st *syscall.Stat_t) error {
	var (
		fi  os.FileInfo
		err error
	)
	if follow {
		fi, err = root.Stat(rel)
	} else {
		fi, err = root.Lstat(rel)
	}
	if err != nil {
		return err
	}
	raw, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return syscall.EINVAL
	}
	*st = *raw
	return nil
}

// readDirInRoot is a host directory's names, in the host's order. Of one that may be read and
// not searched it is also the kind (DT_*) the host's listing has by each name, which is all
// that can be known of what is inside: nothing in it can be looked at.
func readDirInRoot(root *os.Root, rel string, said map[hostFile]uint16) ([]string, map[string]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	var st syscall.Stat_t
	if err != nil || syscall.Fstat(int(f.Fd()), &st) != nil {
		return names, nil, err
	}
	if mode, odd := said[hostFileOf(&st)]; st.Mode&0o100 != 0 && (!odd || mode&0o100 != 0) {
		return names, nil, nil // one that is searched: what is in it is looked at
	}
	return names, hostDirKinds(f), nil
}

func closeHost(fd int) { syscall.Close(fd) }

// canonical is the normalized guest path with the cryptex symlink of a real system followed, so a
// file has one identity (and one inode number) however the guest names it.
func canonical(path string) string {
	path = pathpkg.Clean(path)
	switch {
	case under(path, cryptexLink):
		path = cryptex + path[len(cryptexLink):]
	case under(path, "/etc"), under(path, "/var"), under(path, "/tmp"): // links to the same under /private
		path = "/private" + path
	}
	return path
}

// The users the machine has, as the files a process falls back on when no directory service
// answers: root, and the one every other guest runs as. Root reads the second file, which has
// three more fields.
const (
	passwdFile       = "/private/etc/passwd"
	masterPasswdFile = "/private/etc/master.passwd"
)

func passwd(u user, master bool) string {
	id := strconv.Itoa(int(u.uid)) + ":" + strconv.Itoa(int(u.gid))
	if master {
		return "root:*:0:0::0:0:System Administrator:/var/root:/bin/sh\n" + u.name + ":*:" + id + "::0:0:" + u.name + ":" + u.home + ":" + u.shell + "\n"
	}
	return "root:*:0:0:System Administrator:/var/root:/bin/sh\n" + u.name + ":*:" + id + ":" + u.name + ":" + u.home + ":" + u.shell + "\n"
}

func crcInode(path string) uint64 { return uint64(crc32.ChecksumIEEE([]byte(path)) & 0xFFFFFF) }

// Mount is a host directory the guest sees, and may change, at a guest directory. The home is one.
type Mount struct {
	Guest, Host string
	// Data says the mount is the home or a path of the data volume, which are one volume to a
	// guest. Any other mount is a volume of its own.
	Data bool
	// Template, of a path of the data volume, is what the release's template of one has at a
	// path under it, and Record the host file kept of the directories given that (see data.go).
	Template func(rel string) (TemplateEntry, error)
	Record   string
}

// CheckMounts says what is wrong with the mounts a guest is given besides its home (GuestHome or
// GuestHomeIOS): each is at an absolute guest directory of its own, and neither the root nor the
// home.
func CheckMounts(mounts []Mount, home string) error {
	seen := map[string]bool{canonical(home): true}
	for _, m := range mounts {
		guest := canonical(m.Guest)
		switch {
		case !strings.HasPrefix(m.Guest, "/") || guest == "/":
			return fmt.Errorf("a mount at %q: it has to be at an absolute guest directory other than the root", m.Guest)
		case guest == canonical(home):
			return fmt.Errorf("a mount at %s: that is the guest's home, which is given as the home", guest)
		case seen[guest]:
			return fmt.Errorf("two mounts at %s", guest)
		}
		seen[guest] = true
	}
	return nil
}

// under reports whether a path is a directory or something below it.
func under(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// resolve maps a guest path to what backs it. Only the home, the mounts, the guest executable and
// three device nodes ever name host files; everything else is served from memory or does not exist.
func (p *Process) resolve(path string) (target, bool) {
	if path == "" {
		return target{}, false
	}
	path = p.absolute(path)
	if errno := p.refusals[path]; errno != 0 { // a step up in it could not be taken (see upward)
		p.blocked = errno
		return target{path: path}, false
	}
	if p.blockedAt(path) {
		p.blocked = eACCES
		return target{path: path}, false
	}
	t, ok := p.lookup(path)
	if t.path == "" {
		t.path = path
	}
	return t, ok
}

// blockedAt says whether a directory on the way to a path may not be searched by who asks, so
// that nothing can be said or done of the name: EACCES, whatever the call, whether or not the
// name is there, as measured of names under root's home (tests/kernel/files/sysvolume.c). The
// call that came upon it is answered so when it has ended (see bsd). From the source and not
// measured: root passes any directory.
//
// A name given from a directory, the one the process is in or a descriptor's, is looked for
// from there: what leads to that directory is not asked again, so a process goes on in a
// directory whose parent has since been closed to it, as measured
// (tests/kernel/files/relsearch.c). Not as a Mac has it: a name that goes up from there
// through a closed directory and down again by another way is let through.
func (p *Process) blockedAt(path string) bool {
	if p.uid == 0 || path == "/" || slices.Contains(p.starts, path) {
		return false
	}
	for dir := pathpkg.Dir(path); ; dir = pathpkg.Dir(dir) {
		if !p.searchable(dir) {
			return true
		}
		if dir == "/" || slices.Contains(p.starts, dir) {
			return false
		}
	}
}

// from is a name of the call being answered having been given from a directory: it is looked
// for from there (see blockedAt). A call may be given names from more than one.
func (p *Process) from(dir string) {
	if dir = canonical(dir); !slices.Contains(p.starts, dir) {
		p.starts = append(p.starts, dir)
	}
}

// leadsBehind is where the text of a link at a path leads, and whether that is behind a
// directory that may not be searched: text that is no whole path is walked from the directory
// the link is in, which is where that leads if it is reached by a link itself, as measured.
func (p *Process) leadsBehind(link, text string) (string, bool) {
	starts := p.starts
	defer func() { p.starts = starts }()
	p.starts = nil
	to, dir := text, ""
	if !strings.HasPrefix(text, "/") {
		dir = p.physical(pathpkg.Dir(link))
		p.starts = []string{dir}
		to = dir + "/" + text
	}
	if goesUp(to) {
		p.starts = nil // the walk says where it is looked for from
		to = p.upward(to, dir)
	}
	to = canonical(to)
	return to, p.refusals[to] != 0 || p.blockedAt(to)
}

// passKey is a directory and who would pass it.
type passKey struct {
	uid, gid uint32
	dir      string
}

// searchable says whether a directory's mode lets who asks search it. What was found is kept
// until a directory's mode or owner changes or a name is made, moved or taken away that could
// be one (Machine.changed). A name that is no directory, or is not there, is not what stands
// in the way: the call finds that out for itself.
func (p *Process) searchable(dir string) bool {
	m := p.m
	if m.passSeen != m.passChanges {
		clear(m.pass)
		m.passSeen = m.passChanges
	}
	key := passKey{p.uid, p.gid, dir}
	if ok, known := m.pass[key]; known {
		return ok
	}
	if m.pass == nil {
		m.pass = map[passKey]bool{}
	}
	// Taken to be passable while it is looked at, which may come back here, and where looking
	// has led through several links already: a link that leads under itself, or two that
	// lead to each other, which the call finds out for itself.
	if p.searching >= 4 {
		return true
	}
	m.pass[key] = true
	was := p.blocked
	p.blocked = 0
	p.searching++
	_, info, errno := p.describePath("search", dir, true)
	// A link on the way is passed by the way to what it leads to: one under the home or a
	// mount, which the host follows with nothing said, as measured, and one that leads out of
	// there.
	beyond := false
	if t, ok := p.lookup(dir); ok && t.leads != "" {
		_, beyond = p.leadsBehind(dir, t.leads)
	} else if ok && t.root != nil {
		if text, err := t.root.Readlink(t.rel); err == nil {
			_, beyond = p.leadsBehind(dir, text)
		}
	}
	p.searching--
	p.blocked = was
	m.pass[key] = !beyond && (errno != 0 || info.mode&syscall.S_IFMT != syscall.S_IFDIR || p.permits(info, 1))
	return m.pass[key]
}

// changed is a mode, an owner or a name having changed: what was found of which directories
// may be passed is found anew.
func (m *Machine) changed() { m.passChanges++ }

// place is where a path leads, for the calls that make and move names: whether something is
// there, whether its directory is, whether that is the system volume, and if so why it is not
// written (see systemRefusal), by what is there or else by the directory.
type place struct {
	there, dirThere, system bool
	refusal                 int64
}

func (p *Process) placeOf(path string) place {
	var pl place
	t, ok := p.resolve(path)
	if ok && t.root != nil { // a name under the home or a mount, which may name nothing
		var st syscall.Stat_t
		ok = statInRoot(t.root, t.rel, false, &st) == nil
	}
	if pl.there, pl.dirThere = ok, ok; ok {
		if pl.system = p.onSystem(t); pl.system {
			pl.refusal = systemRefusal(t)
		}
		return pl
	}
	dir, there := p.followed(p.resolve(pathpkg.Dir(p.absolute(path))))
	if pl.dirThere = there; there && p.onSystem(dir) {
		pl.system, pl.refusal = true, systemRefusal(dir)
	}
	return pl
}

// notMade is why a name is not made where nothing of the guest's is kept, by a call that
// makes a link, a pipe or the like: ENOENT where its directory is not; exists where something
// is there already, unless that carries the flag that it is restricted and the call is one
// that says so first (restrictedFirst: symlink and link, as measured); and where nothing is,
// the system volume's refusal. Elsewhere EACCES, which was not measured.
func (p *Process) notMade(path string, exists int64, restrictedFirst bool) int64 {
	switch pl := p.placeOf(path); {
	case !pl.dirThere:
		return -eNOENT
	case pl.there && restrictedFirst && pl.refusal == -ePERM:
		return -ePERM
	case pl.there:
		return exists
	case pl.refusal != 0:
		return pl.refusal
	}
	return -eACCES
}

// rootLinks are the names in the root directory that every release has as symbolic links to
// the same under /private, which canonical follows.
var rootLinks = map[string]string{"/etc": "private/etc", "/tmp": "private/tmp", "/var": "private/var"}

// rootLink is the text of the link a path names, if it names one of those itself.
func (p *Process) rootLink(path string) string {
	if path == "" {
		return ""
	}
	return rootLinks[p.absoluteLink(path)]
}

// absoluteLink is a guest path from the root, with no link in it followed.
func (p *Process) absoluteLink(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = p.workingDirectory() + "/" + path
	}
	return pathpkg.Clean(path)
}

// asDirectory says whether a path is written to name a directory: it ends in a slash, or in a
// dot after one. Such a path is of what a link leads to and never of the link, is ENOTDIR of
// what is no directory, and of nothing is ENOENT even where something was to be made: as
// measured of lstat, stat, readlink, open and access (macOS 27.2.1, by hand).
func asDirectory(path string) bool {
	path = strings.TrimSuffix(path, ".")
	return strings.HasSuffix(path, "/") && strings.Trim(path, "/") != ""
}

// absolute is the canonical form of a guest path, relative to the working directory or not.
func (p *Process) absolute(path string) string {
	from := ""
	if !strings.HasPrefix(path, "/") {
		from = p.workingDirectory()
		path = from + "/" + path
	}
	switch {
	case goesUp(path):
		path = p.upward(path, from)
	case from != "":
		p.from(from) // the name is looked for from here (see blockedAt)
	}
	return canonical(path)
}

// goesUp says whether a path has a step up in it ("..").
func goesUp(path string) bool {
	return strings.Contains(path, "/../") || strings.HasSuffix(path, "/..")
}

// upward is a whole path with steps up in it, walked as the kernel walks it and not as it is
// written: a step up is from a directory that is there and may be searched (ENOENT, ENOTDIR
// or EACCES of the whole path otherwise, which the call is answered with when it has ended;
// see bsd), and from where a link leads, not from where the link is. As measured
// (tests/commands/paths.list, tests/kernel/files/relsearch.c). from is the directory the name
// was given from, if it was given from one: what is looked for is then looked for from the
// highest directory the steps reach, and what leads to that is not asked (see blockedAt).
func (p *Process) upward(path, from string) string {
	key := from + "\x00" + path
	if w, ok := p.walks[key]; ok { // walked before for this call, which asks of one path many times
		if w.top != "" {
			p.from(w.top)
		}
		return w.to
	}
	cur, top := "/", ""
	if from != "" {
		top = canonical(from)
	}
	done := func(to string) string {
		if p.walks == nil {
			p.walks = map[string]walk{}
		}
		if p.walks[key] = (walk{to, top}); top != "" {
			p.from(top) // what is looked for is looked for from the highest directory reached
		}
		return to
	}
	parts := strings.Split(path, "/")
	for i, c := range parts {
		switch c {
		case "", ".":
		case "..":
			was, starts := p.blocked, p.starts
			if p.blocked = 0; top != "" { // this far it has been looked for from here
				p.starts = append(slices.Clone(starts), top)
			}
			_, info, errno := p.describePath("search", cur, true)
			if p.blocked != 0 { // what leads to it may not be searched: that, and not that nothing is there
				errno = -p.blocked
			}
			p.blocked, p.starts = was, starts
			switch {
			case errno != 0:
			case info.mode&syscall.S_IFMT != syscall.S_IFDIR:
				errno = -eNOTDIR
			case p.uid != 0 && !p.permits(info, 1):
				errno = -eACCES
			}
			if errno != 0 {
				// A name nothing has, so that what is refused is this path as it was given and
				// not the file its steps would come to if they were only read.
				whole := pathpkg.Clean(cur+"/"+strings.Join(parts[i:], "/")) + "/\x00"
				if p.refusals == nil {
					p.refusals = map[string]int64{}
				}
				p.refusals[canonical(whole)] = -errno
				return done(whole)
			}
			if cur = canonical(pathpkg.Dir(p.physical(cur))); top != "" && !under(cur, top) {
				top = cur
			}
		default:
			cur = pathpkg.Join(cur, c)
		}
	}
	return done(cur)
}

// walk is a path with steps up as it was walked: where it came to, and the highest directory
// it reached if it was given from one.
type walk struct{ to, top string }

// physical is the directory a path names when the path is a link to one: where it leads.
func (p *Process) physical(dir string) string {
	dir = canonical(dir) // /tmp, /var and /etc are links to the same under /private
	for links := 0; links < 16; links++ {
		t, ok := p.lookup(dir)
		if !ok {
			return dir
		}
		to := t.leads
		switch {
		case to != "":
		case t.root != nil:
			to, _ = t.root.Readlink(t.rel)
		case t.kind == dirFD && t.path != "" && canonical(t.path) != dir: // a link of the system volume
			return canonical(t.path)
		}
		if to == "" {
			return dir
		}
		if !strings.HasPrefix(to, "/") { // from the directory the link is in, which is where that leads
			to = p.physical(pathpkg.Dir(dir)) + "/" + to
		}
		if goesUp(to) {
			to = p.upward(to, "")
		}
		dir = canonical(to)
	}
	return dir
}

func (p *Process) lookup(path string) (target, bool) { return p.lookupThrough(path, 0) }

// lookupThrough is lookup, having followed so many links of the system volume that lead off it.
func (p *Process) lookupThrough(path string, links int) (target, bool) {
	switch {
	case path == GuestExe && p.path == GuestExe: // a file of the host's
		return target{kind: hostFD, host: p.exePath}, true
	case path == p.path: // one of the build's
		return target{kind: dataFD, data: p.exe, inode: p.systemInode(path)}, true
	case path == guestDyld:
		return target{kind: dataFD, data: p.dyld}, true
	case path == passwdFile:
		return target{kind: dataFD, data: []byte(passwd(p.m.user, false)), inode: p.madeUp(0o644)}, true
	case path == masterPasswdFile:
		return target{kind: dataFD, data: []byte(passwd(p.m.user, true)), inode: p.madeUp(0o600)}, true
	}
	if f, ok := p.cachePaths[path]; ok {
		return target{kind: cacheFD, cache: f}, true
	}
	for i, m := range p.mounts {
		switch {
		case under(path, m.guest):
			rel := m.rel(path)
			p.m.populate(&p.mounts[i], rel)
			t := target{kind: hostFD, root: m.root, rel: rel, dataOn: m.dataOn}
			if to, last, ok := m.leadsOut(rel); ok && links < 16 {
				if !last {
					return p.lookupThrough(canonical(to), links+1)
				}
				t.leads = to // the link itself is what is named: where it leads is for whoever follows it
			}
			return t, true
		case under(m.guest, path): // on the way to it
			return p.onTheWay(path), true
		}
	}
	switch {
	case path == "/dev/null" || path == "/dev/zero" || path == "/dev/urandom" || path == "/dev/random":
		return target{kind: hostFD, host: path}, true
	case under(p.cacheDir, path):
		return p.onTheWay(path), true
	}
	switch path {
	case devFDs: // a directory of root's that nobody writes, as measured
		return target{kind: dirFD, inode: p.madeUp(0o555)}, true
	case "/", "/System/Volumes", dataMount, "/System", "/System/Library", "/usr", "/usr/lib", "/private", "/private/var", "/private/etc", "/var", "/var/root", "/private/var/root", "/tmp", "/private/tmp", "/emu":
		return p.onTheWay(path), true
	}
	switch f := p.systemFile(path); {
	case f.Dir:
		return target{kind: dirFD, path: f.Real, readDir: f.ReadDir, link: f.Link, inode: f.Inode, linkInode: f.LinkInode}, true
	case f.File != nil:
		return target{kind: cacheFD, cache: f.File, link: f.Link, inode: f.Inode, linkInode: f.LinkInode}, true
	case f.Leads != "" && links < 16:
		// A link of the system volume to what is not on it (/usr/share/zoneinfo is one into the
		// data volume): what is there, by the guest's own paths.
		if t, ok := p.lookupThrough(canonical(f.Leads), links+1); ok {
			t.link, t.linkInode = f.Link, f.LinkInode
			return t, true
		}
	}
	if under(pathpkg.Dir(p.path), path) { // on the way to its own executable
		return target{kind: dirFD}, true
	}
	return target{}, false
}

// onTheWay is a directory that is answered here without the system volume being asked whether
// it is there (the root, the directories every release has, those on the way to a mount or to
// the shared cache): with the owner and permissions the volume has for it, and its names there
// if the volume has it, so that it lists what a Mac's does.
func (p *Process) onTheWay(path string) target {
	f := p.systemFile(path)
	t := target{kind: dirFD, inode: f.Inode}
	if f.Dir {
		t.readDir = f.ReadDir
	}
	return t
}

// systemFile is what the release's system volume has at a path, if a guest may read it there.
func (p *Process) systemFile(path string) SystemFile {
	if p.system == nil {
		return SystemFile{}
	}
	f, err := away(p.m.clock, func() (SystemFile, error) { return p.system(path) })
	if err != nil {
		stopf("%v", err)
	}
	return f
}

// madeUp is the owner and permissions of a file that is made up here and that a Mac was
// measured to have as root's, with that mode (macOS 27.2.1). What an iPhone has is not known,
// and left out there: the file is the guest's own.
func (p *Process) madeUp(mode uint16) *Inode {
	if p.cache.Platform == dyldcache.IOS {
		return nil
	}
	return &Inode{Mode: mode}
}

// systemTime is when the files of the system volume were last written, as the image has it of
// one every release has; none if it has none.
func (p *Process) systemTime() int64 {
	if p.m.systemWritten == 0 {
		p.m.systemWritten = -1
		if inode := p.systemInode("/System/Library/CoreServices/SystemVersion.plist"); inode != nil && inode.Times[1] != 0 {
			p.m.systemWritten = inode.Times[1]
		}
	}
	return max(p.m.systemWritten, 0)
}

// systemInode is the owner and permissions the system volume has for a path that is answered
// here without it (the root, the directories every release has, a guest's own program), if it
// has the path.
func (p *Process) systemInode(path string) *Inode { return p.systemFile(path).Inode }

// statBuf is a struct stat64 of a file the user owns.
func statBuf(i fileInfo, owner user) []byte {
	b := make([]byte, 144)
	le.PutUint32(b[0:], i.dev)
	le.PutUint16(b[4:], i.mode)
	le.PutUint16(b[6:], max(i.links, 1))
	le.PutUint64(b[8:], i.ino)
	if i.owner != nil {
		owner.uid, owner.gid = i.owner.UID, i.owner.GID
	}
	le.PutUint32(b[16:], owner.uid)
	le.PutUint32(b[20:], owner.gid)
	for n, at := 0, 32; at < 96; n, at = n+1, at+16 { // access, modification, change and birth times
		if le.PutUint64(b[at:], uint64(i.mtime)); i.timed {
			seconds, nanos := timespec(i.times[n])
			le.PutUint64(b[at:], seconds)
			le.PutUint64(b[at+8:], nanos)
		}
	}
	le.PutUint64(b[96:], uint64(i.size))
	if i.mode&syscall.S_IFMT != syscall.S_IFDIR { // a directory's names take no blocks of its own
		le.PutUint64(b[104:], align(uint64(i.size), volumeBlockSize)/512)
	}
	le.PutUint32(b[112:], volumeBlockSize)
	le.PutUint32(b[116:], i.flags)
	return b
}

const (
	fixedMtime = 1700000000
	modeDir    = syscall.S_IFDIR | 0o755
	modeFile   = syscall.S_IFREG | 0o755
	modeLink   = syscall.S_IFLNK | 0o755
)

// putStat writes the struct stat64 of a file that was described without an error.
func (p *Process) putStat(buf uint64, info fileInfo, errno int64) int64 {
	if errno == 0 {
		p.write(buf, statBuf(info, p.m.user))
	}
	return errno
}

func (p *Process) doStat(path string, buf uint64, follow bool) int64 {
	if _, link := standardLink(p.absoluteLink(path)); link && !follow && !asDirectory(path) { // the link itself
		_, info, errno := p.describePath("lstat", path, false)
		return p.putStat(buf, info, errno)
	}
	if n, ok := p.descriptorAt(path); ok { // what the descriptor is, followed or not
		return sysFstat64(p, []uint64{n, buf})
	}
	if p.absolute(path) == ownTerminal {
		p.write(buf, ownTerminalStat())
		return 0
	}
	if t := p.terminalNamed(path); t != nil {
		p.write(buf, p.terminalStat(t, false))
		return 0
	}
	_, info, errno := p.describePath("stat", path, follow)
	return p.putStat(buf, info, errno)
}

func sysStat64(p *Process, a []uint64) int64  { return p.doStat(p.cstr(a[0]), a[1], true) }
func sysLstat64(p *Process, a []uint64) int64 { return p.doStat(p.cstr(a[0]), a[1], false) }

func sysFstatat64(p *Process, a []uint64) int64 {
	return p.atPath(a[0], a[1], func(path string) int64 { return p.doStat(path, a[2], a[3]&0x20 == 0) })
}

// The stat calls that also tell of a file's security (stat64_extended and the like, which
// mkdir -p, ls -l and cp use): {path or descriptor, where the status goes, where the security
// goes, and how much room there is for it, then how much it takes}. Of a file that has no access
// control list a Mac's kernel was measured to say 44 bytes, which are a mark, no owner's and no
// group's identifier, and that there is no list; of one that has, the list follows (see
// acls.go). With less room nothing is written but how much it takes; of a pipe or a socket it
// takes none. The file is named by what the host calls it, if it is one of the host's.
func (p *Process) statExtended(a []uint64, stat func() int64, vnode bool, file func() (hostFile, bool)) int64 {
	security, size := a[2], a[3]
	if errno := stat(); errno != 0 || security == 0 {
		return errno
	}
	if size == 0 { // nowhere to say how much it takes: after the file is found, as measured
		return -eFAULT
	}
	var record []byte
	if vnode {
		id, known := file()
		record = p.m.filesec(id, known)
		if p.u64(size) >= uint64(len(record)) {
			p.write(security, record)
		}
	}
	p.write(size, le64(uint64(len(record))))
	return 0
}

// fileAt is the host file a path names, if it names one under the home or a mount.
func (p *Process) fileAt(path string, follow bool) (hostFile, bool) {
	_, id, errno := p.changeable(path, follow)
	return id, errno == 0
}

func sysStat64Extended(p *Process, a []uint64) int64 {
	path := p.cstr(a[0])
	return p.statExtended(a, func() int64 { return p.doStat(path, a[1], true) }, true, func() (hostFile, bool) { return p.fileAt(path, true) })
}

func sysLstat64Extended(p *Process, a []uint64) int64 {
	path := p.cstr(a[0])
	return p.statExtended(a, func() int64 { return p.doStat(path, a[1], false) }, true, func() (hostFile, bool) { return p.fileAt(path, false) })
}

func sysFstat64Extended(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	return p.statExtended(a, func() int64 { return sysFstat64(p, a) }, f == nil || !f.streams(), func() (hostFile, bool) {
		_, id, errno := p.changeableFD(a[0])
		return id, errno == 0
	})
}

func sysFstat64(p *Process, a []uint64) (r int64) {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	if f.kind == semFD { // as measured
		return -eBADF
	}
	if s := f.shared; f.kind == dataFD && s != nil {
		// Shared memory with a name: its mode with no kind of file, whose it is and its size,
		// and nothing else, as measured.
		b := make([]byte, 144)
		le.PutUint16(b[4:], s.mode)
		le.PutUint32(b[16:], s.uid)
		le.PutUint32(b[20:], s.gid)
		le.PutUint64(b[96:], uint64(len(s.data)))
		p.write(a[1], b)
		return 0
	}
	if f.necp != nil {
		return p.sessionOnly(a)
	}
	if q := f.pipe; f.kind == pipeFD && q.fifo && p.m.fifoPipes[q.named] == q {
		return p.doStat(q.path, a[1], true) // a named pipe is its file, as measured, while it has the name
	}
	if f.kind == pipeFD {
		p.write(a[1], p.pipeStat(f))
		return 0
	}
	if f.kind == ttyFD {
		if f.dead {
			p.unimplemented("fstat of a terminal that was taken from the descriptor", nil)
			return -eNOSYS
		}
		if f.own {
			p.write(a[1], ownTerminalStat())
			return 0
		}
		p.write(a[1], p.terminalStat(f.tty, f.first))
		return 0
	}
	if f.kind == socketFD {
		p.write(a[1], p.socketStat(f))
		return 0
	}
	info, errno := p.describeFD(f)
	return p.putStat(a[1], info, errno)
}

// noFork marks a descriptor as one that a copy of the process (fork) and a program it starts
// (posix_spawn) do not get, or takes the mark off. The program the process itself becomes
// (execve) keeps it, and a duplicate is not marked. As measured on macOS 27.2.1
// (tests/kernel/io/clofork.c); SQLite opens its files so.
func (p *Process) noFork(n uint64, marked bool) {
	if !marked {
		delete(p.clofork, n)
		return
	}
	if p.clofork == nil {
		p.clofork = map[uint64]bool{}
	}
	p.clofork[n] = true
}

// doOpen opens a path, and marks the descriptor as one a program the process starts is not to
// have, if the flags say so.
func (p *Process) doOpen(path string, flags, mode uint64) int64 {
	const cloexec = 0x1000000 // O_CLOEXEC
	n := p.openPath(path, flags, mode)
	if n >= 0 && flags&cloexec != 0 && p.current.state == runnable { // one that waits to open a named pipe has marked its own
		p.cloexec[uint64(n)] = true
	}
	if n >= 0 && flags&0x8000000 != 0 && p.current.state == runnable { // O_CLOFORK
		p.noFork(uint64(n), true)
	}
	return n
}

func (p *Process) openPath(path string, flags, mode uint64) int64 {
	// The shared cache's own files are not held to the limit: dyld opens them here, one
	// after another, where a Mac's kernel has the cache mapped before a program starts and no
	// descriptor is spent on it; a process with a low limit starts there, as measured
	// (ulimit -n 20; sh -c 'ulimit -n').
	if _, cache := p.cachePaths[p.absolute(path)]; p.full() && !cache {
		return -eMFILE
	}
	if n, ok := p.descriptorAt(path); ok {
		return p.openDescriptor(n, flags)
	}
	if n, ok := p.openTerminal(path, flags); ok {
		return n
	}
	const noFollow, symlink = 0x100, 0x200000 // O_NOFOLLOW, O_SYMLINK
	t, ok := p.resolve(path)
	if flags&(noFollow|symlink) == 0 || asDirectory(path) {
		t, ok = p.followed(t, ok)
	}
	if asDirectory(path) {
		if _, info, errno := p.describePath("open", path, true); errno != 0 {
			return errno
		} else if info.mode&syscall.S_IFMT != syscall.S_IFDIR {
			return -eNOTDIR
		}
	}
	if !ok {
		if flags&0x200 != 0 { // to be made, where nothing is made: in a directory of the system volume, or of the devices
			if errno := p.unwritable(path, true); errno != 0 {
				return errno
			}
		}
		p.note("  ENOENT open %s", path)
		return -eNOENT
	}
	if t.kind != hostFD {
		if errno := p.mayOpen(t, flags); errno != 0 {
			return errno
		}
	}
	opened := t.fd()
	switch t.kind {
	case hostFD:
		if t.path == GuestExe && flags&(3|0x200|0x400) != 0 { // the user's file: not written, created or truncated
			return -eACCES
		}
		if mf := p.m.mappedAt(t); mf != nil && flags&0x400 != 0 { // O_TRUNC of a file that is mapped
			defer p.m.reload(mf)
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
		var (
			h   int
			err error
		)
		if t.root != nil {
			// A file it creates has the mode asked for less the guest's umask, not the host's.
			var st syscall.Stat_t
			missing := statInRoot(t.root, t.rel, false, &st) != nil
			created := flags&0x200 != 0 && missing
			if flags&0x100 != 0 && !missing && st.Mode&syscall.S_IFMT == syscall.S_IFLNK {
				return -eLOOP // told not to follow, and it is a link
			}
			if !missing {
				// What its mode, as the guest sees it, does not let who asks do is refused: the
				// host's own mode is not the guest's for a file that is another's here.
				want := [4]uint16{4, 2, 6, 6}[flags&3]
				if flags&0x400 != 0 {
					want |= 2
				}
				info, errno := p.describe(hostFD, t.path, 0, nil, func(is *syscall.Stat_t) error {
					return statInRoot(t.root, t.rel, flags&(noFollow|symlink) == 0, is)
				})
				if errno == 0 && info.mode&syscall.S_IFMT != syscall.S_IFDIR && info.mode&syscall.S_IFMT != syscall.S_IFLNK && !p.permits(info, want) {
					return -eACCES
				}
				// A directory is opened to be read by who may read it, and to be searched only
				// (O_SEARCH) by who may search it, as measured.
				if need := uint16(4); errno == 0 && info.mode&syscall.S_IFMT == syscall.S_IFDIR && flags&3 == 0 {
					if flags&openExec != 0 {
						need = 1
					}
					if !p.permits(info, need) {
						return -eACCES
					}
				}
			}
			if len(p.m.fifos)+len(p.m.sockets) != 0 && !missing {
				is := st // what is opened: the file a link leads to, if there is one
				if st.Mode&syscall.S_IFMT != syscall.S_IFLNK || statInRoot(t.root, t.rel, true, &is) == nil {
					if p.m.fifos[hostFileOf(&is)] {
						return p.openFifo(hostFileOf(&is), uint16(is.Mode&0o7777), flags, p.absolute(path))
					}
					if p.m.sockets[hostFileOf(&is)] != nil { // a socket's name is connected to, not opened, as measured
						if flags&0xA00 == 0xA00 { // to be made, and only if it is not there
							return -eEXIST
						}
						return -eOPNOTSUPP
					}
				}
			}
			h, err = openInRoot(t.root, t.rel, hostFlags, uint32(mode&0o777))
			if err == nil && created {
				if err = syscall.Fchmod(h, uint32(mode&0o777)&^p.umask()); err != nil {
					syscall.Close(h)
				}
				p.created(t)
			}
		} else {
			h, err = syscall.Open(t.host, hostFlags|syscall.O_CLOEXEC, uint32(mode&0o777))
		}
		if err != nil {
			p.note("  open %s failed: %v", path, err)
			return hostErrno(err)
		}
		opened = &fd{kind: hostFD, host: h, path: t.path}
		opened.status().descriptors = 1
	}
	if !opened.streams() {
		opened.opened(flags)
	}
	n := p.newFD(opened)
	p.note("  open %s -> %d", path, n)
	return int64(n)
}

// timespec is a time in nanoseconds as seconds and the nanoseconds past them, which are never
// below zero, whatever the time.
func timespec(ns int64) (seconds, nanos uint64) {
	s, n := ns/1e9, ns%1e9
	if n < 0 {
		s, n = s-1, n+1e9
	}
	return uint64(s), uint64(n)
}

const (
	eFBIG       = 27
	sigXFSZ     = 25
	rlimitFsize = 1 // RLIMIT_FSIZE
)

// fileRoom is how many of the bytes a process would write at a place in a host file its
// limit on a file's size lets it (RLIMIT_FSIZE): all of them, those up to the limit of a
// write that would pass it, or none (EFBIG) of one that starts at or past it, for which the
// process is sent SIGXFSZ. Of a file and nothing else: a device, a pipe and a socket are not
// held to it. As measured (tests/kernel/files/fsizelimit.c) and as the source has it.
func (p *Process) fileRoom(host int, at int64, size uint64) (uint64, int64) {
	limit := p.rlimit(rlimitFsize)[0]
	if limit >= rlimInfinity || at < 0 || size <= limit && uint64(at) <= limit-size {
		return size, 0
	}
	var st syscall.Stat_t
	if syscall.Fstat(host, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return size, 0
	}
	if uint64(at) >= limit {
		p.sendSignal(pendingSignal{sig: sigXFSZ, status: sigXFSZ, pid: p.pid, uid: p.uid}) // to the process, as the source has it
		return 0, -eFBIG
	}
	return limit - uint64(at), 0
}

// tooLarge refuses a length a file is to be given that is past the limit (EFBIG, and
// SIGXFSZ), as measured of truncate and ftruncate.
func (p *Process) tooLarge(length uint64) int64 {
	if limit := p.rlimit(rlimitFsize)[0]; limit < rlimInfinity && int64(length) >= 0 && length > limit {
		p.sendSignal(pendingSignal{sig: sigXFSZ, status: sigXFSZ, pid: p.pid, uid: p.uid}) // to the process, as the source has it
		return -eFBIG
	}
	return 0
}

const maxFD = 10240

const openExec = 0x40000000 // O_EXEC: with O_DIRECTORY, to search a directory and not read it (O_SEARCH)

// umask is the process's file mode creation mask: 022, as launchd gives, until it changes it.
func (p *Process) umask() uint32 {
	if !p.cmaskSet {
		return 0o22
	}
	return p.cmask
}

func sysUmask(p *Process, a []uint64) int64 {
	old := p.umask()
	p.cmask, p.cmaskSet = uint32(a[0])&0o777, true
	return int64(old)
}

// sysFsync writes a host file's data out, through the host's fsync, which also answers for the
// standard descriptors (EINVAL for a pipe, as here). What is read from the system volume has
// nothing to write. Of shared memory and kqueues, what a kernel answers is not known here.
func sysFsync(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	switch {
	case f == nil:
		return -eBADF
	case f.kind == hostFD || f.kind == stdioFD:
		if err := syscall.Fsync(f.host); err != nil {
			return hostErrno(err)
		}
		return 0
	case f.shared == nil && f.path != "":
		return 0
	}
	p.unimplemented("fsync of shared memory or a kqueue", a)
	return -eNOSYS
}

// sysSync flushes what the guests wrote, which is the host's to write: through the host's sync.
func sysSync(p *Process, a []uint64) int64 {
	syscall.Sync()
	return 0
}

// fdLimit is the first descriptor number a process may not have: RLIMIT_NOFILE's soft limit.
func (p *Process) fdLimit() uint64 { return min(p.rlimit(rlimitNofile)[0], maxFD) }

// full says whether the process has as many descriptors as its limit lets it (EMFILE).
func (p *Process) full() bool { return p.allocFD(0) >= p.fdLimit() }

// allocFD returns the lowest unused file descriptor number at or above minFD.
func (p *Process) allocFD(minFD uint64) uint64 {
	for n := minFD; ; n++ {
		if p.fds[n] == nil {
			return n
		}
	}
}

// newFD gives a descriptor the lowest unused number.
func (p *Process) newFD(f *fd) uint64 {
	n := p.allocFD(0)
	p.fds[n] = f
	p.holds(n)
	return n
}

const atFDCWD = -2 // AT_FDCWD on Darwin

// at resolves a path argument relative to a directory descriptor (or cwd for AT_FDCWD).
func (p *Process) at(dirfd, path uint64) (string, int64) {
	s := p.cstr(path)
	if s == "" {
		return "", -eNOENT
	}
	if strings.HasPrefix(s, "/") {
		return s, 0
	}
	dir := p.workingDirectory()
	if int32(dirfd) != atFDCWD {
		var errno int64
		if dir, errno = p.descriptorDirectory(dirfd & 0xFFFFFFFF); errno != 0 {
			return "", errno
		}
	}
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	if !goesUp(dir + s) {
		p.from(dir) // the name is looked for from here (see blockedAt)
	}
	if goesUp(dir + s) { // walked from the directory, with how it ends kept for what asks of that
		walked := p.upward(dir+s, strings.TrimSuffix(dir, "/"))
		switch {
		case walked == "/":
		case strings.HasSuffix(s, "/."):
			walked += "/."
		case strings.HasSuffix(s, "/"):
			walked += "/"
		}
		return walked, 0
	}
	return dir + s, 0
}

// atPath calls f with a path argument resolved relative to a directory descriptor.
func (p *Process) atPath(dirfd, path uint64, f func(string) int64) int64 {
	s, errno := p.at(dirfd, path)
	if errno != 0 {
		return errno
	}
	return f(s)
}

func sysOpen(p *Process, a []uint64) int64 { return p.doOpen(p.cstr(a[0]), a[1], a[2]) }
func sysOpenat(p *Process, a []uint64) int64 {
	return p.atPath(a[0], a[1], func(path string) int64 { return p.doOpen(path, a[2], a[3]) })
}

func sysGuardedOpen(p *Process, a []uint64) int64 { return p.doOpen(p.cstr(a[0]), a[3], a[4]) }

// sysOpenDprotected (open_dprotected_np) opens a file with a data protection class, which only a
// device with a passcode enforces: here it is an open. dyld of iOS 15 to 17.0 creates files this
// way. Its arguments are the path, the flags, the class, flags of its own and the mode.
func sysOpenDprotected(p *Process, a []uint64) int64 { return p.doOpen(p.cstr(a[0]), a[1], a[4]) }

// sysGuardedOpenDprotected (guarded_open_dprotected_np) is both: the path, the guard and its
// flags, the open's flags, the class and its flags, and the mode. SQLite opens a database so
// once it is told how its file is protected (F_GETPROTECTIONCLASS).
func sysGuardedOpenDprotected(p *Process, a []uint64) int64 {
	return p.doOpen(p.cstr(a[0]), a[3], a[6])
}

func sysGuardedClose(p *Process, a []uint64) int64 { return sysClose(p, a[:1]) }
func sysGuardedWrite(p *Process, a []uint64) int64 { return sysWrite(p, []uint64{a[0], a[2], a[3]}) }
func sysGuardedPwrite(p *Process, a []uint64) int64 {
	return sysPwrite(p, []uint64{a[0], a[2], a[3], a[4]})
}

func (p *Process) closeFD(n uint64) int64 {
	f := p.fds[n]
	if f == nil {
		return -eBADF
	}
	delete(p.fds, n)
	delete(p.cloexec, n)
	delete(p.clofork, n)
	if f.kind == hostFD && len(p.m.ranges) != 0 {
		// The process's own locks on the file go with any descriptor of it that it closes.
		var st syscall.Stat_t
		if syscall.Fstat(f.host, &st) == nil && !p.keeps(hostFileOf(&st)) {
			p.m.unlockOwner(hostFileOf(&st), lockOwner{pid: p.pid})
		}
	}
	p.dropFD(f)
	return 0
}

// keeps says whether a process that is becoming another program keeps a descriptor of a file:
// its own locks on the file then stay, whatever else of it is closed for the new program, by
// the kernel's source (not measured).
func (p *Process) keeps(id hostFile) bool {
	if !p.becoming {
		return false
	}
	for n, f := range p.fds {
		var st syscall.Stat_t
		if f.kind == hostFD && !p.cloexec[n] && syscall.Fstat(f.host, &st) == nil && hostFileOf(&st) == id {
			return true
		}
	}
	return false
}

// dropFD lets go of what a descriptor was of, once it is in no table: closed, or sent over a
// socket and never received.
func (p *Process) dropFD(f *fd) {
	if f.kind == hostFD {
		p.hostClose(f)
	}
	if f.kind != hostFD && f.state != nil {
		p.m.unlockRanges(f.state) // a lock on the whole of it goes with its last descriptor
	}
	if f.shared != nil {
		f.shared.release()
	}
	if f.kind == pipeFD {
		p.pipeClosed(f)
	}
	if f.kind == socketFD {
		p.socketClosed(f)
	}
	if f.kind == ttyFD {
		p.terminalClosed(f)
	}
}

func sysClose(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	r := p.closeFD(a[0])
	if f != nil && f.kind == ttyFD && f.refs == 0 && !f.first && !f.tty.until.IsZero() {
		// The last close of a terminal's second end waits for what it wrote to be read, as
		// long as the terminal keeps it (see terminals.go).
		p.current.pipeOp = &pipeOp{f: f, closing: true}
		p.wait(waitPipe, f.tty.id, f.tty.until, result{})
	}
	return r
}

// hostBuffer bounds a guest-supplied length before it becomes a host allocation.
func hostBuffer(n uint64) []byte { return make([]byte, min(n, 1<<30)) }

func sysRead(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	if f.necp != nil {
		return p.sessionOnly(a)
	}
	if f.kind == dataFD && f.shared != nil || f.kind == semFD { // shared memory is mapped, not read or written, as measured; a semaphore likewise
		return -eNXIO
	}
	var data []byte
	switch {
	case f.kind == pipeFD:
		return p.pipeRead(f, []stretch{{a[1], a[2]}})
	case f.kind == socketFD:
		return p.socketReceive(f, []stretch{{a[1], a[2]}}, 0)
	case f.kind == ttyFD:
		return p.terminalRead(f, []stretch{{a[1], a[2]}})
	case f.kind == hostFD || (f.kind == stdioFD && f.host == 0): // the host keeps the position
		p.m.beforeRead(f.host)
		data = hostBuffer(a[2])
		n, err := syscall.Read(f.host, data)
		if err != nil {
			return hostErrno(err)
		}
		data = data[:n]
	case f.kind == stdioFD:
		return -eBADF
	default:
		var errno int64
		if data, errno = p.pread(f, uint64(max(f.pos, 0)), a[2]); errno != 0 {
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
	if f.kind == stdioFD || f.streams() || f.kind == dataFD && f.shared != nil {
		return -eSPIPE
	}
	data, errno := p.pread(f, a[3], a[2])
	if errno != 0 {
		return errno
	}
	p.write(a[1], data)
	return int64(len(data))
}

// maxKeptOutput is how much of what a guest writes to its standard output and error is kept for
// the end of the run. A guest that writes without end would otherwise take the host's memory.
const maxKeptOutput = 64 << 20

func sysWrite(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	if f.necp != nil {
		return p.sessionOnly(a)
	}
	if f.kind == dataFD && f.shared != nil {
		return -eNXIO
	}
	if f.kind == semFD { // as measured
		return -eBADF
	}
	if f.kind == pipeFD {
		return p.pipeWrite(f, p.read(a[1], a[2]))
	}
	if f.kind == socketFD {
		return p.socketSend(f, p.read(a[1], a[2]), 0, true)
	}
	if f.kind == ttyFD {
		return p.terminalWrite(f, p.read(a[1], a[2]))
	}
	if f.kind == stdioFD {
		if f.host != 1 && f.host != 2 {
			return -eBADF
		}
		data := p.read(a[1], a[2])
		// A process started by another writes where that one does, unless it was given
		// somewhere else to.
		owner := p.stdio
		kept, live := &owner.stdout, p.m.cfg.Stdout
		if f.host == 2 {
			kept, live = &owner.stderr, p.m.cfg.Stderr
		}
		if live != nil && owner == p.m.procs[0].stdio {
			// As it is written, and as it is: whoever reads it is the guest's own reader.
			if _, err := live.Write(data); err != nil {
				stopf("the guest's output could not be written: %v", err)
			}
			return int64(a[2])
		}
		text := strings.ToValidUTF8(string(data), "�")
		if owner.stdout.Len()+owner.stderr.Len()+len(text) > maxKeptOutput {
			stopf("%s wrote more than %d MB, and what a guest writes is kept until the run ends, unless it is the one guest of a run against one release, whose output is passed on as it is written", p.Label(), maxKeptOutput>>20)
		}
		kept.WriteString(text)
		if p.trace {
			syscall.Write(2, []byte(text))
		}
		return int64(a[2])
	}
	if f.kind != hostFD {
		return -eBADF
	}
	// Held to the limit from where the descriptor is, though one that adds to the file's end
	// writes there: as measured.
	at, _ := syscall.Seek(f.host, 0, 1)
	room, errno := p.fileRoom(f.host, at, a[2])
	if errno != 0 {
		return errno
	}
	return p.hostWrite(f, a[1], room)
}

// hostWrite writes guest memory to a host file where its descriptor is.
func (p *Process) hostWrite(f *fd, addr, size uint64) int64 {
	data := p.read(addr, size)
	p.m.beforeRead(f.host)
	n, err := syscall.Write(f.host, data)
	p.m.afterWrite(f.host)
	if err != nil {
		return hostErrno(err)
	}
	if n > 0 {
		f.wrote()
	}
	return int64(n)
}

// sysReadv reads into the pieces it is given one after another: of a pipe or a socket what one
// read gives, and of anything else until a piece is not filled.
func sysReadv(p *Process, a []uint64) int64 {
	if a[2] > 1024 {
		return -eINVAL
	}
	var into []stretch
	for i := uint64(0); i < a[2]; i++ {
		into = append(into, stretch{p.u64(a[1] + 16*i), p.u64(a[1] + 16*i + 8)})
	}
	switch f := p.fds[a[0]]; {
	case f != nil && f.kind == pipeFD:
		return p.pipeRead(f, into)
	case f != nil && f.kind == socketFD:
		return p.socketReceive(f, into, 0)
	}
	var got int64
	for _, s := range into {
		n := sysRead(p, []uint64{a[0], s.addr, s.size})
		switch {
		case n < 0 && got == 0:
			return n
		case n < 0:
			return got
		}
		if got += n; uint64(n) < s.size {
			break
		}
	}
	return got
}

// sysWritev writes the pieces it is given one after another, and stops at one that is cut short.
func sysWritev(p *Process, a []uint64) int64 {
	if a[2] > 1024 {
		return -eINVAL
	}
	if f := p.fds[a[0]]; f != nil && f.streams() { // one write, which may wait
		var all []byte
		for i := uint64(0); i < a[2]; i++ {
			all = append(all, p.read(p.u64(a[1]+16*i), p.u64(a[1]+16*i+8))...)
		}
		if f.kind == socketFD {
			return p.socketSend(f, all, 0, true)
		}
		return p.pipeWrite(f, all)
	}
	// The pieces are one write to the limit on a file's size: it is asked once, of all of
	// them, and they are cut short where it ends, as measured of two that pass it and of two
	// of which the first ends at it.
	room, host := ^uint64(0), p.fds[a[0]]
	if host != nil && host.kind == hostFD {
		var all uint64
		for i := uint64(0); i < a[2]; i++ {
			all += min(p.u64(a[1]+16*i+8), 1<<62)
		}
		at, _ := syscall.Seek(host.host, 0, 1)
		var errno int64
		if room, errno = p.fileRoom(host.host, at, all); errno != 0 {
			return errno
		}
	}
	var written int64
	for i := uint64(0); i < a[2]; i++ {
		base, length := p.u64(a[1]+16*i), p.u64(a[1]+16*i+8)
		var n int64
		if host != nil && host.kind == hostFD {
			length = min(length, room)
			n = p.hostWrite(host, base, length)
			room -= uint64(max(n, 0))
		} else {
			n = sysWrite(p, []uint64{a[0], base, length})
		}
		switch {
		case n < 0 && written == 0:
			return n
		case n < 0:
			return written
		}
		if written += n; uint64(n) < length {
			break
		}
	}
	return written
}

func sysPwrite(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f != nil && (f.streams() || f.kind == dataFD && f.shared != nil) {
		return -eSPIPE
	}
	if f == nil || f.kind != hostFD {
		return -eBADF
	}
	room, errno := p.fileRoom(f.host, int64(a[3]), a[2])
	if errno != 0 {
		return errno
	}
	p.m.beforeRead(f.host)
	n, err := syscall.Pwrite(f.host, p.read(a[1], room), int64(a[3]))
	p.m.afterWrite(f.host)
	if err != nil {
		return hostErrno(err)
	}
	if n > 0 {
		f.wrote()
	}
	return int64(n)
}

func sysLseek(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	if f.necp != nil {
		return p.sessionOnly(a)
	}
	off := int64(a[1])
	if f.kind == dataFD && f.shared != nil || f.kind == semFD { // as measured
		return -eSPIPE
	}
	if f.kind == dirFD || f.kind == hostFD && f.isDirectory() {
		// A directory's place is the entry that is next, as getdirentries counts them. Back at
		// the start it is read anew, so what was made or taken away since is seen: as measured
		// of rewinddir (tests/kernel/dev/devfdlist.c).
		switch {
		case a[2] == 0 && off >= 0:
			if f.pos = off; off == 0 && (f.kind == hostFD || f.path == devFDs) { // the system volume's do not change
				f.names = nil
			}
			return off
		case a[2] == 1 && off == 0:
			return f.pos
		}
		p.unimplemented("lseek of a directory from where it is or from its end", a)
		return -eNOSYS
	}
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
	// The limit is asked before anything else, of a descriptor that is none too: as measured.
	if errno := p.tooLarge(a[1]); errno != 0 {
		return errno
	}
	f := p.fds[a[0]]
	if f != nil && f.kind == semFD { // as measured
		return -eINVAL
	}
	if f != nil && f.shared != nil {
		return f.shared.resize(a[1])
	}
	if f != nil && (f.kind == cacheFD || f.kind == dataFD) { // a file that is not open to be written, as one of the system's never is: as measured
		return -eINVAL
	}
	if f == nil || f.kind != hostFD {
		return -eBADF
	}
	p.m.beforeRead(f.host)
	r := hostCall(syscall.Ftruncate(f.host, int64(a[1])))
	p.m.afterWrite(f.host)
	if r == 0 {
		f.wrote()
	}
	return r
}

func (p *Process) doAccess(path string) int64 {
	if n, ok := p.descriptorAt(path); ok {
		if p.fds[n] == nil {
			return -eBADF
		}
		return 0
	}
	t, ok := p.resolve(path)
	if ok && t.kind == hostFD {
		if t.root != nil {
			_, err := t.root.Stat(t.rel)
			if err != nil {
				return hostErrno(err)
			}
			return 0
		}
		var st syscall.Stat_t
		ok = syscall.Stat(t.host, &st) == nil
	}
	if !ok {
		p.note("  ENOENT access %s", path)
		return -eNOENT
	}
	return 0
}

// accessFor is access of a path for a purpose: to be read (4), written (2) or run (1), by the
// file's mode for who asks, its owner by the owner's bits alone, its group by the group's, and
// the rest by the others'. Purposes and flags there are not are not minded; a link that is not
// followed may be anything; a descriptor's name is not to be run, whatever it is of. To write
// what is on the system volume is EPERM, and the root itself EROFS. As measured on macOS 27.2.1
// (tests/kernel/files/accessfor.c). From the source and not measured: root may read and write anything,
// and run what anyone may run and any directory.
func (p *Process) accessFor(path string, mode uint64, follow bool) int64 {
	if n, ok := p.descriptorAt(path); ok && p.fds[n] != nil && mode&1 != 0 {
		return -eACCES
	}
	_, isDescriptor := p.descriptorAt(path)
	if isDescriptor {
		if errno := p.doAccess(path); errno != 0 {
			return errno
		}
	}
	t, info, errno := p.describePath("access", path, follow)
	want := uint16(mode & 7)
	switch {
	case errno != 0:
		return errno
	case want == 0, info.mode&syscall.S_IFMT == syscall.S_IFLNK:
		return 0
	}
	if want&2 != 0 && !isDescriptor && p.onSystem(t) {
		return systemRefusal(t)
	}
	if !p.permits(info, want) {
		if info.acl {
			p.unimplemented("access of a file with an access control list that its mode refuses", []uint64{mode})
			return -eNOSYS
		}
		return -eACCES
	}
	return 0
}

// unwritable is why a name where nothing is kept here is not removed, or not made: what is
// there on the system volume or among the devices is not removed (EPERM), nothing is made in a
// directory of those (EPERM; in the root EROFS), and what is there already is there (EEXIST).
// As measured of rm /bin/ls, mkdir /usr/newdir, a file made in / and in /dev, and unlink and
// mkdir of /dev/zero. Nothing where the place was not measured.
func (p *Process) unwritable(path string, making bool) int64 {
	if t, ok := p.resolve(path); ok {
		switch {
		case making:
			return -eEXIST
		case p.onSystem(t):
			return systemRefusal(t)
		case under(t.path, "/dev"):
			return -ePERM
		}
		return 0
	}
	if !making {
		return 0
	}
	if dir, there := p.followed(p.resolve(pathpkg.Dir(p.absolute(path)))); there {
		switch {
		case p.onSystem(dir):
			return systemRefusal(dir)
		case under(dir.path, "/dev"):
			return -ePERM
		}
	}
	return 0
}

// systemRefusal is why what is on the system volume is not written: what carries the flag that
// it is restricted (SF_RESTRICTED, as the image has it) is EPERM, and the rest EROFS, the
// volume being read-only. As measured of /usr, /usr/lib and /bin/ls, which carry it, and of /,
// which does not.
func systemRefusal(t target) int64 {
	if t.inode != nil && t.inode.Flags&0x80000 != 0 {
		return -ePERM
	}
	return -eROFS
}

// onSystem says whether what a path names is of the system volume, which nobody writes.
func (p *Process) onSystem(t target) bool {
	return t.root == nil && (t.kind == cacheFD || t.inode != nil) && t.path != dataMount && !under(t.path, "/dev") && p.volumeAt(t.path).mount == "/"
}

// permits says whether a file's mode lets who asks read (4), write (2) or run (1) it, every one
// of those asked: its owner by the owner's bits alone, its group by the group's, the rest by
// the others', as measured. From the source and not measured: root may read and write
// anything, and run what anyone may run and any directory.
func (p *Process) permits(info fileInfo, want uint16) bool {
	uid, gid := p.m.user.uid, p.m.user.gid
	if info.owner != nil {
		uid, gid = info.owner.UID, info.owner.GID
	}
	bits := info.mode & 7
	switch {
	case p.uid == 0:
		bits = 6
		if info.mode&0111 != 0 || info.mode&syscall.S_IFMT == syscall.S_IFDIR {
			bits = 7
		}
	case p.uid == uid:
		bits = info.mode >> 6 & 7
	case p.gid == gid:
		bits = info.mode >> 3 & 7
	}
	return bits&want == want
}

// mayOpen is why a file or directory that is not the host's (one of the system volume, or one
// made up here) is not opened as asked, if it is not. As measured on macOS 27.2.1, by hand: to
// be made and only if it is not there, it is there (EEXIST); a directory is not written
// (EISDIR); nothing of the system volume is written, emptied or added to (EPERM); and what its
// mode does not let who asks read is not read (EACCES).
func (p *Process) mayOpen(t target, flags uint64) int64 {
	info, errno := p.describe(t.kind, t.path, t.fd().size(), t.inode, nil)
	if errno != 0 {
		return errno
	}
	how := flags & 3
	switch directory := info.mode&syscall.S_IFMT == syscall.S_IFDIR; {
	case flags&0xA00 == 0xA00:
		return -eEXIST
	case directory && how != 0:
		return -eISDIR
	case (how != 0 || flags&0x400 != 0) && p.onSystem(t):
		return systemRefusal(t)
	case how != 0 || flags&0x400 != 0:
		p.unimplemented("open to write of a file that is made up here", []uint64{flags})
		return -eNOSYS
	case !p.permits(info, 4):
		return -eACCES
	}
	return 0
}

func sysAccess(p *Process, a []uint64) int64 { return p.accessFor(p.cstr(a[0]), a[1], true) }

// sysFaccessat is faccessat(directory, path, purpose, flags): of the link itself if told so
// (AT_SYMLINK_NOFOLLOW).
func sysFaccessat(p *Process, a []uint64) int64 {
	return p.atPath(a[0], a[1], func(path string) int64 { return p.accessFor(path, a[2], a[3]&0x20 == 0) })
}

func sysChdir(p *Process, a []uint64) int64 { return p.changeDirectory(p.cstr(a[0])) }

// changeDirectory is chdir, of a process or of one about to start.
func (p *Process) changeDirectory(path string) int64 {
	dir, errno := p.directory(path)
	if errno == 0 {
		p.cwd = dir
	}
	return errno
}

// directory is the directory a path names, for something to be in.
func (p *Process) directory(path string) (string, int64) {
	t, info, errno := p.describePath("chdir", path, true)
	if errno != 0 {
		return "", errno
	}
	if info.mode&syscall.S_IFMT != syscall.S_IFDIR {
		return "", -eNOTDIR
	}
	if !p.permits(info, 1) { // to be in a directory is to search it, as measured
		return "", -eACCES
	}
	return t.path, 0
}

// workingDirectory is the directory the calling thread is in: the process's, unless the thread
// has one of its own.
func (p *Process) workingDirectory() string {
	if t := p.current; t != nil && t.cwd != "" {
		return t.cwd
	}
	return p.cwd
}

// sysPthreadChdir gives the calling thread a working directory of its own, and sysPthreadFchdir
// one by a descriptor, or with -1 takes it away again: how a thread starts a program in a
// directory without the others finding themselves there.
func sysPthreadChdir(p *Process, a []uint64) int64 {
	dir, errno := p.directory(p.cstr(a[0]))
	if errno == 0 {
		p.current.cwd = dir
	}
	return errno
}

func sysPthreadFchdir(p *Process, a []uint64) int64 {
	if int32(a[0]) == -1 {
		p.current.cwd = ""
		return 0
	}
	dir, errno := p.enterable(a[0])
	if errno == 0 {
		p.current.cwd = dir
	}
	return errno
}

func sysFchdir(p *Process, a []uint64) int64 {
	dir, errno := p.enterable(a[0])
	if errno == 0 {
		p.cwd = dir
	}
	return errno
}

// enterable is the directory a descriptor is of, for a process or a thread to be in: one that
// may be searched now, whatever it was when the descriptor was opened, as measured.
func (p *Process) enterable(n uint64) (string, int64) {
	dir, errno := p.descriptorDirectory(n)
	if errno != 0 {
		return "", errno
	}
	if info, errno := p.describeFD(p.fds[n]); errno == 0 && !p.permits(info, 1) {
		return "", -eACCES
	}
	return dir, 0
}

// descriptorDirectory is the directory a descriptor is of.
func (p *Process) descriptorDirectory(n uint64) (string, int64) {
	f := p.fds[n]
	if f == nil {
		return "", -eBADF
	}
	if f.path == "" {
		return "", -eNOTDIR
	}
	info, errno := p.describeFD(f)
	if errno != 0 {
		return "", errno
	}
	if info.mode&syscall.S_IFMT != syscall.S_IFDIR {
		return "", -eNOTDIR
	}
	return f.path, 0
}

// writableTarget resolves a guest path to a writable target under the home or a mount.
func (p *Process) writableTarget(path string) (target, bool) {
	t, _ := p.resolve(path)
	return t, t.root != nil
}

// hostCall reports the result of a host file operation the way a system call does.
func hostCall(err error) int64 {
	if err != nil {
		return hostErrno(err)
	}
	return 0
}

func (p *Process) doUnlink(path string, dir bool) int64 {
	p.note("  unlink %s", path)
	if asDirectory(path) && !dir { // of what a link leads to: a directory, which is not unlinked, as measured
		if _, _, errno := p.describePath("unlink", path, true); errno != 0 {
			return errno
		}
		return -ePERM
	}
	if dir && (path == ".." || strings.HasSuffix(path, "/..")) { // a directory is not removed by the name of what is above another, as measured
		if _, _, errno := p.describePath("rmdir", path, true); errno != 0 {
			return errno
		}
		return -eINVAL
	}
	t, ok := p.writableTarget(path)
	if !ok {
		if errno := p.unwritable(path, false); errno != 0 {
			return errno
		}
		return -eNOENT
	}
	if asDirectory(path) {
		if strings.HasSuffix(path, ".") { // rmdir of a directory by its own name in it, as measured
			return -eINVAL
		}
		if fi, err := t.root.Lstat(t.rel); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			// A link written as a directory: the directory it leads to is removed and the
			// link is left, as measured.
			if _, _, errno := p.describePath("rmdir", path, true); errno != 0 {
				return errno
			}
			to, err := t.root.Readlink(t.rel)
			if err != nil {
				return hostErrno(err)
			}
			if !strings.HasPrefix(to, "/") {
				to = pathpkg.Join(pathpkg.Dir(p.absolute(path)), to)
			}
			if t, ok = p.writableTarget(to); !ok {
				return -eNOENT
			}
		}
	}
	fi, err := t.root.Lstat(t.rel)
	if err != nil {
		return hostErrno(err)
	}
	switch {
	case dir && !fi.IsDir():
		return -eNOTDIR
	case !dir && fi.IsDir():
		return -ePERM
	}
	forget := p.forgetting(t)
	if err := t.root.Remove(t.rel); err != nil {
		return hostErrno(err) // and what is kept of it stays: a directory that is not empty keeps its mode
	}
	forget()
	return 0
}

func sysUnlink(p *Process, a []uint64) int64 { return p.doUnlink(p.cstr(a[0]), false) }
func sysRmdir(p *Process, a []uint64) int64  { return p.doUnlink(p.cstr(a[0]), true) }
func sysUnlinkat(p *Process, a []uint64) int64 {
	// As a directory if told so (AT_REMOVEDIR, or AT_REMOVEDIR_DATALESS as rm tells it); a
	// flag there is not is not minded.
	return p.atPath(a[0], a[1], func(path string) int64 { return p.doUnlink(path, a[2]&(0x80|0x100) != 0) })
}

// doSymlink makes a symbolic link at a path, to whatever text it is given.
func (p *Process) doSymlink(text, path string) int64 {
	t, ok := p.writableTarget(path)
	if !ok {
		return p.notMade(path, -eEXIST, true)
	}
	if err := t.root.Symlink(text, t.rel); err != nil {
		return hostErrno(err)
	}
	p.m.changed() // a name that may lead to a directory
	p.created(t)
	return 0
}

func sysSymlink(p *Process, a []uint64) int64 { return p.doSymlink(p.cstr(a[0]), p.cstr(a[1])) }
func sysSymlinkat(p *Process, a []uint64) int64 {
	return p.atPath(a[1], a[2], func(path string) int64 { return p.doSymlink(p.cstr(a[0]), path) })
}

// doLink gives a file a second name. Of a symbolic link it is the link that gets it, unless the
// link is to be followed first.
func (p *Process) doLink(from, to string, follow bool) int64 {
	t1, ok1 := p.writableTarget(from)
	t2, ok2 := p.writableTarget(to)
	if !ok1 || !ok2 {
		// As measured: what carries the flag that it is restricted, the file or the place its
		// new name would be, is said first; then another volume; then the read-only volume.
		switch s, d := p.placeOf(from), p.placeOf(to); {
		case !s.there:
			return -eNOENT
		case s.refusal == -ePERM || d.refusal == -ePERM:
			return -ePERM
		case !d.dirThere:
			return -eNOENT
		case s.system != d.system: // the system volume and another
			return -eXDEV
		case d.refusal != 0:
			return d.refusal
		case d.there:
			return -eEXIST
		}
		return -eACCES
	}
	if !sameVolume(t1, t2) {
		return -eXDEV
	}
	for links := 0; follow; links++ {
		text, err := t1.root.Readlink(t1.rel)
		if err != nil {
			break // no link, or nothing
		}
		if strings.HasPrefix(text, "/") || links == 32 {
			p.unimplemented("a hard link to what a symbolic link with a whole path names", nil)
			return -eNOSYS
		}
		t1.rel = pathpkg.Join(pathpkg.Dir(t1.rel), text)
	}
	return p.linkHost(t1, t2)
}

// sysLink is link(a file, another name for it). What a Mac's kernel does when the file is a
// symbolic link was measured for linkat only.
func sysLink(p *Process, a []uint64) int64 {
	from := p.cstr(a[0])
	if t, ok := p.writableTarget(from); ok {
		if _, err := t.root.Readlink(t.rel); err == nil {
			p.unimplemented("link of a symbolic link", a)
			return -eNOSYS
		}
	}
	return p.doLink(from, p.cstr(a[1]), false)
}

// sysLinkat is linkat(directory, path, directory, path, flags): AT_SYMLINK_FOLLOW has a
// symbolic link followed.
func sysLinkat(p *Process, a []uint64) int64 {
	const followLink = 0x40
	if a[4]&^followLink != 0 {
		p.unimplemented("linkat with flags other than AT_SYMLINK_FOLLOW", a)
		return -eNOSYS
	}
	from, errno := p.at(a[0], a[1])
	if errno != 0 {
		return errno
	}
	return p.atPath(a[2], a[3], func(to string) int64 { return p.doLink(from, to, a[4]&followLink != 0) })
}

func (p *Process) doMkdir(path string, mode uint64) int64 {
	t, ok := p.writableTarget(path)
	if !ok {
		if errno := p.unwritable(path, true); errno != 0 {
			return errno
		}
		return -eACCES
	}
	if err := t.root.Mkdir(t.rel, os.FileMode(mode&0o777)); err != nil {
		return hostErrno(err)
	}
	p.created(t)
	return p.chmodAt(t, os.FileMode(uint32(mode&0o777)&^p.umask()), false)
}

// sysMkfifo is mkfifo(path, mode). The pipe is a name under the home or a mount that the machine
// knows to be one, for the run: the host keeps an empty file for it, so that nothing of the host's
// that opens the name waits on it. It can be seen and removed; opening it stops as unimplemented.
func sysMkfifo(p *Process, a []uint64) int64 {
	t, ok := p.writableTarget(p.cstr(a[0]))
	if !ok {
		return p.notMade(p.cstr(a[0]), -eEXIST, false)
	}
	h, err := openInRoot(t.root, t.rel, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY, 0o600)
	if err != nil {
		return hostErrno(err)
	}
	defer syscall.Close(h)
	var st syscall.Stat_t
	if err := syscall.Fstat(h, &st); err != nil {
		return hostErrno(err)
	}
	p.m.fifos[hostFileOf(&st)] = true
	p.created(t)
	return hostCall(syscall.Fchmod(h, uint32(a[1]&0o777)&^p.umask()))
}

func sysMkdir(p *Process, a []uint64) int64 { return p.doMkdir(p.cstr(a[0]), a[1]) }
func sysMkdirat(p *Process, a []uint64) int64 {
	return p.atPath(a[0], a[1], func(path string) int64 { return p.doMkdir(path, a[2]) })
}

func (p *Process) doRename(from, to string) int64 {
	if asDirectory(to) { // which has to be a directory that is there, as measured
		if _, _, errno := p.describePath("rename", to, true); errno != 0 {
			return errno
		}
	}
	t1, ok1 := p.writableTarget(from)
	t2, ok2 := p.writableTarget(to)
	if !ok1 || !ok2 {
		// As measured: another volume is said before the system volume's refusal, which is
		// that of what is to be renamed.
		switch s, d := p.placeOf(from), p.placeOf(to); {
		case !s.there || !d.dirThere:
			return -eNOENT
		case s.system != d.system: // the system volume and another
			return -eXDEV
		case s.refusal != 0:
			return s.refusal
		case d.refusal != 0:
			return d.refusal
		}
		return -eACCES
	}
	if !sameVolume(t1, t2) {
		return -eXDEV
	}
	if to, err := t2.root.Lstat(t2.rel); err == nil && to.IsDir() { // a directory is not replaced by what is none, as measured
		if from, err := t1.root.Lstat(t1.rel); err == nil && !from.IsDir() {
			return -eISDIR
		}
	}
	p.forgetFile(t2) // what it replaces, if anything
	return p.renameHost(t1, t2)
}

func sysRename(p *Process, a []uint64) int64 { return p.doRename(p.cstr(a[0]), p.cstr(a[1])) }

func sysRenameat(p *Process, a []uint64) int64 {
	from, errno := p.at(a[0], a[1])
	if errno != 0 {
		return errno
	}
	to, errno := p.at(a[2], a[3])
	if errno != 0 {
		return errno
	}
	return p.doRename(from, to)
}

// sysRenameatx is renameat with flags (swap, exclusive), which nothing has needed yet.
func sysRenameatx(p *Process, a []uint64) int64 {
	if a[4] != 0 {
		return -eNOTSUP
	}
	return sysRenameat(p, a)
}

func sysTruncate(p *Process, a []uint64) int64 {
	if errno := p.tooLarge(a[1]); errno != 0 { // before the name is looked at, as measured
		return errno
	}
	t, ok := p.writableTarget(p.cstr(a[0]))
	if !ok {
		if errno := p.unwritable(p.cstr(a[0]), false); errno != 0 { // as measured of the system's SystemVersion.plist
			return errno
		}
		return -eACCES
	}
	f, err := t.root.OpenFile(t.rel, os.O_WRONLY, 0)
	if err != nil {
		return hostErrno(err)
	}
	defer f.Close()
	if mf := p.m.mappedAt(t); mf != nil {
		mf.store(false)
		defer p.m.reload(mf)
	}
	return hostCall(f.Truncate(int64(a[1])))
}

// doReadlink serves the links a guest makes under its home or a mount; nothing else is a symbolic link.
func (p *Process) doReadlink(path string, buf, size uint64) int64 {
	if asDirectory(path) { // of what the link leads to, which is no link
		if _, _, errno := p.describePath("readlink", path, true); errno != 0 {
			return errno
		}
		return -eINVAL
	}
	if to, ok := standardLink(p.absoluteLink(path)); ok {
		n := min(uint64(len(to)), size)
		p.write(buf, []byte(to)[:n])
		return int64(n)
	}
	t, ok := p.resolve(path)
	link := t.link // one of the system volume's
	if root := p.rootLink(path); root != "" {
		t, link = target{}, root
	}
	switch {
	case t.root != nil && link == "": // one a guest made, or the data volume's
		var err error
		if link, err = t.root.Readlink(t.rel); err != nil {
			return hostErrno(err)
		}
		// A link is read by who its own mode lets read it (chmod -h), as measured; it is
		// followed whatever its mode.
		if len(p.m.said) != 0 {
			if _, info, errno := p.describePath("readlink", path, false); errno == 0 && !p.permits(info, 4) {
				return -eACCES
			}
		}
	case !ok && link == "": // nothing is there, as measured of the system volume and of the root
		if _, _, errno := p.describePath("readlink", path, false); errno == 0 {
			return -eINVAL // something that is answered another way (/dev/tty), and is no link
		}
		return -eNOENT
	case link == "": // something that is no link
		return -eINVAL
	}
	n := min(uint64(len(link)), size)
	p.write(buf, []byte(link)[:n])
	return int64(n)
}

func sysReadlink(p *Process, a []uint64) int64 { return p.doReadlink(p.cstr(a[0]), a[1], a[2]) }
func sysReadlinkat(p *Process, a []uint64) int64 {
	return p.atPath(a[0], a[1], func(path string) int64 { return p.doReadlink(path, a[2], a[3]) })
}

// list reads the names in an open directory, once: directories under the home or a mount, and
// allowed directories on the build's system volume.
func (p *Process) list(f *fd) int64 {
	if f.path != "" {
		p.from(f.path) // what is in it is looked for from it, whatever leads to it now (see blockedAt)
	}
	if f.names != nil {
		return 0
	}
	if f.path == devFDs {
		// The process's descriptors by their numbers, this one among them, and no names for
		// the directory itself or the one above: as measured.
		numbers := slices.Sorted(maps.Keys(p.fds))
		f.names = make([]string, 0, len(numbers))
		for _, n := range numbers {
			if p.fds[n] != nil {
				f.names = append(f.names, strconv.FormatUint(n, 10))
			}
		}
		return 0
	}
	t := target{kind: f.kind, path: f.path, readDir: f.readDir}
	if f.kind == hostFD {
		t, _ = p.writableTarget(f.path)
	}
	names, kinds, ok := p.dirNames(t)
	if !ok {
		return -eNOTDIR
	}
	f.kinds = kinds
	for _, m := range p.mounts { // what is on the way to a mount is there, though the volume has no such name
		if dir := pathpkg.Clean(f.path); f.kind == dirFD && m.guest != dir && under(m.guest, dir) {
			name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(m.guest, dir), "/"), "/")
			if !slices.Contains(names, name) {
				names = append(names, name)
				slices.Sort(names)
			}
		}
	}
	f.names = append([]string{".", ".."}, names...)
	if pathpkg.Clean(f.path) == "/dev" {
		// The devices that are answered here, of the hundreds a Mac has, and the terminals.
		for _, name := range append([]string{"fd", "null", "random", "stderr", "stdin", "stdout", "tty", "urandom", "zero"}, p.terminalNames()...) {
			if !slices.Contains(f.names, name) {
				f.names = append(f.names, name)
			}
		}
	}
	return 0
}

// dirNames reads the names in a directory target, and says whether it is one that can be listed.
func (p *Process) dirNames(t target) ([]string, map[string]byte, bool) {
	switch {
	case t.root != nil:
		names, kinds, err := readDirInRoot(t.root, t.rel, p.m.said)
		return names, kinds, err == nil
	case t.kind == dirFD:
		if t.readDir == nil {
			return nil, nil, true
		}
		names, err := away(p.m.clock, t.readDir)
		if err != nil {
			stopf("readdir %s: %v", t.path, err)
		}
		return names, nil, true
	}
	return nil, nil, false
}

// sysGetdirentries64 lists a directory under the home, a mount, or an allowed system directory.
// A buffer of 1024 bytes or more ends in flags, where 1 says the listing is complete.
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
		if f.path == devFDs {
			kind = 0 // of no kind that is said (DT_UNKNOWN), as measured
		} else if _, link := standardLink(child); link {
			kind = syscall.DT_LNK
		} else if f.pos >= 2 && pathpkg.Clean(f.path) == "/dev" && name == "fd" {
			kind = 0 // as measured
		} else if f.pos >= 2 && pathpkg.Clean(f.path) == "/dev" {
			kind = syscall.DT_CHR
		} else if f.pos >= 2 && p.terminalNamed(child) != nil {
			kind = syscall.DT_CHR
		} else if f.pos >= 2 {
			if t, ok := p.resolve(child); ok {
				switch {
				case t.root != nil:
					var st syscall.Stat_t
					if statInRoot(t.root, t.rel, false, &st) == nil {
						kind = byte(st.Mode >> 12) // DT_* are the file type bits of a mode
						if p.m.fifos[hostFileOf(&st)] {
							kind = syscall.DT_FIFO
						}
						if p.m.sockets[hostFileOf(&st)] != nil {
							kind = syscall.DT_SOCK
						}
					} else if k := f.kinds[name]; k != 0 { // a host that does not say a kind says none // not to be looked at: the directory may not be searched
						kind = k
					}
				case t.kind == cacheFD || t.kind == dataFD:
					kind = byte(syscall.DT_REG)
				}
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

// dupFD duplicates oldfd to target (if exact) or to the lowest unused number >= minFD.
func (p *Process) dupFD(oldfd, minFD uint64, exact bool) int64 {
	f := p.fds[oldfd]
	if f == nil {
		return -eBADF
	}
	if int64(minFD) < 0 || minFD >= p.fdLimit() {
		if exact {
			return -eBADF
		}
		return -eINVAL
	}
	if exact && oldfd == minFD {
		return int64(minFD)
	}
	dup, err := f.duplicate()
	if err != nil {
		return hostErrno(err)
	}
	n := minFD
	if exact {
		p.closeFD(n) // if open
	} else if n = p.allocFD(minFD); n >= p.fdLimit() {
		if dup.kind == hostFD {
			p.hostClose(dup)
		}
		if dup == f {
			dup.refs--
		}
		return -eMFILE
	}
	p.fds[n] = dup
	p.holds(n)
	delete(p.cloexec, n) // a duplicate is the child's too, whatever the original is
	delete(p.clofork, n)
	return int64(n)
}

// duplicate is one more descriptor for what a descriptor is of, to be put in a table or sent to
// another process: it shares the open file, and its place in it, as a real duplicate does.
func (f *fd) duplicate() (*fd, error) {
	switch {
	case f.kind == hostFD:
		return f.hostDup()
	case f.kind == stdioFD:
		return &fd{kind: stdioFD, host: f.host, path: f.path, state: f.status()}, nil
	case f.shared != nil:
		return &fd{kind: f.kind, shared: f.shared.retain(), path: f.path, pos: f.pos, flags: f.flags, state: f.status()}, nil
	}
	f.refs++
	return f, nil
}

func sysFcntl(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	if f.streams() || f.kind == ttyFD {
		if r, ok := p.pipeFcntl(f, a[1], a[2]); ok {
			return r
		}
	}
	switch cmd, arg := a[1], a[2]; cmd {
	case 50: // F_GETPATH
		path := devFDs
		if f.path != "" {
			path = f.path
		}
		p.write(arg, str(path))
	case 0, 67: // F_DUPFD, F_DUPFD_CLOEXEC
		n := p.dupFD(a[0], arg, false)
		if n >= 0 && cmd == 67 {
			p.cloexec[uint64(n)] = true
		}
		return n
	case 1: // F_GETFD: close-on-exec is 1 and close-on-fork 2, as measured
		return int64(truth(p.cloexec[a[0]])) + 2*int64(truth(p.clofork[a[0]]))
	case 2: // F_SETFD
		if arg&fdCloexec != 0 {
			p.cloexec[a[0]] = true
		} else {
			delete(p.cloexec, a[0])
		}
		p.noFork(a[0], arg&2 != 0)
		return 0
	case 3: // F_GETFL
		return f.fileFlags()
	case 4: // F_SETFL
		return p.setFileFlags(f, arg)
	case 97: // F_ADDFILESIGS_RETURN: the signature covers the file up to the blob
		p.write(arg, p.read(arg+8, 8))
	case 105: // F_GETSIGSINFO: files of the build are platform binaries, the guest's own are not
		p.write(arg+12, le32(uint32(truth(f.kind != hostFD))))
	case 92, 7: // F_OFD_GETLK, F_GETLK
		return p.getRange(f, arg, cmd == 7, a)
	case 90, 91, 8, 9: // F_OFD_SETLK, F_OFD_SETLKW, F_SETLK, F_SETLKW
		return p.setRange(f, arg, cmd == 91 || cmd == 9, cmd == 8 || cmd == 9, a)
	case 42: // F_PREALLOCATE
		return p.preallocate(f, arg, a)
	case 45, 48, 51, 85: // F_RDAHEAD, F_NOCACHE, F_FULLFSYNC, F_BARRIERFSYNC
		return p.fcntlCaching(f, cmd, arg, a)
	case 44: // F_RDADVISE
		return p.readAdvise(f, arg, a)
	case 63, 95, 96, 98, 101:
		return p.fcntlMeasured(f, cmd, arg, a)
	default:
		p.unimplemented(fmt.Sprintf("fcntl request %d", cmd), a)
		return -eINVAL
	}
	return 0
}

// plain is whether a descriptor is of a file and nothing else: one of the host's or of the
// build, and no directory, pipe, socket, kqueue or shared memory.
func (f *fd) plain() bool {
	return f.kind == hostFD || f.kind == cacheFD || f.kind == dataFD && f.kq == nil && f.shared == nil && f.necp == nil
}

// fcntlCaching answers what is asked of how a file is read and written out, which a shell asks
// of each script it reads and a database of each file it writes, as measured (tests/kernel/files/fileflags.c):
// not keeping it in memory and not reading ahead of it, which here are only said back by F_GETFL,
// there being no such memory to keep it in; and writing it out to the disk, which is the host's
// to do for a file of its own. A pipe is EBADF.
func (p *Process) fcntlCaching(f *fd, cmd, arg uint64, a []uint64) int64 {
	if f.streams() {
		return -eBADF
	}
	if !f.plain() && f.kind != dirFD {
		p.unimplemented(fmt.Sprintf("fcntl request %d of this kind of descriptor", cmd), a)
		return -eINVAL
	}
	flag, set := uint32(flagNoCache), arg != 0
	switch cmd {
	case 45: // read ahead, unless told 0
		flag, set = flagNoAhead, arg == 0
	case 51, 85:
		return sysFsync(p, a)
	}
	if s := f.status(); set {
		s.flags |= flag
	} else {
		s.flags &^= flag
	}
	return 0
}

// readAdvise is F_RDADVISE, by which a program says what part of a file it is about to read
// ({from where, how much}), as measured (tests/kernel/files/fileflags.c): nothing is read ahead for it
// here, and it is told what a Mac tells it. A part that starts at or past the file's end is
// EFBIG, one from before its start or of less than nothing EINVAL, as is a directory; a pipe is
// EBADF. Of a file that holds nothing it was not measured, and stops.
func (p *Process) readAdvise(f *fd, arg uint64, a []uint64) int64 {
	if f.streams() {
		return -eBADF
	}
	// What is asked is read before the file is looked at, by the source: so nowhere to read it
	// from is EFAULT and a part of less than nothing EINVAL whatever the file (not measured of
	// a directory, which is EINVAL with a part that is right).
	advice := p.read(arg, 16)
	from, count := int64(le.Uint64(advice)), int32(le.Uint32(advice[8:]))
	switch {
	case from < 0 || count < 0:
		return -eINVAL
	case f.kind == dirFD || f.kind == hostFD && p.isDirectory(f):
		return -eINVAL
	case !f.plain():
		p.unimplemented("fcntl request 44 of this kind of descriptor", a)
		return -eINVAL
	}
	size := f.size()
	if f.kind == hostFD {
		var st syscall.Stat_t
		if err := syscall.Fstat(f.host, &st); err != nil {
			return hostErrno(err)
		}
		size = st.Size
	}
	switch {
	case size == 0:
		p.unimplemented("fcntl request 44 of a file that holds nothing", a)
		return -eNOSYS
	case from >= size:
		return -eFBIG
	}
	return 0
}

// preallocate is F_PREALLOCATE, by which cp and the like ask for room for what they are about to
// write, past the end of a file (tests/kernel/files/fileflags.c). The file is no longer for it, and the
// room is said to be had, all that was asked: none is kept for it on the host, so what stat says
// the file takes on the disk does not grow as a Mac's does. Asked from anywhere but the end of
// the file, or for less than nothing, it is EINVAL; of what is not a file open to write, EBADF.
func (p *Process) preallocate(f *fd, arg uint64, a []uint64) int64 {
	const fromEnd = 3        // F_PEOFPOSMODE
	asked := p.read(arg, 32) // how, from where (a mode and an offset), how much, and how much was had
	mode, offset, length := le.Uint32(asked[4:]), int64(le.Uint64(asked[8:])), int64(le.Uint64(asked[16:]))
	switch {
	case f.streams() || f.kind == dirFD || f.kind == hostFD && f.accessMode() == 0:
		return -eBADF
	case f.kind != hostFD:
		p.unimplemented("room asked ahead (F_PREALLOCATE) for what is no file of the home's or a mount's", a)
		return -eNOSYS
	case mode != fromEnd || offset != 0:
		return -eINVAL
	case length < 0:
		p.write(arg+24, le64(0))
		return -eINVAL
	}
	p.write(arg+24, le64(uint64(length)))
	return 0
}

// fcntlMeasured answers the requests SQLite and dyld make of every file they open, as a Mac's
// kernel of macOS 27.2.1 was measured to answer a user's process (tests/kernel/locks/rangelocks.c). What
// was not measured, of a request or of a kind of descriptor, stops.
func (p *Process) fcntlMeasured(f *fd, cmd, arg uint64, a []uint64) int64 {
	file := f.plain()
	switch cmd {
	case 63: // F_GETPROTECTIONCLASS: how it is protected. An iPhone's files have this class unless given another, as Apple documents, which was not measured.
		switch {
		case f.streams():
			return -eBADF
		case file || f.kind == dirFD:
			return 3
		}
	case 95: // F_SETCONFINED: keeping it from being moved out
		switch {
		case arg != 0: // of a file, a directory and a pipe alike
			return -eBADF
		case file:
			return -ePERM
		}
	case 96: // F_GETCONFINED
		if file {
			return 0
		}
	case 98: // F_CHECK_LV: whether a library is signed
		if !f.streams() {
			return p.checkLibrary(f, arg)
		}
	case 101: // F_SPECULATIVE_READ: a part it will read soon, which there is nothing to do about here
		hint := p.read(arg, 24) // flags, a word kept back, where from and how much
		switch {
		case f.streams():
			return -eBADF
		case f.kind == dirFD, int64(le.Uint64(hint[8:])) < 0, int64(le.Uint64(hint[16:])) < 0:
			return -eINVAL
		case file:
			return 0
		}
	}
	p.unimplemented(fmt.Sprintf("fcntl request %d of this kind of descriptor", cmd), a)
	return -eINVAL
}

func sysDup(p *Process, a []uint64) int64  { return p.dupFD(a[0], 0, false) }
func sysDup2(p *Process, a []uint64) int64 { return p.dupFD(a[0], a[1], true) }

// volume is a mounted file system. There are two, as on a real system, because callers that list
// the mounts (CarbonCore) insist that each has its own device id.
type volume struct {
	dev           uint32 // what stat says of its files
	mount, device string
	name          string // as a newly installed Mac calls it
	uuid          string // 16 bytes that are this machine's own for it, and no other volume's
	// What statfs says besides: the volume's own id, and how a Mac has it mounted (measured on
	// macOS 27.2.1; not known for iOS, and left out there).
	fsid, flags uint32
}

const (
	preboot    = "/System/Volumes/Preboot"
	dataMount  = "/System/Volumes/Data"
	systemFSID = deviceID + 2 // the system volume's own id, to statfs
)

// volumes are the mounts, the root last. A Mac's system volume and its data volume are one
// device to stat and getattrlist, which is the data volume's, and two volumes to statfs: the
// system's has an id of its own there, and either id finds a file of either (fsgetpath).
var (
	volumes = []volume{
		{dev: deviceID + 1, mount: preboot, device: "/dev/disk1s2", name: "Preboot", uuid: "ipswrun preboot\x00", fsid: deviceID + 1, flags: 0x4909000},
		{dev: deviceID, mount: "/", device: "/dev/disk1", name: "Macintosh HD", uuid: "ipswrun volume \x00", fsid: systemFSID, flags: 0x4480d001},
	}
	// dataFiles is the volume the home and the paths of the data volume are on.
	dataFiles = volume{dev: deviceID, mount: dataMount, device: "/dev/disk1s1", name: "Data", uuid: "ipswrun data   \x00", fsid: deviceID, flags: 0x4909080}
)

// volumeOf is the volume a canonical guest path is on, by where it is.
func volumeOf(path string) volume {
	for _, v := range volumes {
		if under(path, v.mount) {
			return v
		}
	}
	return volumes[len(volumes)-1]
}

// volumeAt is the volume statfs and the volume's attributes name for a path: on a Mac the data
// volume for the home and the paths of the data volume, and otherwise where it is. How an
// iPhone has its volumes mounted is not known: there it is where the path is, with no flags.
func (p *Process) volumeAt(path string) volume {
	if p.cache.Platform == dyldcache.IOS {
		v := volumeOf(path)
		v.flags = 0
		return v
	}
	for _, m := range p.mounts {
		switch {
		case !under(path, m.guest):
		case m.dataOn != 0:
			return dataFiles
		default: // a mount of the run's own, which is no volume of a Mac's: the root's, and how it is mounted not said
			v := volumeOf(path)
			v.flags = 0
			return v
		}
	}
	return volumeOf(path)
}

func (p *Process) statfs(path string, buf uint64) int64 {
	p.write(buf, p.volumeAt(path).statfs())
	return 0
}

const statfsSize = 2168

// statfs is the struct statfs of a volume.
func (v volume) statfs() []byte {
	b := make([]byte, statfsSize)
	le.PutUint32(b[0:], volumeBlockSize)  // f_bsize
	le.PutUint32(b[4:], 1<<20)            // f_iosize
	le.PutUint64(b[8:], volumeBlocks)     // f_blocks
	le.PutUint64(b[16:], volumeFree)      // f_bfree
	le.PutUint64(b[24:], volumeFree)      // f_bavail
	le.PutUint64(b[32:], volumeObjects)   // f_files
	le.PutUint64(b[40:], volumeObjects/2) // f_ffree
	le.PutUint32(b[48:], v.fsid)
	le.PutUint32(b[52:], fsTypeAPFS)
	le.PutUint32(b[64:], v.flags) // f_flags
	copy(b[72:], "apfs")
	copy(b[88:], v.mount)
	copy(b[1112:], v.device)
	return b
}

// sysGetfsstat64 is getfsstat(buffer, its size, flags): the volumes that are mounted, the root
// first, as many as there is room for, or with no buffer how many there are. They are the three
// of a Mac's that are here: a Mac has more (/dev, and the volumes it keeps for itself), which
// are left out. Of an iPhone's only the root is known to be, and is all that is listed, as
// statfs says it there (its dyld asks as it starts).
func sysGetfsstat64(p *Process, a []uint64) int64 {
	list := []volume{p.volumeAt("/")}
	if p.cache.Platform != dyldcache.IOS {
		list = []volume{volumeOf("/"), volumeOf(preboot), dataFiles}
	}
	if a[0] == 0 {
		return int64(len(list))
	}
	n := min(uint64(len(list)), a[1]/statfsSize)
	for i := range n {
		p.write(a[0]+i*statfsSize, list[i].statfs())
	}
	return int64(n)
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

// saw notes where a file of a number is, for fsgetpath: every file a guest has looked at, which
// is the machine's to know and so any process's to ask, as a Mac's kernel finds a file by its
// number for whoever asks (tests/kernel/proc/spawngroup.c). One no guest has looked at is not found here.
func (m *Machine) saw(id fileID, path string) {
	if m.seen == nil {
		m.seen = map[fileID]string{}
	}
	m.seen[id] = path
}

// sysFsgetpath turns a volume and an inode number back into a path: of a volume's root, or of a
// file a guest has looked at before. Every process's own program has one number here, so that
// number is the asker's own program, whoever looked last.
func sysFsgetpath(p *Process, a []uint64) int64 {
	id, root := le.Uint32(p.read(a[2], 8)), ""
	for _, v := range volumes { // a volume by its own id or its device's: its files are known by the device's
		if id == v.fsid || id == v.dev {
			id, root = v.dev, v.mount
		}
	}
	path, ok := p.m.seen[fileID{id, a[3]}]
	switch {
	case a[3] == rootInode && root != "": // which is there whoever has looked
		path, ok = root, true
	case ok && a[3] == exeInode:
		path = p.path
	}
	if !ok || uint64(len(path)) >= a[1] {
		return -eNOENT
	}
	p.write(a[0], str(path))
	return int64(len(path) + 1)
}
