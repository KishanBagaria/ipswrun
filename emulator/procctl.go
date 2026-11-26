package emulator

import (
	"bytes"
	"fmt"
)

// What a process tells the kernel of itself through proc_info, as the kernel's own source has
// it (xnu's proc_setcontrol, proc_dirtycontrol and memorystatus_dirty_*) and as macOS 27.2.1
// was measured to answer each thing (examples/procctl.c).

// procSetControl is proc_info's call 5, of which the one thing asked is a name for the thread
// that asks (pthread_setname_np): of up to 63 bytes (ENAMETOOLONG), and of the caller's own
// process (EINVAL). The name is kept for the thread. The other things it may set stop.
func (p *Process) procSetControl(pid uint32, flavor, buf, size uint64, a []uint64) int64 {
	const threadName, longest = 2, 63
	switch {
	case pid != p.pid:
		return -eINVAL
	case flavor != threadName:
		p.unimplemented(fmt.Sprintf("proc_info call 5 (what a process sets of itself), flavor %d", flavor), a)
		return -eNOSYS
	case size > longest:
		return -eNAMETOOLONG
	}
	name, _, _ := bytes.Cut(p.read(buf, size), []byte{0}) // as far as its first zero
	p.current.name = string(name)
	return 0
}

// dirtyTracking is what a process has told the kernel of having work in hand, so that it may
// be ended when it has none and memory is short, which it never is here: it is kept and said
// back, as the bits proc_get_dirty answers with, and nothing is ended.
type dirtyTracking uint32

const (
	dirtyTracked, dirtyIdleExit, dirtyHasWork, dirtyLaunching dirtyTracking = 1, 2, 4, 8
)

// procDirtyControl is proc_info's call 8, of the caller's own process: keeping track (1), having
// work or none (2), what it is (3), and giving up that it is starting or put off (4). Before it
// keeps track there is nothing to set or give up (EINVAL), and what it says is 0; saying again
// what it has said of its work is EALREADY. Being put off is taken and changes nothing that is
// said back. Of launchd and of the kernel a user is refused keeping track (EPERM), and the
// kernel says 0; of no process it is ESRCH. What was not measured stops: any other process,
// one that has ended, and being ended as soon as it has no work (0x20), which a Mac's kernel
// does at once to one that has none.
func (p *Process) procDirtyControl(pid uint32, flavor, arg uint64, a []uint64) int64 {
	const track, set, get, clear = 1, 2, 3, 4
	const tracked, idleExit, deferred, launching, deferAlways = 1, 2, 4, 8, 0x10 // what is asked for
	if pid != p.pid {
		about := p.m.process(pid)
		switch {
		case (pid == launchdPID || pid == 0) && flavor == track && p.uid != 0:
			return -ePERM
		case pid == 0 && flavor == get:
			return 0
		case pid != launchdPID && pid != 0 && about == nil && p.m.ended(pid) == nil:
			return -eSRCH
		}
		p.unimplemented(fmt.Sprintf("proc_info call 8 (keeping track of work in hand) of another process, flavor %d", flavor), a)
		return -eNOSYS
	}
	d, with := &p.dirtying, uint32(arg)
	switch flavor {
	case track:
		// To be ended when idle, and to be starting, are for one that keeps track; being put
		// off is one way or the other, and for one that may be ended when idle.
		off := with & (deferred | deferAlways)
		switch {
		case with&(idleExit|launching) != 0 && with&tracked == 0, off == deferred|deferAlways, off != 0 && with&idleExit == 0:
			return -eINVAL
		case with&^(tracked|idleExit|deferred|launching|deferAlways) != 0:
			p.unimplemented(fmt.Sprintf("proc_info call 8 keeping track with %#x, to be ended as soon as it has no work or what is beyond that", with), a)
			return -eNOSYS
		}
		if with&tracked != 0 {
			*d |= dirtyTracked
		}
		if with&idleExit != 0 {
			*d |= dirtyIdleExit
		}
		if with&launching != 0 {
			*d |= dirtyLaunching
		}
	case set:
		switch has := *d&dirtyHasWork != 0; {
		case *d&dirtyTracked == 0:
			return -eINVAL
		case has == (with != 0):
			return -eALREADY
		}
		*d ^= dirtyHasWork
	case get:
		return int64(*d)
	case clear:
		if *d&dirtyTracked == 0 || with&(launching|deferred|deferAlways) == 0 {
			return -eINVAL
		}
		if with&launching != 0 {
			*d &^= dirtyLaunching
		}
	default:
		p.unimplemented(fmt.Sprintf("proc_info call 8 (keeping track of work in hand), flavor %d", flavor), a)
		return -eNOSYS
	}
	return 0
}
