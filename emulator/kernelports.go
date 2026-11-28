package emulator

import (
	"slices"
	"time"
)

// The ports the kernel itself answers on: a task's own, the host's, a clock's, a thread's and a
// voucher's. Each is a port like any other to the process that holds a right to it, named and
// counted as its other rights are, and each answers what is asked of its kind: a request of
// another kind is KERN_INVALID_ARGUMENT, as a Mac's kernel was measured to answer the time asked
// of a task and what a task holds asked of the host (tests/kernel/mach/machports.c).

type portKind int

const (
	kindTask portKind = iota + 1
	kindHost
	kindHostPriv // the host's port as root has it, which a user's process has no right to
	kindClock
	kindThread
	kindVoucher
	kindTaskName // a task's port to those who may only name it (task_name_for_pid)
)

// kernelPort is what such a port stands for: its kind, whose it is, for a task's or a thread's,
// and where it is kept, if it is, to be made anew once the last right to it has gone.
type kernelPort struct {
	kind   portKind
	of     *Process
	at     **port
	thread *thread // of a thread's: which
}

// routineKind is the kind of port a request is for, by the subsystem its number is of.
func routineKind(id uint32) portKind {
	switch id / 100 {
	case 2:
		return kindHost
	case 4:
		return kindHostPriv
	case 10:
		return kindClock
	case 32, 34, 38, 48, 80: // a task's ports, the task itself, its memory, and its restartable ranges
		return kindTask
	case 36:
		return kindThread
	}
	return 0
}

func (k kernelPort) request(p *Process, id uint32, body []byte) (migReply, bool) {
	kind := routineKind(id)
	if k.kind == kindTaskName {
		// What a task says to one who may only name it: of the process's own, what it is in
		// short (task_info's TASK_BASIC_INFO_64), as measured. Any other question, and any
		// of another process, is not answered here.
		const taskInfo, basic64 = 3405, 18 // on this kind of CPU the 64-bit record is TASK_BASIC_INFO_64_2
		if k.of != p || id != taskInfo || len(body) < 36 || le.Uint32(body[32:]) != basic64 {
			return migReply{}, false
		}
		return migRoutines[id](p, body), true
	}
	if id == 412 && k.kind == kindHost && len(body) >= 40 {
		// host_get_special_port(host, node, which): the ports the host keeps for root. A user is
		// refused them all (KERN_INVALID_ARGUMENT), as measured. Root gets the host's port for
		// which 1 and 2, as measured in a virtual machine (macOS 27.2): there the second is the
		// very port root's mach_host_self names and the first another, and here both are the
		// one port the host has. Of none set (4) it is no port and no error, and of 0
		// KERN_INVALID_ARGUMENT, as measured. The rest (the one IOKit starts from, and those
		// daemons set) are not answered here.
		switch which := le.Uint32(body[36:]); {
		case p.uid != 0 || which == 0:
			return migCode(kernInvalidArg), true
		case which == 1 || which == 2:
			return migPort(p.kernelSelf(&p.m.host, kindHost)), true
		case which == 4:
			return migPort(0), true
		}
		return migReply{}, false
	}
	switch {
	case kind == 0, kind == kindHostPriv && p.uid == 0, k.of != nil && k.of != p:
		// What root's host port answers is not known, nor what another process's task does.
		return migReply{}, false
	case kind != k.kind && (id == 8000 || id == 8001):
		return migCode(kernFailure), true // restartable ranges are the asking task's own, as measured
	case kind != k.kind:
		return migCode(kernInvalidArg), true
	}
	routine := migRoutines[id]
	if routine == nil {
		return migReply{}, false
	}
	p.target = k.thread
	return routine(p, body), true
}

func (k kernelPort) released() {
	if k.at != nil {
		*k.at = nil
	}
}

// kernelSelf gives the process one more right to send to a port of the kernel's kept at a
// place, under the name it already has for it if it has one.
func (p *Process) kernelSelf(at **port, kind portKind) uint32 {
	k := kernelPort{kind: kind}
	if kind == kindTask {
		k.of = p
	}
	return p.kernelRightTo(at, k)
}

// kernelRightTo is kernelSelf for what the port is to stand for, which is made if it is not there.
func (p *Process) kernelRightTo(at **port, k kernelPort) uint32 {
	if *at == nil {
		k.at = at
		*at = &port{m: p.m, object: k}
	}
	return p.sendRight(*at)
}

// threadSelf gives the process one more right to a thread's port, which stands for that thread.
func (p *Process) threadSelf(t *thread) uint32 {
	return p.kernelRightTo(&t.self, kernelPort{kind: kindThread, of: p, thread: t})
}

