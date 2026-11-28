package emulator

import (
	"bytes"
	"syscall"

	"golang.org/x/sys/unix"
)

// A file mapped to be shared (mmap with MAP_SHARED of a file under the home or a mount). What is
// written through the mapping is the file's and what is written to the file is the mapping's,
// at once, for every mapping of it in every process. The mapping is memory of the machine's
// own, one for the file, which every mapping of the file is a window on; it is written to the
// file before the file is read, and read from the file after the file is written, cut or
// grown, so that nobody finds the two apart. As measured on macOS 27.2.1 (tests/kernel/memory/mapfile.c).
//
// A mapping may reach past the file's end: the pages past the one its last byte is in fault
// (SIGBUS), and are taken back from whoever has them when the file is cut. A gigabyte of the
// host's addresses is kept for the memory, which costs nothing until it is touched, so that the
// file can grow under its mappings: a file larger than that, mapped, stops the run. What reads or changes the file by another way than read, pread, write,
// pwrite and ftruncate (a copy of it, a truncation by its name, the host) does not go by it.

type mappedFile struct {
	shared *sharedMemory
	host   int    // a descriptor of the file, the machine's own
	shadow []byte // what the file was last known to hold, to tell what was written through a mapping
	owed   bool   // what was written through a mapping has reached the file with its time of change left as it was
}

const mappedRoom = 1 << 30

// fileOf is the host file a descriptor is of.
func fileOf(host int) (hostFile, int64, bool) {
	var st syscall.Stat_t
	if syscall.Fstat(host, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return hostFile{}, 0, false
	}
	return hostFileOf(&st), st.Size, true
}

// mappedFor is the mapping of a descriptor's file, made if there is none; nil with the errno
// if it cannot be.
func (p *Process) mappedFor(f *fd) (*mappedFile, int64) {
	id, size, ok := fileOf(f.host)
	if !ok {
		return nil, -eNODEV
	}
	if mf := p.m.mappedFiles[id]; mf != nil {
		return mf, 0
	}
	if uint64(size) > mappedRoom {
		stopf("a file of %d bytes mapped to be shared: more than the gigabyte kept for one", size)
	}
	host, err := syscall.Dup(f.host)
	if err != nil {
		return nil, hostErrno(err)
	}
	s := p.m.newShared(mappedRoom)
	if s == nil {
		syscall.Close(host)
		return nil, -eNOMEM
	}
	mf := &mappedFile{shared: s.retain(), host: host}
	mf.load()
	if p.m.mappedFiles == nil {
		p.m.mappedFiles = map[hostFile]*mappedFile{}
	}
	p.m.mappedFiles[id] = mf
	return mf, 0
}

// load makes the memory what the file holds, with nothing past its end.
func (mf *mappedFile) load() {
	data := mf.shared.data
	_, size, _ := fileOf(mf.host)
	if uint64(size) > uint64(len(data)) {
		stopf("a mapped file grown to %d bytes: more than the gigabyte kept for one", size)
	}
	held := uint64(size)
	if n, err := syscall.Pread(mf.host, data[:held], 0); err != nil || uint64(n) != held {
		stopf("a mapped file could not be read: %d of %d bytes, %v", n, held, err)
	}
	clear(data[held:max(held, uint64(len(mf.shadow)))])
	mf.shadow = bytes.Clone(data[:held])
}

// store writes to the file what was written through a mapping, a page at a time and no further
// than the file's end: what is written past it in its last page is no part of the file. The
// file's time of change is left as it was unless it is msync that asks, as measured: a read of
// the file after a write through its mapping does not move it, and msync does.
func (mf *mappedFile) store(moves bool) {
	data := mf.shared.data
	var before syscall.Stat_t
	wrote := false
	defer func() {
		switch {
		case wrote && !moves:
			restoreTimes(mf.host, &before)
			mf.owed = true
		case moves && mf.owed && !wrote:
			unix.Futimes(mf.host, nil) // now
			mf.owed = false
		case moves:
			mf.owed = false
		}
	}()
	syscall.Fstat(mf.host, &before)
	for at := 0; at < len(mf.shadow); at += pageSize {
		end := min(at+pageSize, len(mf.shadow))
		if bytes.Equal(data[at:end], mf.shadow[at:end]) {
			continue
		}
		if n, err := syscall.Pwrite(mf.host, data[at:end], int64(at)); err != nil || n != end-at {
			stopf("a mapped file could not be written: %d of %d bytes, %v", n, end-at, err)
		}
		wrote = true
		copy(mf.shadow[at:end], data[at:end])
	}
}

