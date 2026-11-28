package emulator

import (
	"cmp"
	"slices"
)

// What a task has the kernel keep for it: the ports it and what it starts are handed when they
// ask (its special ports), the stretches of its code a thread is put back to the start of when
// another has to know that none is inside (restartable ranges, which the Objective-C runtime
// has for the code that reads a class's cache of methods), and whether memory it has no use for
// now may be emptied (purgeable memory). As measured on macOS 27.2.1
// (tests/kernel/mach/taskkeeps.c), but where it says the source.

const (
	kernNoAccess         = 8
	kernResourceShortage = 6
	kernDenied           = 53
)

// specialPort is one a task has set: none is kept too, for it is then no port that is given.
type specialPort struct{ port *port }

// migTaskGetSpecialPort is task_get_special_port(task, which): the host's port (2), the task as
// those who may only name it have it (3), where it finds launchd (4, unless it was set to
// another), and the one a debugger is asked through (10) if it was set. The task's own port to
// rule it by (1) is not given this way (KERN_DENIED), and a number that is none is
// KERN_INVALID_ARGUMENT. The ports to look into the task and to read it (5 and 6), and those a
// Mac's own daemons set for it (9, and 10 where the task did not), are not answered here.
func migTaskGetSpecialPort(p *Process, body []byte) migReply {
	which := int32(le.Uint32(body[32:]))
	p.note("  task_get_special_port %d", which)
	if set, ok := p.special[which]; ok {
		if set.port == nil {
			return migPort(0)
		}
		return migPort(p.sendRight(set.port))
	}
	switch which {
	case 1:
		return migCode(kernDenied)
	case 2:
		return migPort(p.kernelSelf(&p.m.host, kindHost))
	case 3:
		return migPort(p.kernelRightTo(&p.taskName, kernelPort{kind: kindTaskName, of: p}))
	case 4:
		return migPort(bootstrapPort)
	case 5, 6, 9, 10:
		return migReply{none: true}
	}
	return migCode(kernInvalidArg)
}

// migTaskSetSpecialPort is task_set_special_port(task, which, port): where it finds launchd (4)
// and the debugger's (10) may be set, to a port it may send to or to none, and are then what it
// is given back and what a copy of it has. Those the kernel keeps to itself (1, 2 and 9) are
// KERN_NO_ACCESS, and the rest KERN_INVALID_ARGUMENT. A name that is no right to send is refused
// as the message is sent.
func migTaskSetSpecialPort(p *Process, body []byte) migReply {
	if len(body) < 52 || le.Uint32(body[24:]) != 1 {
		return migReply{none: true}
	}
	name, which := le.Uint32(body[28:]), int32(le.Uint32(body[48:]))
	var to *port
	if name != 0 && name != bootstrapPort {
		r := p.names[name]
		if r == nil || r.sends == 0 || r.port.dead {
			return migReply{refused: sendInvalidRight}
		}
		to = r.port
	}
	switch which {
	case 1, 2, 9:
		return migCode(kernNoAccess)
	case 4, 10:
	default:
		return migCode(kernInvalidArg)
	}
	if p.special == nil {
		p.special = map[int32]specialPort{}
	}
	if p.special[which] = (specialPort{to}); which == 4 && name == bootstrapPort {
		delete(p.special, which) // launchd's again, which is what it has when nothing was set
	}
	return migCode(0)
}

// restartable is a stretch of a task's code and where in it a thread found inside is put back.
type restartable struct{ from, to, back uint64 }

// migRestartableRegister is task_restartable_ranges_register(task, ranges): once for a task,
// and while it has one thread (KERN_NOT_SUPPORTED after: every program has it so, the
// Objective-C runtime having registered its own as the program started). In the order of the
// source: no ranges, one longer than a page or put back further than that, one with flags, one
// that wraps or runs into the next are KERN_INVALID_ARGUMENT, and more than 64
// KERN_RESOURCE_SHORTAGE (the last from the source).
func migRestartableRegister(p *Process, body []byte) migReply {
	const size, most, far = 16, 64, 4096
	if len(body) < 36 {
		return migReply{none: true}
	}
	count := uint64(le.Uint32(body[32:]))
	if uint64(len(body)) < 36+size*count {
		return migReply{none: true}
	}
	var ranges []restartable
	for i := range count {
		r := body[36+size*i:]
		at, length, back, flags := le.Uint64(r), uint64(le.Uint16(r[8:])), uint64(le.Uint16(r[10:])), le.Uint32(r[12:])
		if length > far || back > far || flags != 0 || at+length < at || at+back < at {
			return migCode(kernInvalidArg)
		}
		ranges = append(ranges, restartable{at, at + length, at + back})
	}
	slices.SortFunc(ranges, func(a, b restartable) int { return cmp.Or(cmp.Compare(a.from, b.from), cmp.Compare(a.to, b.to)) })
	for i := range ranges {
		if i+1 < len(ranges) && ranges[i].to > ranges[i+1].from {
			return migCode(kernInvalidArg)
		}
	}
	switch {
	case count == 0:
		return migCode(kernInvalidArg)
	case count > most:
		return migCode(kernResourceShortage)
	case len(p.threads) > 1 || p.restartable != nil:
		return migCode(kernNotSupported)
	}
	p.restartable = ranges
	return migCode(0)
}

// migRestartableSynchronize is task_restartable_ranges_synchronize(task): every other thread
// that is inside one of the ranges is put back to where the range says, so that the caller
// knows none is still in the middle of one. Threads take turns here only where they ask the
// kernel something, which no such range does, so none is ever found inside: the putting back
// is as the source has it and has not been seen to happen.
func migRestartableSynchronize(p *Process, body []byte) migReply {
	for _, t := range p.threads {
		if t == p.current || t.ctx == 0 {
			continue
		}
		pc := p.uc.contextReg(t.ctx, regPC)
		for _, r := range p.restartable {
			if pc >= r.from && pc < r.to {
				p.uc.setContextReg(t.ctx, regPC, r.back)
			}
		}
	}
	return migCode(0)
}

// machPurgableControl is vm_purgable_control(task, address, what, &state), the trap: of memory
// made to be emptied (vm_allocate with VM_FLAGS_PURGABLE) it says whether it may be (0: no, 1:
// yes) or sets that and says what it was. Memory is never short here, so what may be emptied
// never is. Of other memory it is KERN_INVALID_ARGUMENT, as of a thing to do that there is not,
// and of memory that is not there KERN_INVALID_ADDRESS.
func machPurgableControl(p *Process, a []uint64) int64 {
	const set, get = 0, 1
	switch a[2] {
	case set, get:
	case 2, 3: // to empty everything, and the kernel's own setting: not measured
		p.unimplemented("vm_purgable_control to empty all, or as the kernel sets it", a)
		return kernInvalidArg
	default:
		return kernInvalidArg
	}
	r := p.findRegion(a[1] & tbiMask)
	switch {
	case r == nil:
		return kernInvalidAddress
	case !r.purgeable:
		return kernInvalidArg
	}
	was := r.volatile
	if a[2] == set {
		r.volatile = p.u32(a[3]) & 3
	}
	p.write(a[3], le32(was))
	return 0
}