// nameThread is threadSelf for the first thread, whose port the kernel names before the program
// runs and tells it of as it starts.
func (p *Process) nameThread(t *thread, name uint32) {
	old := p.threadSelf(t)
	p.names[name], p.named[t.self] = p.names[old], name
	delete(p.names, old)
}

// isTask is whether a name is the process's for its own task.
func (p *Process) isTask(name uint32) bool {
	r := p.names[name]
	return r != nil && p.task != nil && r.port == p.task
}

func machTaskSelf(p *Process, a []uint64) int64 { return int64(p.kernelSelf(&p.task, kindTask)) }
func machHostSelf(p *Process, a []uint64) int64 { return int64(p.kernelSelf(&p.m.host, kindHost)) }

// machThreadSelf is the thread's name for itself, held once more each time it is asked for.
func machThreadSelf(p *Process, a []uint64) int64 {
	t := p.current
	t.port = p.threadSelf(t)
	return int64(t.port)
}

// migPortGetRefs is mach_port_get_refs(task, name, right, &refs): how often a name is held as a
// right of one kind, which is none for a kind it is not. Of a name that is nothing it is
// KERN_INVALID_NAME, as measured; of one whose port is gone, which was not, it stops.
func migPortGetRefs(p *Process, body []byte) migReply {
	name, kind := le.Uint32(body[32:]), le.Uint32(body[36:])
	r := p.held(name, "mach_port_get_refs")
	held := func(is bool) migReply {
		if is {
			return migData(le32(1))
		}
		return migData(le32(0))
	}
	switch {
	case r != nil && r.port.dead, kind > rightDeadName:
		p.unimplemented("mach_port_get_refs of a name whose port is gone, or of a kind of right that was not measured", nil)
		return migCode(kernInvalidName)
	case r == nil:
		return migCode(kernInvalidName)
	case kind == rightSend:
		return migData(le32(uint32(r.sends)))
	case kind == rightReceive:
		return held(r.receive && !r.port.set)
	case kind == rightSendOnce:
		return held(r.once)
	case kind == rightPortSet:
		return held(r.port.set)
	}
	return migData(le32(0)) // a dead name, which a port that is there is not
}

// The three ports a task keeps for whoever asks, as a shell keeps the port it found launchd by
// (mach_ports_register and mach_ports_lookup), as the kernel's source has them and as measured
// (tests/kernel/mach/registered.c): what is kept replaces what was, and is given back as rights to send,
// one more each time, under the names the task has for them. A copy of the process is given
// its parent's. A library asks in one of two forms, and is answered in the one it asked in:
// three ports in the message itself, or (before macOS 14 and iOS 17) an array of up to three
// beside the message.

const (
	sendInvalidRight = 0x1000000a
	portsKept        = 3                       // TASK_PORT_REGISTER_MAX
	portArray        = 2                       // the kind of descriptor that is an array of ports beside the message
	lookupInMessage  = 24 + 4 + 3*12 + 8       // what the library that asks for three ports in the message receives into: a header, a count, three ports, a trailer
	lookupBeside     = 24 + 4 + 16 + 8 + 4 + 8 // and the one that asks for an array: a header, a count, the array, how its numbers are written, how many, a trailer
)

// migPortsRegister keeps three ports, each a right the message copies or moves, or none.
func migPortsRegister(p *Process, body []byte) migReply {
	const each = 12
	var names [portsKept]uint32
	how := uint32(copySend)
	switch {
	case len(body) >= 28+portsKept*each && le.Uint32(body)&bitsComplex != 0 && le.Uint32(body[24:]) == portsKept && body[28+11] == 0 && body[28+each+11] == 0 && body[28+2*each+11] == 0:
		for i := range names {
			names[i] = le.Uint32(body[28+i*each:])
		}
		how = uint32(body[28+10]) // the library gives all three the same way
		if uint32(body[28+each+10]) != how || uint32(body[28+2*each+10]) != how {
			return migReply{none: true}
		}
	case len(body) >= 28+16 && le.Uint32(body)&bitsComplex != 0 && le.Uint32(body[24:]) == 1 && body[28+11] == portArray:
		if le.Uint32(body[28+12:]) > portsKept { // more than it keeps, which the newer library refuses before it asks, the same way
			return migCode(kernInvalidArg)
		}
		how = uint32(body[28+10])
		for i, raw := 0, p.read(le.Uint64(body[28:]), 4*uint64(le.Uint32(body[28+12:]))); i < len(raw)/4; i++ {
			names[i] = le.Uint32(raw[4*i:])
		}
	default:
		return migReply{none: true}
	}
	// What may be kept is none, the port launchd is found by, or a right to send that the task
	// has. A port it can only receive from is no right to send (measured). What was not
	// measured stops: a name that is nothing, a right given another way, and a port of the
	// kernel's own, which the source refuses a task's and a thread's.
	for _, name := range names {
		r := p.names[name]
		kernels := false
		if r != nil {
			_, kernels = r.port.object.(kernelPort)
		}
		switch {
		case r == nil && (name == 0 || name == bootstrapPort):
		case r == nil, kernels, how != copySend && how != moveSend:
			return migReply{none: true}
		case r.sends == 0:
			return migReply{refused: sendInvalidRight}
		}
	}
	for i, name := range names {
		old := p.registered[i]
		p.registered[i] = p.takeIn(name, how)
		if old.port != nil {
			release(old.port)
		}
	}
	return migCode(0)
}

