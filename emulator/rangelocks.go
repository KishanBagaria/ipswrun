package emulator

import (
	"cmp"
	"io"
	"slices"
	"syscall"
	"time"

	"github.com/blacktop/go-macho"
)

// Locks on parts of a file (fcntl): an open file's (F_OFD_SETLK and its kin, which SQLite takes
// on every database it opens) and a process's own (F_SETLK and its kin, which most other
// programs take), as a Mac's kernel was measured to keep them (tests/kernel/locks/rangelocks.c,
// tests/kernel/locks/proclocks.c) and as its source has them (bsd/kern/kern_lockf.c). An open file's is
// held by the open file, so a duplicate and a copy of the process hold it too; a process's is
// held by the process whatever descriptor took it, is no copy's, and goes when the process
// closes any descriptor of the file or ends. Whoever else asks for what conflicts with one, an
// open file or a process, in the same process or not, is refused (EAGAIN) or waits: any lock
// where one of the two is to write. Locking again what it holds changes the kind; unlocking
// the middle of one leaves the two ends; neighbours of one kind are one lock; unlocking what
// is not locked is no error. They are the machine's and not the host's, so they are the same
// on every host.

// lockOwner is who holds a lock: an open file, or a process by its number, which the program a
// process becomes keeps.
type lockOwner struct {
	file  *status
	pid   uint32
	whole bool // an open file's lock on the whole file (flock), which is not the holder its locks on parts are
}

type rangeLock struct {
	owner    lockOwner
	from, to uint64 // to is past its last byte; all ones is to the end of the file, however long it gets
	write    bool
}

// rangeWait is a lock a thread waits to be given.
type rangeWait struct {
	id       hostFile
	owner    lockOwner
	from, to uint64
	write    bool
	// Whose lock it waits behind and since when among those that do, or the waiter it was put
	// behind when that one alone was woken; and that it has been woken to look again.
	on      lockOwner
	seq     uint64
	follows *rangeWait
	woken   bool
}

const (
	lockRead, lockNone, lockWrite = 1, 2, 3 // F_RDLCK, F_UNLCK, F_WRLCK
	toTheEnd                      = ^uint64(0)
	flockSize                     = 24 // struct flock: start, length, pid, type, whence
	eDEADLK                       = 11
)

// rangeAsked is the part of a file a struct flock names and the kind of lock, for a descriptor
// that may hold one: a file of the host's.
func (p *Process) rangeAsked(f *fd, arg uint64, a []uint64) (id hostFile, from, to uint64, kind int16, errno int64) {
	if f.streams() {
		return id, 0, 0, 0, -eBADF
	}
	if f.kind != hostFD {
		p.unimplemented("a lock on part of what is no file of the home's or a mount's", a)
		return id, 0, 0, 0, -eNOSYS
	}
	raw := p.read(arg, flockSize)
	start, length, whence := int64(le.Uint64(raw)), int64(le.Uint64(raw[8:])), int16(le.Uint16(raw[22:]))
	kind = int16(le.Uint16(raw[20:]))
	var st syscall.Stat_t
	if err := syscall.Fstat(f.host, &st); err != nil {
		return id, 0, 0, 0, hostErrno(err)
	}
	switch whence {
	case 0:
	case 1:
		at, err := syscall.Seek(f.host, 0, 1)
		if err != nil {
			return id, 0, 0, 0, hostErrno(err)
		}
		start += at
	case 2:
		start += st.Size
	default:
		return id, 0, 0, 0, -eINVAL
	}
	if length < 0 { // the part before where it starts, as the source has it and as measured to be taken
		start, length = start+length, -length
	}
	if start >= 0 && length > 0 && start+length-1 < start { // past the last byte there can be, as measured
		return id, 0, 0, 0, -eOVERFLOW
	}
	if kind != lockRead && kind != lockNone && kind != lockWrite || start < 0 {
		return id, 0, 0, 0, -eINVAL
	}
	if to = uint64(start + length); length == 0 {
		to = toTheEnd
	}
	return hostFileOf(&st), uint64(start), to, kind, 0
}

