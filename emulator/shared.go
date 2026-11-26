package emulator

import "syscall"

// Memory that is shared is the host's: each mapping of it, in one process or in several, is
// given the same pages, so what is written through one is read through the others. It is what
// shm_open names, where a daemon and its clients keep the counters by which a client sees,
// without asking, that something has changed. And it is what memory becomes once a port names
// it (mach_make_memory_entry) or it is mapped a second time (mach_vm_remap).

const (
	oCreate   = 0x200
	oExcl     = 0x800
	mapShared = 0x1
)

// sharedMemory is memory of the host's that mappings share. It is kept for as long as anything
// refers to it: a mapping, an open descriptor, a port that names it, or the name shm_open knows
// it by.
type sharedMemory struct {
	data []byte // a whole number of pages
	refs int
}

// newShared is shared memory of a size, zeroed, that nothing refers to yet. With no size it is
// memory still to be given one (see resize). Nil if the host has none of that size to give.
func (m *Machine) newShared(size uint64) *sharedMemory {
	s := &sharedMemory{}
	if size > 0 && s.resize(size) != 0 {
		return nil
	}
	m.shared = append(m.shared, s)
	return s
}

// resize is ftruncate: it gives the memory its size, once.
func (s *sharedMemory) resize(size uint64) int64 {
	size = align(size, pageSize)
	switch {
	case size == uint64(len(s.data)):
		return 0
	case s.data != nil || size == 0 || size > 1<<30:
		return -eINVAL
	}
	data, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return -eNOMEM
	}
	s.data = data
	return 0
}

func (s *sharedMemory) retain() *sharedMemory {
	s.refs++
	return s
}

// release ends one reference, and with the last gives the memory back to the host.
func (s *sharedMemory) release() {
	if s.refs--; s.refs <= 0 {
		s.free()
	}
}

func (s *sharedMemory) free() {
	if s.data != nil {
		syscall.Munmap(s.data)
		s.data = nil
	}
}

// window reports whether whole pages from off, length long, are all within the memory.
func (s *sharedMemory) window(off, length uint64) bool {
	return off%pageSize == 0 && length > 0 && off <= uint64(len(s.data)) && align(length, pageSize) <= uint64(len(s.data))-off
}

// span is a stretch of shared memory.
type span struct {
	of        *sharedMemory
	off, size uint64
}

// share makes the pages a stretch of the process's memory is in memory that can be mapped again,
// and returns it: what the pages hold moves to memory of the host's, which the pages then are.
// Nil for a stretch that is not all memory the process has, that is of the shared cache, which
// stays what it is, or that is shared in part already: making it one would cut the part that is
// shared off from whoever shares it.
func (p *Process) share(addr, size uint64) *span {
	start := addr &^ (pageSize - 1)
	size = align(addr+size, pageSize) - start
	first := p.findRegion(start)
	if first != nil && first.kind == sharedRegion && start+size <= first.end {
		return &span{first.shared, first.off + (start - first.start), size} // it is already
	}
	name := "shared"
	for at := start; at < start+size; {
		r := p.findRegion(at)
		if r == nil || r.kind == cacheRegion || r.kind == sharedRegion {
			return nil
		}
		at, name = r.end, r.name
	}
	s := p.m.newShared(size)
	if s == nil {
		return nil
	}
	p.readInto(s.data[:size], start)
	p.alloc(size, allocation{addr: start, fixed: true, kind: sharedRegion, shared: s, name: name})
	return &span{s, 0, size}
}

// sysShmOpen opens memory by name, making it if asked to. Nothing has any before a guest makes it.
func sysShmOpen(p *Process, a []uint64) int64 {
	name := p.cstr(a[0])
	s := p.m.named[name]
	p.note("  shm_open %s", name)
	switch {
	case p.full():
		return -eMFILE
	case s == nil && a[1]&oCreate == 0:
		return -eNOENT
	case s == nil:
		s = p.m.newShared(0).retain()
		p.m.named[name] = s
	case a[1]&oCreate != 0 && a[1]&oExcl != 0:
		return -eEXIST
	}
	return int64(p.newFD(&fd{kind: dataFD, shared: s.retain(), flags: uint32(a[1] & 3)})) // what it may do, for fcntl
}

// sysShmUnlink takes the name away. Whoever has the memory open or mapped keeps it.
func sysShmUnlink(p *Process, a []uint64) int64 {
	name := p.cstr(a[0])
	s := p.m.named[name]
	if s == nil {
		return -eNOENT
	}
	delete(p.m.named, name)
	s.release()
	return 0
}

// mapShared is mmap of shared memory, shared: the mapping is the memory itself, denied deny.
func (p *Process) mapShared(s *sharedMemory, addr, length, off uint64, fixed bool, deny uint32) int64 {
	if !s.window(off, length) {
		return -eINVAL
	}
	start, ok := p.alloc(length, allocation{addr: addr, fixed: fixed, kind: sharedRegion, shared: s, off: off, name: "shared memory", deny: deny})
	if !ok {
		return -eNOMEM
	}
	return int64(start)
}