// mapFile maps a window on a mapped file's memory, which may reach past the memory there is:
// those pages are never the file's, and fault.
func (p *Process) mapFile(mf *mappedFile, addr, length, off uint64, fixed bool, deny uint32) int64 {
	if off%pageSize != 0 || length == 0 { // as measured
		return -eINVAL
	}
	start, ok := p.alloc(length, allocation{addr: addr, fixed: fixed, kind: sharedRegion, shared: mf.shared, off: off, name: "mapped file", deny: deny})
	if !ok {
		return -eNOMEM
	}
	return int64(start)
}

// mappedOf is the mapping of the file a host descriptor is of, if it has one.
func (m *Machine) mappedOf(host int) *mappedFile {
	if len(m.mappedFiles) == 0 {
		return nil
	}
	id, _, ok := fileOf(host)
	if !ok {
		return nil
	}
	return m.mappedFiles[id]
}

// beforeRead brings a file up to what was written through its mappings, before it is read.
func (m *Machine) beforeRead(host int) {
	if mf := m.mappedOf(host); mf != nil {
		mf.store(false)
	}
}

// valid is how far into its memory a mapped file's pages reach: to the end of the page its last
// byte is in. A mapping may reach further, and what is past that faults (SIGBUS), as measured.
func (mf *mappedFile) valid() uint64 { return align(uint64(len(mf.shadow)), pageSize) }

// mappedOwner is the mapped file a shared memory is, if it is one's.
func (m *Machine) mappedOwner(s *sharedMemory) *mappedFile {
	for _, mf := range m.mappedFiles {
		if mf.shared == s {
			return mf
		}
	}
	return nil
}

// reload brings a file's mappings up to the file after it was written, cut or grown, and takes
// from every process the pages that are now past the file's last: touched again, they fault.
func (m *Machine) reload(mf *mappedFile) {
	mf.load()
	valid := mf.valid()
	for _, p := range m.procs {
		if p.exited {
			continue
		}
		taken := false
		for _, r := range p.regions {
			if r.kind != sharedRegion || r.shared != mf.shared || r.off+(r.end-r.start) <= valid {
				continue
			}
			for at := r.start + (max(valid, r.off) - r.off); at < r.end; at += pageSize {
				if p.isMapped(at) {
					p.uc.memUnmap(at, pageSize)
					delete(p.mapped, at/pageSize)
					taken = true
				}
			}
		}
		if taken {
			p.uc.forgetTranslations()
		}
	}
}

// mappedAt is the mapping of the file a path of the host's root names, if it has one.
func (m *Machine) mappedAt(t target) *mappedFile {
	var st syscall.Stat_t
	if len(m.mappedFiles) == 0 || t.root == nil || statInRoot(t.root, t.rel, true, &st) != nil {
		return nil
	}
	return m.mappedFiles[hostFileOf(&st)]
}

// afterWrite brings a file's mappings up to the file, after it was written, cut or grown.
func (m *Machine) afterWrite(host int) {
	if mf := m.mappedOf(host); mf != nil {
		m.reload(mf)
	}
}

// settleMapped writes every mapped file out, and lets go of those nothing maps any more.
func (m *Machine) settleMapped() { m.settle(false) }

func (m *Machine) settle(moves bool) {
	for id, mf := range m.mappedFiles {
		mf.store(moves)
		if mf.shared.refs <= 1 {
			syscall.Close(mf.host)
			mf.shared.release()
			delete(m.mappedFiles, id)
		}
	}
}

// sysMsync is msync(address, length, flags): what was written through a mapping of a file is
// written to it, which here it is as soon as anyone looks. Asked both to wait and not to wait
// it is EINVAL, as is an address inside a page; of memory that is not mapped ENOMEM; of memory
// that is no file's nothing. As measured.
func sysMsync(p *Process, a []uint64) int64 {
	const async, invalidate, synchronous, known = 0x1, 0x2, 0x10, 0x1 | 0x2 | 0x10
	addr, flags := a[0]&tbiMask, a[2]
	switch {
	case addr%pageSize != 0, flags&async != 0 && flags&synchronous != 0, a[1] == 0: // as measured
		return -eINVAL
	case flags&^known != 0:
		p.unimplemented("msync with flags that were not measured", a)
		return -eNOSYS
	}
	for at, end := addr, addr+align(a[1], pageSize); at < end; {
		r := p.findRegion(at)
		if r == nil {
			return -eNOMEM
		}
		at = r.end
	}
	p.m.settle(true)
	return 0
}

// restoreTimes sets a file's times of access and change back to what a stat of it said.
func restoreTimes(host int, st *syscall.Stat_t) {
	access, change := hostTimes(st)
	unix.Futimes(host, []unix.Timeval{unix.NsecToTimeval(access), unix.NsecToTimeval(change)})
}
