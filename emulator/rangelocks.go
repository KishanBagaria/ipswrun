package emulator

import (
	"io"
	"slices"
	"syscall"

	"github.com/blacktop/go-macho"
)

// Locks on parts of a file that are an open file's (fcntl's F_OFD_SETLK and F_OFD_GETLK, which
// SQLite takes on every database it opens), as a Mac's kernel was measured to keep them
// (examples/rangelocks.c). A lock is held by the open file, so a duplicate and a copy of the
// process hold it too, and another open of the same file, in the same process or not, is
// refused what it conflicts with (EAGAIN): any lock where one of the two is to write. Locking
// again what it holds changes the kind; unlocking what is not locked is no error; the locks go
// with the last descriptor of the open file. They are the machine's and not the host's, so
// they are the same on every host.

type rangeLock struct {
	owner    *status
	from, to uint64 // to is past its last byte; all ones is to the end of the file, however long it gets
	write    bool
}

const (
	lockRead, lockNone, lockWrite = 1, 2, 3 // F_RDLCK, F_UNLCK, F_WRLCK
	toTheEnd                      = ^uint64(0)
	flockSize                     = 24 // struct flock: start, length, pid, type, whence
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
	switch {
	case kind != lockRead && kind != lockNone && kind != lockWrite, start < 0:
		return id, 0, 0, 0, -eINVAL
	case length < 0:
		p.unimplemented("a lock on a part of a file of a length below zero", a)
		return id, 0, 0, 0, -eNOSYS
	}
	if to = uint64(start + length); length == 0 {
		to = toTheEnd
	}
	return hostFileOf(&st), uint64(start), to, kind, 0
}

// against is the lock of another open file that one asked for would conflict with, the lowest
// in the file if there are several.
func (m *Machine) against(id hostFile, owner *status, from, to uint64, write bool) *rangeLock {
	var found *rangeLock
	for i, l := range m.ranges[id] {
		if l.owner != owner && l.from < to && from < l.to && (write || l.write) && (found == nil || l.from < found.from) {
			found = &m.ranges[id][i]
		}
	}
	return found
}

// setRange is F_OFD_SETLK, and F_OFD_SETLKW where there is nothing to wait for.
func (p *Process) setRange(f *fd, arg uint64, wait bool, a []uint64) int64 {
	id, from, to, kind, errno := p.rangeAsked(f, arg, a)
	if errno != 0 {
		return errno
	}
	if mode := f.accessMode(); kind == lockWrite && mode == 0 || kind == lockRead && mode == 1 {
		return -eBADF // it was not opened for what the lock is for
	}
	m, owner := p.m, f.status()
	if kind != lockNone && m.against(id, owner, from, to, kind == lockWrite) != nil {
		if wait {
			p.unimplemented("waiting for a lock on part of a file that another open of it holds (F_OFD_SETLKW)", a)
		}
		return -eAGAIN
	}
	// What the open file held of that part is given up, the rest of each of its locks kept.
	var kept []rangeLock
	for _, l := range m.ranges[id] {
		if l.owner != owner || l.to <= from || to <= l.from {
			kept = append(kept, l)
			continue
		}
		if l.from < from {
			kept = append(kept, rangeLock{owner, l.from, from, l.write})
		}
		if to < l.to {
			kept = append(kept, rangeLock{owner, to, l.to, l.write})
		}
	}
	if kind != lockNone {
		kept = append(kept, rangeLock{owner, from, to, kind == lockWrite})
	}
	if m.ranges == nil {
		m.ranges = map[hostFile][]rangeLock{}
	}
	if m.ranges[id] = kept; len(kept) == 0 {
		delete(m.ranges, id)
	}
	return 0
}

// getRange is F_OFD_GETLK: the lock that the one asked about would conflict with, which is of
// no process (-1), or that there is none, with the rest of what was asked left as it was.
func (p *Process) getRange(f *fd, arg uint64, a []uint64) int64 {
	id, from, to, kind, errno := p.rangeAsked(f, arg, a)
	switch {
	case errno != 0:
		return errno
	case kind == lockNone, kind == lockWrite && f.accessMode() == 0, kind == lockRead && f.accessMode() == 1:
		p.unimplemented("asking what stands against an unlock, or against a lock the file was not opened for", a)
		return -eNOSYS
	}
	l := p.m.against(id, f.status(), from, to, kind == lockWrite)
	if l == nil {
		p.write(arg+20, le16(lockNone))
		return 0
	}
	is, length := uint16(lockRead), l.to-l.from
	if l.write {
		is = lockWrite
	}
	if l.to == toTheEnd {
		length = 0
	}
	p.write(arg, slices.Concat(le64(l.from, length), le32(0xFFFFFFFF), le16(is, 0)))
	return 0
}

// unlockRanges gives up every lock of an open file, when its last descriptor is closed.
func (m *Machine) unlockRanges(owner *status) {
	for id, locks := range m.ranges {
		if locks = slices.DeleteFunc(locks, func(l rangeLock) bool { return l.owner == owner }); len(locks) == 0 {
			delete(m.ranges, id)
		} else {
			m.ranges[id] = locks
		}
	}
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
