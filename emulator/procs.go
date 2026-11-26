package emulator

import (
	"fmt"
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
	flags              uint32
	nice               int8
	leads              bool // its session
}

func (r procRecord) bytes() []byte {
	b := make([]byte, kinfoProcSize)
	le.PutUint64(b[0:], uint64(r.began.Unix())) // p_starttime
	le.PutUint32(b[8:], uint32(r.began.Nanosecond()/1000))
	le.PutUint32(b[32:], r.flags) // p_flag
	b[36] = r.state               // p_stat
	le.PutUint32(b[40:], r.pid)   // p_pid
	b[242] = byte(r.nice)         // p_nice
	copy(b[243:243+16], r.name)   // p_comm, 16 bytes and a zero
	le.PutUint32(b[392:], r.uid)  // e_pcred: the real and saved user and group
	le.PutUint32(b[396:], r.uid)
	le.PutUint32(b[400:], r.gid)
	le.PutUint32(b[404:], r.gid)
	le.PutUint32(b[420:], r.uid) // e_ucred: the user, and the groups, its own first
	le.PutUint16(b[424:], 1)
	le.PutUint32(b[428:], r.gid)
	le.PutUint32(b[560:], r.parent)   // e_ppid
	le.PutUint32(b[564:], r.group)    // e_pgid
	le.PutUint32(b[572:], 0xFFFFFFFF) // e_tdev: no terminal
	if r.leads {
		le.PutUint32(b[612:], 2) // e_flag: EPROC_SLEADER
	}
	return b
}

// procRecords are the processes there are, the newest first, and launchd last.
func (m *Machine) procRecords() []procRecord {
	var out []procRecord
	for i := len(m.procs) - 1; i >= 0; i-- {
		p := m.procs[i]
		if !p.there() {
			continue
		}
		r := procRecord{pid: p.pid, parent: p.parentPID(), group: p.group(), uid: p.uid, gid: p.gid, name: p.name, began: p.began, state: procRunning, flags: procLP64, nice: int8(p.nice), leads: p.sessionOf() == p.pid}
		if p.exited {
			r.state = procZombie
		}
		if p.execed || p.parent == nil {
			r.flags |= procExeced
		}
		out = append(out, r)
	}
	return append(out, procRecord{pid: launchdPID, group: launchdPID, name: "launchd", began: m.clock.boot, state: procRunning, flags: procLP64 | procExeced, leads: true})
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
