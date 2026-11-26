package emulator

// The ports the kernel itself answers on: a task's own, the host's, a clock's, a thread's and a
// voucher's. Each is a port like any other to the process that holds a right to it, named and
// counted as its other rights are, and each answers what is asked of its kind: a request of
// another kind is KERN_INVALID_ARGUMENT, as a Mac's kernel was measured to answer the time asked
// of a task and what a task holds asked of the host (examples/machports.c).

type portKind int

const (
	kindTask portKind = iota + 1
	kindHost
	kindHostPriv // the host's port as root has it, which a user's process has no right to
	kindClock
	kindThread
	kindVoucher
)

// kernelPort is what such a port stands for: its kind, whose it is, for a task's or a thread's,
// and where it is kept, if it is, to be made anew once the last right to it has gone.
type kernelPort struct {
	kind portKind
	of   *Process
	at   **port
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
	switch {
	case kind == 0, kind == kindHostPriv && p.uid == 0, k.of != nil && k.of != p:
		// What root's host port answers is not known, nor what another process's task does.
		return migReply{}, false
	case kind != k.kind:
		return migCode(kernInvalidArg), true
	}
	routine := migRoutines[id]
	if routine == nil {
		return migReply{}, false
	}
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
	if *at == nil {
		k := kernelPort{kind: kind, at: at}
		if kind == kindTask || kind == kindThread {
			k.of = p
		}
		*at = &port{m: p.m, object: k}
	}
	return p.sendRight(*at)
}

// nameKernel is kernelSelf for a port the kernel names before the program runs, as it does the
// first thread's, which the program is told of as it starts.
func (p *Process) nameKernel(at **port, kind portKind, name uint32) {
	p.kernelSelf(at, kind)
	old := p.named[*at]
	p.names[name], p.named[*at] = p.names[old], name
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
	t.port = p.kernelSelf(&t.self, kindThread)
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