// against is the lock of another that one asked for would conflict with: the first of them
// as the kernel keeps them, each holder's together in the order the holders came and from the
// start of the file on, as measured (the first to lock is found, not the lowest in the file).
func (m *Machine) against(id hostFile, owner lockOwner, from, to uint64, write bool) *rangeLock {
	for i, l := range m.ranges[id] {
		if l.owner != owner && l.from < to && from < l.to && (write || l.write) {
			return &m.ranges[id][i]
		}
	}
	return nil
}

// lockOwnerOf is who a lock asked for through a descriptor would be held by.
func (p *Process) lockOwnerOf(f *fd, ofProcess bool) lockOwner {
	if ofProcess {
		return lockOwner{pid: p.pid}
	}
	return lockOwner{file: f.status()}
}

// setRange is F_OFD_SETLK and F_OFD_SETLKW, and F_SETLK and F_SETLKW for a process's own.
// One that waits for what another holds waits until it is given up, or is ended by a signal
// (EINTR); a process that would wait for one that waits for it, straight or round by others,
// is told so instead (EDEADLK), as measured of two, which the source looks for among
// processes' locks only.
func (p *Process) setRange(f *fd, arg uint64, wait, ofProcess bool, a []uint64) int64 {
	id, from, to, kind, errno := p.rangeAsked(f, arg, a)
	if errno != 0 {
		return errno
	}
	if mode := f.accessMode(); kind == lockWrite && mode == 0 || kind == lockRead && mode == 1 {
		return -eBADF // it was not opened for what the lock is for
	}
	m, owner := p.m, p.lockOwnerOf(f, ofProcess)
	if kind != lockNone {
		if held := m.against(id, owner, from, to, kind == lockWrite); held != nil {
			if !wait {
				return -eAGAIN
			}
			if ofProcess && m.waitsFor(held.owner, p.pid) {
				return -eDEADLK
			}
			p.current.rangeWait = &rangeWait{id: id, owner: owner, from: from, to: to, write: kind == lockWrite}
			m.queue(p.current.rangeWait, held.owner)
			p.wait(waitRange, 0, time.Time{}, result{})
			return 0
		}
	}
	m.takeRange(id, owner, from, to, kind)
	m.retryRanges()
	return 0
}

// waitsFor says whether the holder of a lock is a process one of whose threads waits for a
// lock of its own that the process pid holds: as far as the kernel's source looks, which is
// one step and among processes' own locks.
func (m *Machine) waitsFor(holder lockOwner, pid uint32) bool {
	if holder.file != nil {
		return false
	}
	for _, q := range m.procs {
		if q.pid != holder.pid || q.exited {
			continue
		}
		for _, t := range q.threads {
			if w := t.rangeWait; w != nil && w.owner.file == nil && t.state == waiting && t.kind == waitRange {
				if held := m.against(w.id, w.owner, w.from, w.to, w.write); held != nil && held.owner.file == nil && held.owner.pid == pid {
					return true
				}
			}
		}
	}
	return false
}

// takeRange gives an owner a lock on part of a file, or with no kind takes it away: what the
// owner held of that part is given up, the rest of each of its locks kept, and what then lies
// side by side or over one another of one kind is one lock.
func (m *Machine) takeRange(id hostFile, owner lockOwner, from, to uint64, kind int16) {
	var kept []rangeLock
	touched := false
	defer func() {
		if touched { // those that waited behind what was changed look again
			m.wakeBehind(id, owner, kind != lockNone)
		}
	}()
	for _, l := range m.ranges[id] {
		if l.owner != owner || l.to <= from || to <= l.from {
			kept = append(kept, l)
			continue
		}
		touched = true
		if l.from < from {
			kept = append(kept, rangeLock{owner, l.from, from, l.write})
		}
		if to < l.to {
			kept = append(kept, rangeLock{owner, to, l.to, l.write})
		}
	}
	if kind != lockNone {
		mine := rangeLock{owner, from, to, kind == lockWrite}
		kept = slices.DeleteFunc(kept, func(l rangeLock) bool {
			if l.owner != owner || l.write != mine.write || l.to < mine.from || mine.to < l.from {
				return false
			}
			mine.from, mine.to = min(mine.from, l.from), max(mine.to, l.to)
			return true
		})
		kept = append(kept, mine)
	}
	// Each holder's together, the holders in the order they came, and from the start on.
	rank := map[lockOwner]int{}
	for _, l := range m.ranges[id] {
		if _, ok := rank[l.owner]; !ok {
			rank[l.owner] = len(rank)
		}
	}
	if _, ok := rank[owner]; !ok {
		rank[owner] = len(rank)
	}
	slices.SortStableFunc(kept, func(a, b rangeLock) int {
		if rank[a.owner] != rank[b.owner] {
			return rank[a.owner] - rank[b.owner]
		}
		return cmp.Compare(a.from, b.from)
	})
	if m.ranges == nil {
		m.ranges = map[hostFile][]rangeLock{}
	}
	if m.ranges[id] = kept; len(kept) == 0 {
		delete(m.ranges, id)
	}
}

