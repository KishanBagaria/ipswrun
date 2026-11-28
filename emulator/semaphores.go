package emulator

import "time"

// Semaphores by name (sem_open): a count that as many waits pass as it holds before one has to
// wait for it to be given back, made, found and taken away by a name, and held by a descriptor,
// which is what the library hands out as the semaphore. A copy of a process shares its
// semaphores with it. As measured on macOS 27.2.1 (tests/kernel/posixipc.c), and from the source
// (bsd/kern/posix_sem.c) where it says so.

type namedSemaphore struct {
	id       uint64 // what a thread that waits for it waits on
	value    int
	mode     uint16 // what it was made with, and whose it is
	uid, gid uint32
	waiters  []*thread // in the order they came, which is the order they are let through, as measured
}

const (
	semNameMost  = 30    // a semaphore's name, as measured (31 is refused); the source's limit counts the name's end
	shmNameMost  = 31    // shared memory's
	semValueMost = 32767 // SEM_VALUE_MAX
)

// sysSemOpen is sem_open(name, flags, mode, value): the descriptor of a semaphore of that name,
// made with the value if it is asked for and not there. A name of more than 31 bytes is
// ENAMETOOLONG. A value above the most one holds is EINVAL, from the source.
func sysSemOpen(p *Process, a []uint64) int64 {
	name := p.cstr(a[0])
	s := p.m.semaphores[name]
	switch {
	case len(name) > semNameMost:
		return -eNAMETOOLONG
	case p.full():
		return -eMFILE
	case a[1]&oCreate != 0 && a[3] > semValueMost: // whether or not it is there already, as measured
		return -eINVAL
	case s == nil && a[1]&oCreate == 0:
		return -eNOENT
	case s == nil:
		p.m.pipes++
		s = &namedSemaphore{id: p.m.pipes, value: int(a[3]), mode: uint16(a[2] & 0o7777), uid: p.uid, gid: p.gid}
		if p.m.semaphores == nil {
			p.m.semaphores = map[string]*namedSemaphore{}
		}
		p.m.semaphores[name] = s
	case a[1]&oCreate != 0 && a[1]&oExcl != 0:
		return -eEXIST
	case !p.allowed(s.mode, s.uid, s.gid, false): // one that its mode does not let be read is not opened, as measured of its owner
		return -eACCES
	}
	return int64(p.newFD(&fd{kind: semFD, sem: s, refs: 1}))
}

// semaphoreOf is the semaphore a descriptor holds; none for any other descriptor (EBADF, as
// measured of one that was closed).
func (p *Process) semaphoreOf(n uint64) *namedSemaphore {
	if f := p.fds[n]; f != nil && f.kind == semFD {
		return f.sem
	}
	return nil
}

// sysSemClose is sem_close: the descriptor is given up, and the semaphore is as it was.
func sysSemClose(p *Process, a []uint64) int64 {
	if p.semaphoreOf(a[0]) == nil {
		return -eBADF
	}
	return p.closeFD(a[0])
}

// sysSemUnlink takes the name away: whoever has the semaphore keeps it.
func sysSemUnlink(p *Process, a []uint64) int64 {
	name := p.cstr(a[0])
	switch {
	case len(name) > semNameMost:
		return -eNAMETOOLONG
	case p.m.semaphores[name] == nil:
		return -eNOENT
	case p.m.semaphores[name].mode&3 != 0:
		// The source asks its mode of the caller as if it were what is wanted, which was
		// not measured: it would leave a semaphore on the measuring Mac until it restarts.
		p.unimplemented("sem_unlink of a semaphore made with a mode that lets others write or run it", a)
		return -eNOSYS
	}
	delete(p.m.semaphores, name)
	return 0
}

// sysSemWait takes one of the semaphore's count, waiting for one to be given if there is none.
// A signal that is caught breaks the wait off (EINTR), whatever its handler says of calls being
// made again: measured of one that does not say so, and from the source of one that does.
func sysSemWait(p *Process, a []uint64) int64 {
	s := p.semaphoreOf(a[0])
	switch {
	case s == nil:
		return -eBADF
	case s.value > 0:
		s.value--
		return 0
	}
	s.waiters = append(s.waiters, p.current)
	p.wait(waitNamed, s.id, time.Time{}, result{})
	return 0
}

// sysSemTrywait takes one if there is one, and is EAGAIN if not.
func sysSemTrywait(p *Process, a []uint64) int64 {
	s := p.semaphoreOf(a[0])
	switch {
	case s == nil:
		return -eBADF
	case s.value == 0:
		return -eAGAIN
	}
	s.value--
	return 0
}

// sysSemPost gives one back: to the thread that has waited for it longest, of whichever
// process, or to the count, which has no most, as measured.
func sysSemPost(p *Process, a []uint64) int64 {
	s := p.semaphoreOf(a[0])
	if s == nil {
		return -eBADF
	}
	for len(s.waiters) > 0 {
		t := s.waiters[0]
		s.waiters = s.waiters[1:]
		if t.state == waiting && t.kind == waitNamed && t.key == s.id { // not one a signal brought back, or that ended
			t.answer = &result{}
			t.wake()
			return 0
		}
	}
	s.value++
	return 0
}
