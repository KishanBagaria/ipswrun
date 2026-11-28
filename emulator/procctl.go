package emulator

import (
	"bytes"
	"fmt"
	"strings"
	"syscall"
	"time"
)

// What a process tells the kernel of itself through proc_info, as the kernel's own source has
// it (xnu's proc_setcontrol, proc_dirtycontrol and memorystatus_dirty_*) and as macOS 27.2.1
// was measured to answer each thing (tests/kernel/proc/procctl.c).

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

// sysProcRlimitControl is proc_rlimit_control(pid, kind, argument), by which a daemon asked for
// a limit on how often it may be woken: the kernel takes that and keeps nothing, its source
// says, and as measured (tests/kernel/proc/daemonasks.c). The pid is -1 for the caller; another user's
// process is EACCES, none ESRCH, and a kind there is not EINVAL. The kinds that do something
// (how much of a processor it may use, and its memory over a time) stop.
func sysProcRlimitControl(p *Process, a []uint64) int64 {
	const wakeups, last = 1, 4
	pid, kind := int32(a[0]), uint32(a[1])
	about := p
	if pid == 0 {
		p.unimplemented("proc_rlimit_control of the kernel, pid 0", a)
		return -eNOSYS
	}
	if pid != -1 && uint32(pid) != p.pid {
		if about = p.m.process(uint32(pid)); about == nil && uint32(pid) != launchdPID {
			return -eSRCH
		}
	}
	switch {
	case p.uid != 0 && (about == nil || about.uid != p.uid): // launchd is root's
		return -eACCES
	case kind == wakeups:
		return 0
	case kind > wakeups && kind <= last:
		p.unimplemented(fmt.Sprintf("proc_rlimit_control of kind %d", kind), a)
		return -eNOSYS
	}
	return -eINVAL
}

// sysProcInfoExtendedID is proc_info_extended_id(call, pid, flavour, how, id, argument, buffer,
// size): proc_info, and for what is asked of a process by its pid (call 2) the process must
// also be the one with a version (1) or a number of its own (2), as a daemon asks of a client
// it knows by its audit token. One that is another by then is ESRCH, once it is found; to ask
// by both is EINVAL, as measured. The other calls take no notice of it, the source says.
func sysProcInfoExtendedID(p *Process, a []uint64) int64 {
	const byVersion, byNumber = 1, 2
	how := uint32(a[3])
	switch {
	case how == byVersion|byNumber:
		return -eINVAL
	case how&^(byVersion|byNumber) != 0:
		p.unimplemented(fmt.Sprintf("proc_info_extended_id told to compare %#x", how), a)
		return -eNOSYS
	case a[0] == 2 && how != 0:
		p.mustBe = &sameProcess{byNumber: how == byNumber, id: a[4]}
		defer func() { p.mustBe = nil }()
	}
	return sysProcInfo(p, []uint64{a[0], a[1], a[2], a[5], a[6], a[7]})
}

// sameProcess is what a process asked of by its pid must also be: the one with a version, or
// with a number of its own.
type sameProcess struct {
	byNumber bool
	id       uint64
}

// another is whether a process is not the one that was meant.
func (s *sameProcess) another(is lineage) bool {
	if s == nil {
		return false
	}
	if s.byNumber {
		return s.id != is.unique
	}
	return s.id != uint64(is.version)
}

// sysInitgroups is initgroups(how many, groups, user), which sets a process's groups and is
// root's to do: more than 16 is EINVAL, and a user is refused (EPERM), as measured. What root
// is told stops.
func sysInitgroups(p *Process, a []uint64) int64 {
	if uint32(a[0]) > 16 {
		return -eINVAL
	}
	p.read(a[1], 4*uint64(uint32(a[0]))) // the groups are read before it is asked who asks: a fault is EFAULT
	if p.uid != 0 {
		return -ePERM
	}
	p.unimplemented("initgroups by root", a)
	return -eNOSYS
}

// sysMemorystatusControl is memorystatus_control(command, pid, flags, buffer, size), by which
// daemons ask of and set how processes are ended when memory is short. It is root's, or a
// program's with the entitlement for it: any other is refused (EPERM), as the source has it
// and as measured of three commands. What those two are told, and the three commands the
// source lets anyone ask (whether a process may be frozen), stop.
func sysMemorystatusControl(p *Process, a []uint64) int64 {
	const entitlement = "com.apple.private.memorystatus"
	switch command := uint32(a[0]); {
	case (command == 14 || command == 15) && p.uid == 0 && a[4] > 65536:
		return -eINVAL // more than a buffer may be (MEMORYSTATUS_BUFFERSIZE_MAX): from the source, not measured
	case (command == 14 || command == 15) && p.uid == 0:
		// To be kept longer when idle and short of memory, and that taken back, which
		// containermanagerd asks of itself. Memory is never short here, so nothing is kept or
		// ended for it. As measured of root in a virtual machine (macOS 27.2): of itself 0, each
		// time and whatever the flags; of pid 0 and of launchd EINVAL; of no process ESRCH.
		// Of another process it was not measured, and stops.
		switch pid := uint32(a[1]); {
		case pid == 0 || pid == launchdPID:
			return -eINVAL
		case pid == p.pid:
			return 0
		case p.m.process(pid) == nil:
			return -eSRCH
		}
		p.unimplemented(fmt.Sprintf("memorystatus_control command %d of another process", command), a)
		return -eNOSYS
	case command == 18 || command == 19 || command == 24, p.uid == 0, strings.Contains(p.entitlements, entitlement):
		p.unimplemented(fmt.Sprintf("memorystatus_control command %d", command), a)
		return -eNOSYS
	}
	return -ePERM
}