// queue puts a waiter behind a holder's lock, last of those there unless it was there already.
func (m *Machine) queue(w *rangeWait, on lockOwner) {
	if w.seq == 0 || w.on != on || w.follows != nil {
		m.lockSeq++
		w.seq = m.lockSeq
	}
	w.on, w.follows, w.woken = on, nil, false
}

// rangeWaiters is the threads that wait for a lock, in the order they came to wait where they do.
func (m *Machine) rangeWaiters() []*thread {
	var held []*thread
	for _, q := range m.procs {
		if q.exited {
			continue
		}
		for _, t := range q.threads {
			if t.state == waiting && t.kind == waitRange && t.rangeWait != nil {
				held = append(held, t)
			}
		}
	}
	slices.SortFunc(held, func(a, b *thread) int { return cmp.Compare(a.rangeWait.seq, b.rangeWait.seq) })
	return held
}

// wakeBehind has those that wait behind a holder's lock on a file look again. When a lock on
// the whole file is given up and only such locks wait behind it, the first alone is woken and
// the rest are put behind that one, to wait until it has had the lock and given it up, even
// those that could share it with it: as the source has it (F_WAKE1_SAFE) and as measured, of
// two that waited to share a file and got it one after the other. Behind a lock that is
// changed and not given up, everyone is woken.
func (m *Machine) wakeBehind(id hostFile, owner lockOwner, all bool) {
	var group []*rangeWait
	one := !all && owner.whole
	for _, t := range m.rangeWaiters() {
		if w := t.rangeWait; w.id == id && w.follows == nil && w.on == owner {
			group = append(group, w)
			one = one && w.owner.whole
		}
	}
	for i, w := range group {
		if one && i > 0 {
			w.follows = group[0]
		} else {
			w.woken = true
		}
	}
}

// retryRanges has the waiters that were woken look again, the longest waiting first: one that
// nothing stands against any more is given its lock, and those put behind it wait behind that
// lock now; one that something stands against waits behind that, and those put behind it with
// it. Those behind one that waits no more (a signal ended it) are woken.
func (m *Machine) retryRanges() {
	for {
		held := m.rangeWaiters()
		waits := map[*rangeWait]bool{}
		for _, t := range held {
			waits[t.rangeWait] = true
		}
		var next *thread
		for _, t := range held {
			w := t.rangeWait
			if w.follows != nil && !waits[w.follows] {
				w.follows, w.woken = nil, true
			}
			if w.woken && next == nil {
				next = t
			}
		}
		if next == nil {
			break
		}
		w := next.rangeWait
		var behind []*rangeWait
		for _, t := range held {
			if t.rangeWait.follows == w {
				behind = append(behind, t.rangeWait)
			}
		}
		if at := m.against(w.id, w.owner, w.from, w.to, w.write); at != nil {
			on := at.owner
			m.queue(w, on)
			for _, b := range behind {
				m.queue(b, on)
			}
			continue
		}
		kind := int16(lockRead)
		if w.write {
			kind = lockWrite
		}
		m.takeRange(w.id, w.owner, w.from, w.to, kind)
		for _, b := range behind {
			m.queue(b, w.owner)
		}
		next.rangeWait, next.answer = nil, &result{}
		next.wake()
	}
	// One that still waits looks again at whom it waits for, as the source has it do each time
	// it is woken: it may be one that now waits for it.
	for _, t := range m.rangeWaiters() {
		if w := t.rangeWait; w.owner.file == nil {
			if at := m.against(w.id, w.owner, w.from, w.to, w.write); at != nil && m.waitsFor(at.owner, w.owner.pid) {
				t.rangeWait, t.answer = nil, &result{x0: eDEADLK, carry: true}
				t.wake()
			}
		}
	}
}

