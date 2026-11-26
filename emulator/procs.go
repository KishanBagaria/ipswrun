package emulator

import (
	"fmt"
	"slices"
	"time"
)

// The processes there are, as ps asks for them: sysctl {CTL_KERN, KERN_PROC, a kind, of what},
// which answers with a struct kinfo_proc for each. They are those of the machine that run, or
// have ended and are still to be waited for, and launchd, which is there to be asked of as on a
// Mac. A Mac's list has the kernel too (pid 0) and whatever else it runs, which are not here.
// What a record says was measured on macOS 27.2.1 (examples/procs.c); what was not read off a
// real one (priorities, sizes of memory, the terminal's session) is left as zeros.

const (
	kinfoProcSize = 648
	procAll       = 0 // KERN_PROC_ALL
	procPID       = 1 // KERN_PROC_PID
	procGroup     = 2 // KERN_PROC_PGRP
	procUID       = 5 // KERN_PROC_UID
	procRealUID   = 6 // KERN_PROC_RUID

	procRunning = 2      // SRUN
	procZombie  = 5      // SZOMB
	procLP64    = 0x4    // P_LP64
	procExeced  = 0x4000 // P_EXEC: it was started as, or has become, a program
	spareProcs  = 5      // a Mac's kernel says there is room needed for five more than there are
)

// procRecord is what a struct kinfo_proc says of one process.
type procRecord struct {
	pid, parent, group uint32
	uid, gid           uint32
	name               string
	began              time.Time
	state              byte
	execed             bool // it was started as, or has become, a program
	nice               int8
	leads              bool // its session
	files              int  // the descriptors it has open
}

func (r procRecord) bytes() []byte {
	b := make([]byte, kinfoProcSize)
	le.PutUint64(b[0:], uint64(r.began.Unix())) // p_starttime
	le.PutUint32(b[8:], uint32(r.began.Nanosecond()/1000))
	flags := uint32(procLP64)
	if r.execed {
		flags |= procExeced
	}
	le.PutUint32(b[32:], flags)  // p_flag
	b[36] = r.state              // p_stat
	le.PutUint32(b[40:], r.pid)  // p_pid
	b[242] = byte(r.nice)        // p_nice
	copy(b[243:243+16], r.name)  // p_comm, 16 bytes and a zero
	le.PutUint32(b[392:], r.uid) // e_pcred: the real and saved user and group
	le.PutUint32(b[396:], r.uid)
	le.PutUint32(b[400:], r.gid)
	le.PutUint32(b[404:], r.gid)
	le.PutUint32(b[420:], r.uid) // e_ucred: the user, and the groups, its own first
	le.PutUint16(b[424:], 1)
	le.PutUint32(b[428:], r.gid)
	le.PutUint32(b[560:], r.parent)   // e_ppid
	le.PutUint32(b[564:], r.group)    // e_pgid
	le.PutUint32(b[572:], noTerminal) // e_tdev
	if r.leads {
		le.PutUint32(b[612:], 2) // e_flag: EPROC_SLEADER
	}
	return b
}

// record is what the kernel says of a process that is there.
func (p *Process) record() procRecord {
	r := procRecord{pid: p.pid, parent: p.parentPID(), group: p.group(), uid: p.uid, gid: p.gid, name: p.name, began: p.began, state: procRunning,
		execed: p.execed || p.parent == nil, nice: int8(p.nice), leads: p.sessionOf() == p.pid, files: len(p.fds)}
	if p.exited {
		r.state = procZombie
	}
	return r
}

// launchdRecord is what it says of launchd, which is no process of the machine's and is there.
func (m *Machine) launchdRecord() procRecord {
	return procRecord{pid: launchdPID, group: launchdPID, name: "launchd", began: m.clock.boot, state: procRunning, execed: true, leads: true}
}

// procRecords are the processes there are, the newest first, and launchd last.
func (m *Machine) procRecords() []procRecord {
	var out []procRecord
	for i := len(m.procs) - 1; i >= 0; i-- {
		if p := m.procs[i]; p.there() {
			out = append(out, p.record())
		}
	}
	return append(out, m.launchdRecord())
}

