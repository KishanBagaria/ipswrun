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

// sharedMemory is the memory behind a name given to shm_open. It has no size until ftruncate
// gives it one, and keeps that size.
type sharedMemory struct {
	data []byte // a whole number of pages of the host's
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
	if s.data = hostPages(size); s.data == nil {
		return -eNOMEM
	}
	return 0
}

// hostPages is zeroed memory of the host's, a whole number of pages; nil if there is none to have.
func hostPages(size uint64) []byte {
	data, err := syscall.Mmap(-1, 0, int(align(size, pageSize)), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil
	}
	return data
}

// hostMemory is host memory with no name, which the machine keeps until it is closed.
func (m *Machine) hostMemory(size uint64) []byte {
	data := hostPages(size)
	if data != nil {
		m.unnamed = append(m.unnamed, data)
	}
	return data
}

// share makes whole pages of the process's memory memory that can be mapped again, and returns
// it: what the pages hold moves to memory of the host's, which the pages then are. False for a
// stretch that is not all memory the process has.
func (p *Process) share(start, size uint64) ([]byte, bool) {
	if size == 0 || size > 1<<30 || start+size < start {
		return nil, false
	}
	first := p.findRegion(start)
	if first != nil && first.kind == sharedRegion && start+size <= first.end {
		from := first.off + (start - first.start)
		return first.data[from : from+size], true // it is already
	}
	name := "shared"
	for at := start; at < start+size; {
		r := p.findRegion(at)
		if r == nil {
			return nil, false
		}
		at, name = r.end, r.name
	}
	host := p.m.hostMemory(size)
	if host == nil {
		return nil, false
	}
	copy(host, p.read(start, size))
	p.alloc(size, allocation{addr: start, fixed: true, kind: sharedRegion, data: host, name: name})
	return host, true
}

func (s *sharedMemory) free() {
	if s.data != nil {
		syscall.Munmap(s.data)
		s.data = nil
	}
}

// sysShmOpen opens memory by name, making it if asked to. Nothing has any before a guest makes it.
func sysShmOpen(p *Process, a []uint64) int64 {
	name := p.cstr(a[0])
	s := p.m.shared[name]
	p.note("  shm_open %s", name)
	switch {
	case s == nil && a[1]&oCreate == 0:
		return -eNOENT
	case s == nil:
		s = &sharedMemory{}
		p.m.shared[name] = s
	case a[1]&oCreate != 0 && a[1]&oExcl != 0:
		return -eEXIST
	}
	return int64(p.newFD(&fd{kind: dataFD, shared: s}))
}

// sysShmUnlink takes the name away. Whoever has the memory open or mapped keeps it.
func sysShmUnlink(p *Process, a []uint64) int64 {
	name := p.cstr(a[0])
	if p.m.shared[name] == nil {
		return -eNOENT
	}
	delete(p.m.shared, name)
	return 0
}

// mapShared is mmap of shared memory, shared: the mapping is the memory itself.
func (p *Process) mapShared(s *sharedMemory, addr, length, off uint64, fixed bool) int64 {
	length = align(length, pageSize)
	if off%pageSize != 0 || length == 0 || off > uint64(len(s.data)) || length > uint64(len(s.data))-off {
		return -eINVAL
	}
	start, ok := p.alloc(length, allocation{addr: addr, fixed: fixed, kind: sharedRegion, data: s.data, off: off, name: "shared memory"})
	if !ok {
		return -eNOMEM
	}
	return int64(start)
}