// unlockProcess gives up every lock a process holds of its own, when it ends.
func (m *Machine) unlockProcess(pid uint32) {
	changed := false
	for id, locks := range m.ranges {
		before := len(locks)
		if locks = slices.DeleteFunc(locks, func(l rangeLock) bool { return l.owner.file == nil && l.owner.pid == pid }); len(locks) == 0 {
			delete(m.ranges, id)
		} else {
			m.ranges[id] = locks
		}
		if len(locks) != before {
			changed = true
			m.wakeBehind(id, lockOwner{pid: pid}, false)
		}
	}
	if changed {
		m.retryRanges()
	}
}

// getRange is F_OFD_GETLK and F_GETLK: the lock that the one asked about would conflict with,
// which is said to be of the process that holds it, or of none (-1) for an open file's, or that
// there is none, with the rest of what was asked left as it was.
func (p *Process) getRange(f *fd, arg uint64, ofProcess bool, a []uint64) int64 {
	id, from, to, kind, errno := p.rangeAsked(f, arg, a)
	switch {
	case errno != 0:
		return errno
	case kind == lockNone, !ofProcess && (kind == lockWrite && f.accessMode() == 0 || kind == lockRead && f.accessMode() == 1):
		p.unimplemented("asking what stands against an unlock, or against an open file's lock the file was not opened for", a)
		return -eNOSYS
	}
	l := p.m.against(id, p.lockOwnerOf(f, ofProcess), from, to, kind == lockWrite)
	if l == nil {
		p.write(arg+20, le16(lockNone))
		return 0
	}
	is, length, holder := uint16(lockRead), l.to-l.from, uint32(0xFFFFFFFF)
	if l.write {
		is = lockWrite
	}
	if l.to == toTheEnd {
		length = 0
	}
	if l.owner.file == nil {
		holder = l.owner.pid
	}
	p.write(arg, slices.Concat(le64(l.from, length), le32(holder), le16(is, 0)))
	return 0
}

// unlockOwner gives up every lock an owner holds on a file: a process's when it closes any
// descriptor of the file.
func (m *Machine) unlockOwner(id hostFile, owner lockOwner) {
	before := len(m.ranges[id])
	if before == 0 {
		return
	}
	m.takeRange(id, owner, 0, toTheEnd, lockNone)
	if len(m.ranges[id]) != before {
		m.retryRanges()
	}
}

// unlockRanges gives up every lock of an open file, when its last descriptor is closed.
func (m *Machine) unlockRanges(owner *status) {
	for id, locks := range m.ranges {
		before := len(locks)
		if locks = slices.DeleteFunc(locks, func(l rangeLock) bool { return l.owner.file == owner }); len(locks) == 0 {
			delete(m.ranges, id)
		} else {
			m.ranges[id] = locks
		}
		if len(locks) != before {
			m.wakeBehind(id, lockOwner{file: owner}, false)
			m.wakeBehind(id, lockOwner{file: owner, whole: true}, false)
		}
	}
	m.retryRanges()
}

// checkLibrary is F_CHECK_LV, which dyld asks of a library before it loads it into a program
// that takes only signed ones: 0 for a program with a code signature, from where in the file it
// is said to begin; EPERM, and why, for what has none or is no program there, as measured of a
// file that is none and of the start of one that holds several.
func (p *Process) checkLibrary(f *fd, arg uint64) int64 {
	const unsigned = "Trying to load an unsigned library"
	raw := p.read(arg, 24) // where it begins, room for a message, and the message
	start, room, message := le.Uint64(raw), le.Uint64(raw[8:]), le.Uint64(raw[16:])
	if image, err := macho.NewFile(io.NewSectionReader(descriptorReader{p, f}, int64(start), 1<<62)); err != nil || image.CodeSignature() == nil {
		if message != 0 && room > 0 {
			p.write(message, str(unsigned[:min(uint64(len(unsigned)), room-1)]))
		}
		return -ePERM
	}
	return 0
}