// migPortsLookup gives what is kept, in the form the caller has room for.
func migPortsLookup(p *Process, body []byte) migReply {
	if p.asked.rcvSize != lookupInMessage && p.asked.rcvSize != lookupBeside {
		return migReply{none: true}
	}
	var names []byte
	for _, c := range p.registered {
		name := c.name // of what is no port here (the port launchd is found by), its number
		if c.port != nil {
			c.port.sends++
			name = p.giveOut(carried{port: c.port, disposition: moveSend}, nil)
		}
		names = append(names, le32(name)...)
	}
	if p.asked.rcvSize == lookupInMessage {
		reply := le32(portsKept)
		for i := range portsKept {
			reply = append(reply, portDescriptor(le.Uint32(names[4*i:]), moveSend)...)
		}
		return migReply{body: reply, complex: true}
	}
	// One array of ports beside the message, which the caller is given memory for, and how many.
	reply := slices.Concat(le32(1), le64(0), []byte{0, 1, moveSend, portArray}, le32(portsKept), ndr, le32(portsKept))
	return migReply{body: reply, complex: true, memory: []outOfLine{{at: 24 + 4, data: names}}}
}

// forgetRegistered gives up what a process that has ended kept.
func (p *Process) forgetRegistered() {
	for i, c := range p.registered {
		if p.registered[i] = (carried{}); c.port != nil {
			release(c.port)
		}
	}
}

// inheritRegistered gives a copy of a process what its parent keeps.
func (p *Process) inheritRegistered(from *Process) {
	p.registered = from.registered
	for _, c := range p.registered {
		if c.port != nil {
			c.port.sends++
		}
	}
}

// migThreadInfo is thread_info(thread, kind, &info, &count), of a thread of the process: in
// short (THREAD_BASIC_INFO) and at length (THREAD_EXTENDED_INFO), as the kernel's source has
// them and as measured (tests/kernel/mach/threadinfo.c). The time is what the process used while the
// thread ran; a thread that waits says so (state 3, and the flag that it is swapped out) and
// any other is running (1); it is scheduled as a Mac schedules the threads of a program
// started plainly, by sharing time (policy 1) at priority 31 of at most 63, which is said and
// is not how threads take turns here. Its share of the processor, which a Mac works out over
// time, is not worked out here and is said to be none. Room for less than the record, and a
// kind there is none of, are KERN_INVALID_ARGUMENT; the other kinds stop.
func migThreadInfo(p *Process, body []byte) migReply {
	const basic, identifier, extended, basicWords, extendedWords = 3, 4, 5, 10, 28
	const running, waitingState, swapped, timeshare, priority, highest = 1, 3, 1, 1, 31, 63
	if len(body) < 40 {
		return migReply{none: true}
	}
	t, kind, room := p.target, le.Uint32(body[32:]), le.Uint32(body[36:])
	state, flags := uint32(running), uint32(0)
	switch {
	case t.state == waiting && t.kind == waitCond: // as measured of one that waits for a condition
		state, flags = waitingState, swapped
	case t != p.current && t.state != runnable:
		return migReply{none: true} // waiting for something else, which was not measured
	}
	p.charge()
	micros := func(d time.Duration) uint64 { return uint64(d / time.Microsecond * time.Microsecond) } // the kernel has them in millionths
	switch {
	case kind == basic && room >= basicWords:
		return counted(slices.Concat(timeValue(t.user), timeValue(t.system), le32(0, timeshare, state, flags, 0, 0)))
	case kind == extended && room >= extendedWords:
		return counted(slices.Concat(le64(micros(t.user), micros(t.system)), le32(0, timeshare, state, flags, 0, priority, priority, highest), fixed(t.name, 64)))
	case kind == basic, kind == extended:
		return migCode(kernInvalidArg)
	case kind != identifier && kind != 6 && kind != 10 && kind != 11 && kind != 12: // the source lists no other
		return migCode(kernInvalidArg)
	}
	return migReply{none: true}
}