// sysctlProcs answers kern.proc: mib is what follows {CTL_KERN, KERN_PROC}.
func (p *Process) sysctlProcs(mib []int32, oldp, oldlenp uint64, a []uint64) int64 {
	if len(mib) == 0 {
		return -eINVAL
	}
	kind, of := mib[0], uint32(0)
	switch kind {
	case procAll:
	case procPID, procGroup, procUID, procRealUID:
		if len(mib) < 2 {
			return -eINVAL
		}
		of = uint32(mib[1])
	case 3, 4, 7: // by session, by terminal, by login context: there to ask on a Mac, and not measured
		p.unimplemented(fmt.Sprintf("sysctl kern.proc of kind %d", kind), a)
		return -eNOSYS
	default:
		return -eNOENT
	}
	wanted := func(r procRecord) bool {
		switch kind {
		case procPID:
			return r.pid == of
		case procGroup:
			return r.group == of
		case procUID, procRealUID: // a process here has one user
			return r.uid == of
		}
		return true
	}
	var found []byte
	for _, r := range p.m.procRecords() {
		if wanted(r) {
			found = append(found, r.bytes()...)
		}
	}
	if oldlenp == 0 {
		return 0
	}
	if oldp == 0 {
		p.write(oldlenp, le64(uint64(len(found))+spareProcs*kinfoProcSize))
		return 0
	}
	room := p.u64(oldlenp)
	fits := min(uint64(len(found)), room/kinfoProcSize*kinfoProcSize)
	p.write(oldp, found[:fits])
	if fits < uint64(len(found)) { // those that fit are written, and none is said to have been
		p.write(oldlenp, le64(0))
		return -eNOMEM
	}
	p.write(oldlenp, le64(fits))
	return 0
}

// sysctlProcArgs answers kern.procargs2, {CTL_KERN, KERN_PROCARGS2, pid}: what a process of the
// caller's user was started with, as ps shows it. It is how many arguments, the program's path
// and zeros after it to a multiple of 8, the arguments, each with its closing zero, and the
// environment, and a zero. A Mac's goes on with what else the kernel put on the new process's
// stack, which is its own and is left out; and a Mac's is the stack as it is now, where this is
// what the process was started with, so one that has written over its arguments is not seen to. Of another user's process, of one that has ended, of
// none and into four bytes or fewer it is EINVAL, as measured; into room for less than all of it
// a Mac answers with a part, which is not done here.
func (p *Process) sysctlProcArgs(mib []int32, oldp, oldlenp uint64, a []uint64) int64 {
	if len(mib) != 1 || oldlenp == 0 {
		return -eINVAL
	}
	about := p.m.process(uint32(mib[0]))
	if about == nil && uint32(mib[0]) == launchdPID && p.uid == 0 {
		p.unimplemented("sysctl kern.procargs2 of launchd, by root", a)
		return -eNOSYS
	}
	if about == nil || p.uid != 0 && about.uid != p.uid {
		return -eINVAL
	}
	block := append(le32(uint32(len(about.argv))), about.path...)
	block = append(block, make([]byte, 8-len(about.path)%8)...)
	for _, s := range slices.Concat(about.argv, about.env) {
		block = append(append(block, s...), 0)
	}
	block = append(block, 0)
	if oldp != 0 {
		room := p.u64(oldlenp)
		if room <= 4 {
			return -eINVAL
		}
		if room < uint64(len(block)) {
			p.unimplemented("sysctl kern.procargs2 into room for less than a process's arguments", a)
			return -eNOSYS
		}
		p.write(oldp, block)
	}
	p.write(oldlenp, le64(uint64(len(block))))
	return 0
}

// sysTaskReadForPID is task_read_for_pid(the caller's task, pid, &port): a port by which a
// process's task can be read. A user is given none but its own process's (EPERM, and a port of
// 0, even for a child of its own, as measured on macOS 27.2.1 of a program with no entitlement
// asking of one that does not allow its task to be had; what one with either is told is not known); a pid there is no process of is
// ESRCH, and a task that is not the caller's EINVAL. Its own is a second name for its task, a
// right to send, the same name for as long as it has it (examples/taskport.c). What root is
// given stops the run.
func sysTaskReadForPID(p *Process, a []uint64) int64 {
	pid := uint32(a[1])
	p.write(a[2], le32(0)) // no port, whatever is answered
	switch {
	case !p.isTask(uint32(a[0])):
		return -eINVAL
	case pid != launchdPID && pid != 0 && p.m.process(pid) == nil:
		return -eSRCH
	case pid == p.pid:
		// A name of its own for its task, the same while the process has it. What only the
		// task's own port may do (changing its memory) is not refused by this one here.
		p.write(a[2], le32(p.kernelSelf(&p.taskRead, kindTask)))
		return 0
	case p.uid == 0:
		p.unimplemented("task_read_for_pid by root", a)
		return -eNOSYS
	}
	return -ePERM
}