// sysIdentitysvc is identitysvc(opcode, message), by which opendirectoryd makes itself the one
// the kernel asks who a user or a group is, and takes the kernel's questions. The kernel looks
// first for the entitlement for it: a program without is refused whatever it asks (EPERM), as
// the source has it and as measured of a user and of root. With the entitlement, as measured of
// a probe signed with it in a virtual machine (macOS 27.2, 26B123, with SIP off there): a user is
// refused everything; root registers (0, and again); anything else is for root's process that
// registered and no other (EPERM); it is told how many answers the kernel keeps (of groups'
// members and of users, in that order: 500 and 250 once a Mac's opendirectoryd has set them,
// from the 100 of each its log says it found), may set them (EINVAL above the 10,000 the source has; not measured between)
// or have them forgotten, and deregisters, after which it is refused as any other. An opcode that
// names nothing is taken and does nothing. A worker waits for a question of the kernel's, and
// none comes here: nothing in this kernel asks who a user is by anything but a number, so the
// wait has no end, as it had none in three seconds on a Mac and none after deregistering.
// Stopping, not having been measured: a worker's answer to a question (which there never is).
func sysIdentitysvc(p *Process, a []uint64) int64 {
	const register, answer, worker, deregister, getSizes, setSizes, clear = 0, 1, 2, 4, 8, 16, 32
	const most = 10000
	opcode := uint32(a[0])
	switch {
	case !strings.Contains(p.entitlements, "com.apple.private.identitysvc"):
		return -ePERM
	case opcode == register:
		if p.uid != 0 {
			return -ePERM
		}
		p.m.resolver = p.pid
		return 0
	case p.uid != 0 || p.m.resolver != p.pid:
		return -ePERM
	case opcode == getSizes:
		p.write(a[1], append(le32(p.m.caches[0]), le32(p.m.caches[1])...))
		return 0
	case opcode == setSizes:
		sizes := p.read(a[1], 8)
		groups, users := le.Uint32(sizes), le.Uint32(sizes[4:])
		if users > most || groups > most {
			return -eINVAL
		}
		p.m.caches = [2]uint32{groups, users}
		return 0
	case opcode == clear:
		return 0 // the kernel keeps no answers here, there being no questions
	case opcode == deregister:
		p.m.resolver = 0
		return 0
	case opcode&answer != 0:
		p.unimplemented("identitysvc with the resolver's answer to a question the kernel never asked", a)
		return -eNOSYS
	case opcode&worker != 0:
		p.wait(waitResolver, 0, time.Time{}, result{})
		return 0
	}
	return 0
}

// The mark dirhelper puts on the per-user folders it makes, so that only what the system allows
// removes them: an attribute that names their class of storage. (It also sets, by fchflags, the
// flag that they may not be unlinked.)
const (
	rootlessAttribute = "com.apple.rootless"
	flagNoUnlink      = 0x100000 // SF_NOUNLINK
)

// sandboxStorageClass is the sandbox's calls 261 and 264, {a descriptor, the name of a class of
// storage}, by which dirhelper marks a folder it has made and then checks the mark. They are the
// sandbox extension's, in no source, and were measured of root in a virtual machine (macOS 27.2,
// 26B123, with SIP off there). Both: no class is EINVAL, a descriptor that is not open EBADF.
// 261 gives the file the attribute com.apple.rootless with the class for its value, whatever the
// class and whether or not the program has an entitlement, and a user is refused it (EACCES).
// 264 says whether the file has that class: 0 if so, EINVAL if it has another, and that there is
// no such attribute (ENOATTR) if it has none. With SIP on, as it is here, who may set a class
// was not measured: it is done for a program that has the entitlement for the class, as
// dirhelper has for "folders", and stops for another, as does a user's check.
func (p *Process) sandboxStorageClass(a []uint64, set bool) int64 {
	const eNOATTR = 93
	args := p.read(a[2], 16)
	fd, class := le.Uint64(args), le.Uint64(args[8:])
	switch {
	case class == 0:
		return -eINVAL
	case p.fds[fd] == nil:
		return -eBADF
	case p.uid != 0 && set:
		return -eACCES
	}
	name := p.cstr(class)
	if p.uid != 0 || set && !strings.Contains(p.entitlements, "<key>com.apple.rootless.storage."+name+"</key>") {
		p.unimplemented("a class of storage ("+name+") set by a program with no entitlement for it, or checked by a user", a)
		return -eNOSYS
	}
	_, id, errno := p.changeableFD(fd)
	if errno != 0 {
		p.unimplemented("the class of storage of a file that is not under the home or a mount", a)
		return -eNOSYS
	}
	if set {
		p.m.mark(id, name)
		return 0
	}
	switch has, ok := p.m.xattrs[id][rootlessAttribute]; {
	case !ok:
		return -eNOATTR
	case string(has) != name:
		return -eINVAL
	}
	return 0
}

// mark gives a host file the attribute of a class of storage.
func (m *Machine) mark(id hostFile, class string) {
	if m.xattrs[id] == nil {
		m.xattrs[id] = map[string][]byte{}
	}
	m.xattrs[id][rootlessAttribute] = []byte(class)
}

// isDirectory says whether an open host file is a directory.
func (p *Process) isDirectory(f *fd) bool {
	var st syscall.Stat_t
	return syscall.Fstat(f.host, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR
}