// descriptorReader reads what a descriptor is of, from any place in it.
type descriptorReader struct {
	p *Process
	f *fd
}

func (r descriptorReader) ReadAt(b []byte, at int64) (int, error) {
	data, errno := r.p.pread(r.f, uint64(at), uint64(len(b)))
	n := copy(b, data)
	switch {
	case errno != 0:
		return n, syscall.Errno(-errno)
	case n < len(b):
		return n, io.EOF
	}
	return n, nil
}

// sysFlock is flock(descriptor, how): a lock on the whole file that is the open file's, kept in
// the one list with the locks on parts. It stands against a process's own locks and every
// other open file's, and against the locks on parts of its own open file too, which are another
// holder's: unlocking the whole leaves those, and unlocking those leaves it. Asked both to
// share and to write it is to write; asked neither it is EBADF. One that shares the file and
// waits to write it gives up its share before it waits, and one that does not wait keeps it.
// As measured on macOS 27.2.1 (tests/kernel/locks/locklist.c). A file of the host's can be locked, a
// directory and a device among them, a file or directory of the system volume, and either end
// of a terminal; of what is no file (a pipe, a socket, a kqueue, memory with a name) it is not
// supported; of the rest it is not there.
func sysFlock(p *Process, a []uint64) int64 {
	f, how := p.fds[a[0]], int32(a[1])
	switch {
	case f == nil:
		return -eBADF
	}
	var id hostFile
	switch {
	case f.kind == hostFD:
		var st syscall.Stat_t
		if err := syscall.Fstat(f.host, &st); err != nil {
			return hostErrno(err)
		}
		id = hostFileOf(&st)
	case f.kind == ttyFD && !f.own: // each end of a terminal is a file of its own, as measured
		id = hostFile{dev: ^uint64(0), ino: f.tty.id<<1 | uint64(truth(f.first))}
	case f.streams(), f.shared != nil, f.kq != nil, f.kind == stdioFD: // as measured of a pipe, a socket, a kqueue and named memory
		return -eNOTSUP
	case (f.kind == dirFD || f.kind == cacheFD || f.kind == dataFD) && f.path != "":
		// A file or directory of the system volume, which is the one its name says, as measured
		// of /bin/ls and /usr/share.
		if p.m.lockNames == nil {
			p.m.lockNames = map[string]uint64{}
		}
		if _, ok := p.m.lockNames[f.path]; !ok {
			p.m.lockNames[f.path] = uint64(len(p.m.lockNames))
		}
		id = hostFile{dev: ^uint64(0) - 1, ino: p.m.lockNames[f.path]}
	default:
		p.unimplemented("a lock on the whole of what is no file, no directory and no terminal", a)
		return -eNOSYS
	}
	m, owner := p.m, lockOwner{file: f.status(), whole: true}
	kind := int16(lockNone)
	switch {
	case how&lockUnlock != 0:
		m.takeRange(id, owner, 0, toTheEnd, lockNone)
		m.retryRanges()
		return 0
	case how&lockExclusive != 0:
		kind = lockWrite
	case how&lockShared != 0:
		kind = lockRead
	default:
		return -eBADF
	}
	if held := m.against(id, owner, 0, toTheEnd, kind == lockWrite); held != nil {
		if how&lockNonblock != 0 {
			return -eAGAIN
		}
		on := held.owner
		if kind == lockWrite {
			m.takeRange(id, owner, 0, toTheEnd, lockNone)
		}
		p.current.rangeWait = &rangeWait{id: id, owner: owner, to: toTheEnd, write: kind == lockWrite}
		m.queue(p.current.rangeWait, on)
		m.retryRanges()
		p.wait(waitRange, 0, time.Time{}, result{})
		return 0
	}
	m.takeRange(id, owner, 0, toTheEnd, kind)
	m.retryRanges()
	return 0
}
