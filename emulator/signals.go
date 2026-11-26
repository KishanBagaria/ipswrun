package emulator

import (
	"fmt"
	"time"
)

// Signals. One ends the process it is sent to, does nothing, or has a handler of the process's
// run, by what the process has said of it or by what it does unasked. A process is sent one by
// itself or by another (kill), a thread by its process (pthread_kill, which is how abort and
// raise send theirs), a writer by a pipe nobody reads, and a parent by a child that ends.
//
// A handler is run as the kernel runs it: what the thread was doing is put on its stack, or on
// the one it set aside for signals, with what the handler is told, and the thread goes on in
// libc's trampoline, which calls the handler and gives the kernel the frame back (sigreturn) for
// the thread to go on where it was. A signal is taken by a thread when it comes back from the
// kernel, and a thread that waits in a call a signal interrupts is brought back for it. No thread
// is ever taken off the CPU, so one that makes no system call takes no signal either. What a
// Mac's kernel does was measured with examples/signals.c and examples/handlers.c.

const (
	sigAbort = 6
	sigCont  = 19
)

// What a signal does to a process that has said nothing of it, where that is not to end it.
var (
	signalsIgnored = map[uint64]bool{16: true, sigCont: true, 20: true, 23: true, 28: true, 29: true} // URG, CONT, CHLD, IO, WINCH, INFO
	signalsStop    = map[uint64]bool{sigStop: true, 18: true, 21: true, 22: true}                     // STOP, TSTP, TTIN, TTOU
)

// status is how a process ended, as wait4 tells its parent: the signal that ended it, or the
// status it exited with.
func (p *Process) status() uint32 {
	if p.endSignal != 0 {
		return uint32(p.endSignal)
	}
	return uint32(p.exitCode&0xFF) << 8
}

// Signal is the signal that ended the guest, or 0 if it exited, or has not ended.
func (p *Process) Signal() int { return p.endSignal }

// A signal on its way: which, what its handler is told of it, and the thread it is for, if it is
// for one.
type pendingSignal struct {
	sig          uint64
	code, status int32
	pid, uid     uint32
	to           *thread
	addr         uint64 // of a fault: the address that was touched, or the instruction that trapped
	fault        bool
}

// The signals a fault makes, and what their handlers are told of them, as a Mac's kernel says it.
const (
	sigTrap  = 5
	sigBus   = 10
	sigSegv  = 11
	sigAlarm = 14

	codeNotMapped  = 2 // of SIGSEGV: memory that is not there
	codeNoAccess   = 1 // of SIGBUS: memory that may not be touched this way
	codeBreakpoint = 2 // of SIGTRAP: a trap instruction
)

// breakpoint says whether an instruction is a trap a program puts in itself (brk), other than
// the ones a failed pointer check makes, which are another matter to the kernel.
func breakpoint(instruction uint32) bool {
	immediate := instruction >> 5 & 0xFFFF
	return instruction&0xFFE0001F == 0xD4200000 && (immediate < 0xC470 || immediate > 0xC473)
}

// faulted is the thread that has the CPU having made a fault: its handler for the signal is
// run, with where the fault was, and may mend it or go elsewhere. With no handler, or with the
// signal blocked or ignored, which a fault is not held back by, the process ends by the signal.
func (p *Process) faulted(s pendingSignal, pc uint64) {
	t := p.current
	s.to, s.status = t, int32(s.sig)
	handler := p.sigactions[s.sig].handler
	if handler <= sigIgnore || t.sigmask>>(s.sig-1)&1 != 0 {
		p.crash = fmt.Sprintf("%s at %s lr %s\n%s", p.faultText(s), p.where(pc), p.where(p.uc.reg(regLR)), p.Diagnostics())
		p.endSignal, p.exited = int(s.sig), true
		p.note("%s", p.crash)
		return
	}
	p.current.pc = pc
	p.runHandler(s)
}

func (p *Process) faultText(s pendingSignal) string {
	switch s.sig {
	case sigSegv:
		return fmt.Sprintf("a touch of memory that is not there, at %#x", s.addr)
	case sigBus:
		return fmt.Sprintf("a touch of memory that may not be touched that way, at %#x", s.addr)
	}
	return "a trap"
}

// Crash describes the fault that ended the guest, if one did.
func (p *Process) Crash() string { return p.crash }

// intervalTimer is a process's real-time interval timer (setitimer's ITIMER_REAL, and alarm's).
type intervalTimer struct {
	due      time.Time // zero: it is not running
	interval time.Duration
}

// sysSetitimer is setitimer(which, &new, &old): the timer that sends SIGALRM when the time it
// was given has passed, and again after every interval, if it has one. The two that count the
// process's own time on the CPU are read as not running, and starting one is not implemented.
func sysSetitimer(p *Process, a []uint64) int64 {
	which, set, old := a[0], a[1], a[2]
	if which > 2 {
		return -eINVAL
	}
	var next intervalTimer
	if set != 0 {
		b := p.read(set, 32)
		interval, value, ok := timevalOf(b), timevalOf(b[16:]), true
		for _, at := range []int{0, 16} {
			ok = ok && int64(le.Uint64(b[at:])) >= 0 && le.Uint32(b[at+8:]) < 1000000
		}
		switch {
		case !ok:
			return -eINVAL
		case which != 0 && value != 0:
			p.unimplemented("setitimer that starts a timer of the process's own time", a)
			return -eNOSYS
		case value != 0:
			next = intervalTimer{due: p.m.clock.now().Add(value), interval: interval}
		}
	}
	if old != 0 {
		p.write(old, p.timerLeft(which))
	}
	if set != 0 && which == 0 {
		p.itimer = next
	}
	return 0
}

func sysGetitimer(p *Process, a []uint64) int64 {
	if a[0] > 2 {
		return -eINVAL
	}
	p.write(a[1], p.timerLeft(a[0]))
	return 0
}

func timevalOf(b []byte) time.Duration {
	return time.Duration(le.Uint64(b))*time.Second + time.Duration(le.Uint32(b[8:]))*time.Microsecond
}

// timerLeft is a timer as getitimer reads it: its interval, and how long until it is next due.
func (p *Process) timerLeft(which uint64) []byte {
	out := make([]byte, 32)
	if which != 0 || p.itimer.due.IsZero() {
		return out
	}
	timeval := func(at int, d time.Duration) {
		le.PutUint64(out[at:], uint64(d/time.Second))
		le.PutUint32(out[at+8:], uint32(d%time.Second/time.Microsecond))
	}
	timeval(0, p.itimer.interval)
	timeval(16, max(p.itimer.due.Sub(p.m.clock.now()), time.Microsecond))
	return out
}

// ringTimer sends the process SIGALRM if its timer is due, and reports when it is next.
func (p *Process) ringTimer(now time.Time) time.Time {
	t := &p.itimer
	if t.due.IsZero() || t.due.After(now) {
		return t.due
	}
	if t.interval > 0 {
		t.due = t.due.Add((now.Sub(t.due)/t.interval + 1) * t.interval)
	} else {
		t.due = time.Time{}
	}
	p.sendSignal(pendingSignal{sig: sigAlarm, status: sigAlarm})
	return t.due
}

const (
	// Flags of a handler (SA_…).
	saOnStack   = 0x1
	saRestart   = 0x2
	saResetHand = 0x4
	saNoDefer   = 0x10
	saSigInfo   = 0x40

	ssOnStack = 0x1 // SS_ONSTACK

	// The frame a handler is run with: what it is told, the context it interrupted, and the
	// machine's state in that context, one after another.
	sigInfoSize     = 104
	sigContextSize  = 56
	sigMachineSize  = 816
	sigRedZone      = 128        // below the stack pointer, which a function may use without moving it
	sigStyleHandler = 1          // UC_TRAD: the handler takes the signal's number
	sigStyleInfo    = 30         // UC_FLAVOR: and what it is told, and the context
	sigSyscallESR   = 0x56000080 // the exception the thread last took: the system call it comes back from
	sigNoPtrauth    = 0x1        // of the thread state: its pointers are not signed (dyld is told so, too)
	sigSetAltStack  = 0x40000000 // UC_SET_ALT_STACK
	sigResetStack   = 0x80000000 // UC_RESET_ALT_STACK
	sigToken        = 0x5169_7265_7475_726E

	codeChildExited = 1 // CLD_EXITED
	codeChildKilled = 2 // CLD_KILLED
	sigChild        = 20
)

// sysKill is kill(pid, signal): to a process, by its pid.
func sysKill(p *Process, a []uint64) int64 {
	pid, sig := int32(a[0]), a[1]
	if sig >= numSignals {
		return -eINVAL
	}
	switch {
	case pid == -1:
		p.unimplemented("kill of every process", a)
		return -eSRCH
	case pid == 0:
		return p.killGroup(p.group(), sig)
	case pid < 0:
		return p.killGroup(uint32(-pid), sig)
	}
	target := p.m.process(uint32(pid))
	switch {
	case target != nil:
	case uint32(pid) == launchdPID && p.uid != 0:
		return -ePERM
	case uint32(pid) == launchdPID:
		p.unimplemented("a signal to launchd from root", a)
		return -ePERM
	default:
		// One that has ended and that its parent has not waited for is still there to be sent
		// a signal, which does nothing to it.
		if ended := p.m.ended(uint32(pid)); ended != nil && ended.unwaited() {
			return 0
		}
		return -eSRCH
	}
	if !p.signals(target, sig) {
		return -ePERM
	}
	return 0
}

// unwaited is whether a process has ended and its parent is still to wait for it. Once the parent
// has ended too there is no one to: launchd waits for it then, and it is gone.
func (p *Process) unwaited() bool {
	return p.exited && p.parent != nil && !p.parent.exited && !p.reaped
}

// signals sends a process a signal, or with signal 0 none, and says whether it may: its own
// user's process, or any if it is root's.
func (p *Process) signals(q *Process, sig uint64) bool {
	if p.uid != 0 && q.uid != p.uid {
		return false
	}
	if sig != 0 {
		q.sendSignal(pendingSignal{sig: sig, status: int32(sig), pid: p.pid, uid: p.uid})
	}
	return true
}

// killGroup sends a signal to each process of a group, the sender too if it is in it, or with
// signal 0 only asks whether it could. A group no process is in is ESRCH. One whose processes
// are all another user's, or have ended and are only still to be waited for, is EPERM: they are
// there, and none can be sent a signal.
func (p *Process) killGroup(group uint32, sig uint64) int64 {
	found, sent := false, false
	for _, q := range p.m.inGroup(group) {
		found = true
		if !q.exited && p.signals(q, sig) {
			sent = true
		}
	}
	switch {
	case sent:
		return 0
	case found:
		return -ePERM
	}
	return -eSRCH
}

// inGroup are the processes of a group: those that run, and those that have ended and are still
// to be waited for, which keep the group there.
func (m *Machine) inGroup(group uint32) []*Process {
	var in []*Process
	for _, q := range m.procs {
		if q.group() == group && (!q.exited || q.unwaited()) {
			in = append(in, q)
		}
	}
	return in
}

// sysPthreadKill is __pthread_kill(thread, signal): to a thread of the process itself.
func sysPthreadKill(p *Process, a []uint64) int64 {
	sig := a[1]
	var to *thread
	for _, t := range p.threads {
		if uint64(t.port) == a[0] && t.state != dead {
			to = t
		}
	}
	switch {
	case sig >= numSignals:
		return -eINVAL
	case to == nil:
		return -eSRCH
	case sig != 0:
		p.sendSignal(pendingSignal{sig: sig, status: int32(sig), pid: p.pid, uid: p.uid, to: to})
	}
	return 0
}

func sysSigpending(p *Process, a []uint64) int64 {
	var set uint32
	for _, s := range p.pending {
		if s.forThread(p.current) {
			set |= 1 << (s.sig - 1)
		}
	}
	p.write(a[0], le32(set))
	return 0
}

// forThread says whether a signal that waits is one a thread could be given: the process's, or
// the thread's own.
func (s pendingSignal) forThread(t *thread) bool { return s.to == nil || s.to == t }

// takes says whether a thread can take a signal now: it is for it, and it does not block it.
func (s pendingSignal) takes(t *thread) bool {
	return t.state != dead && s.forThread(t) && (s.sig == sigKill || t.sigmask>>(s.sig-1)&1 == 0)
}

// sendSignal sends the process a signal. It ends the process or does nothing at once, if that is
// what it does. One with a handler, or that every thread it could go to blocks, waits for a
// thread to take it, and a thread that waits in a call a signal interrupts is brought back.
func (p *Process) sendSignal(s pendingSignal) {
	if p.exited {
		return
	}
	p.signalled(s.sig) // whoever watches for it with a kqueue is told, whatever it then does
	handler := p.sigactions[s.sig].handler
	switch {
	case s.sig == sigKill:
		p.endBySignal(sigKill)
		return
	case signalsStop[s.sig] && (handler == 0 || s.sig == sigStop):
		p.unimplemented(fmt.Sprintf("signal %d, which stops a process until it is continued", s.sig), nil)
		return
	case handler == sigIgnore, handler == 0 && signalsIgnored[s.sig]:
		return
	}
	var takers []*thread
	for _, t := range p.threads {
		if s.takes(t) {
			takers = append(takers, t)
		}
	}
	if handler == 0 && len(takers) > 0 {
		p.endBySignal(int(s.sig))
		return
	}
	p.pending = append(p.pending, s)
	if containsThread(takers, func(t *thread) bool { return t.state == runnable }) {
		return // taken when that thread next comes back from the kernel
	}
	for _, t := range takers {
		if p.interrupt4(t, s.sig) {
			return
		}
	}
}

// interrupt4 brings a thread back from a call that a signal interrupts, to take the signal: the
// call fails as interrupted, or is made again once the handler has run, if the handler says so
// and the call is one that can be. False for a thread that waits in no such call: for a lock, a
// Mach message or work, which it comes back from in its own time.
func (p *Process) interrupt4(t *thread, sig uint64) bool {
	if t.state != waiting {
		return false
	}
	again := p.sigactions[sig].flags&saRestart != 0
	switch t.kind {
	case waitPipe:
		again = again && t.pipeOp != nil && t.pipeOp.done == 0
		if t.pipeOp != nil && t.pipeOp.done > 0 { // what went in before the signal is the call's answer
			t.answer = &result{x0: uint64(t.pipeOp.done)}
			again = false
		}
	case waitChild, waitLock:
	case waitSelect, waitKevent, waitSleep:
		again = false
	case waitSemaphore:
		if !t.sleeping { // a semaphore of Mach's, which is not a call a signal's handler is run from here
			return false
		}
		again = false
	case waitSignal:
		if t.suspended == nil { // sigwait, which no handler ends
			return false
		}
		again = false
	default:
		return false
	}
	t.pipeOp, t.selecting, t.polling, t.kevent, t.waiting4 = nil, nil, nil, nil, nil
	switch {
	case again:
		// As it was before the call: its first argument back where the answer went, and the
		// instruction that makes the call next.
		t.pc -= 4
		if t == p.current {
			p.setX(0, t.arg0)
			p.uc.setReg(regPC, t.pc)
		} else {
			p.uc.setContextReg(t.ctx, regX0, t.arg0)
			p.uc.setContextReg(t.ctx, regPC, t.pc)
		}
		t.answer = nil
	case t.answer == nil:
		t.answer = &result{x0: eINTR, carry: true}
	}
	t.wake()
	return true
}

func containsThread(threads []*thread, f func(*thread) bool) bool {
	for _, t := range threads {
		if f(t) {
			return true
		}
	}
	return false
}

// takeSignals has the thread that is coming back from the kernel take the signals that wait
// for it: each ends the process, or has its handler run, the last one taken first.
func (p *Process) takeSignals() {
	t := p.current
	for !p.exited && t.state == runnable {
		at := -1
		for i, s := range p.pending {
			if s.takes(t) {
				at = i
				break
			}
		}
		if at < 0 {
			return
		}
		s := p.pending[at]
		p.pending = append(p.pending[:at:at], p.pending[at+1:]...)
		switch handler := p.sigactions[s.sig].handler; {
		case handler == sigIgnore, handler == 0 && signalsIgnored[s.sig]:
		case handler == 0:
			p.endBySignal(int(s.sig))
		default:
			p.runHandler(s)
		}
	}
}

// runHandler puts what the thread was doing on its stack, with what the handler is told, and
// sends the thread to the trampoline that calls the handler.
func (p *Process) runHandler(s pendingSignal) {
	t, act := p.current, p.sigactions[s.sig]
	sp, pc, wasOnStack := p.uc.reg(regSP), p.uc.reg(regPC), t.onstack
	interrupted := t.sigmask
	if t.suspended != nil { // sigsuspend's mask is for the wait: the handler returns to the one before it
		interrupted, t.suspended = *t.suspended, nil
	}
	var stackSize uint64
	if act.flags&saOnStack != 0 && t.altstack[1] != 0 && !t.onstack {
		sp, stackSize, t.onstack = t.altstack[0]+t.altstack[1], t.altstack[1], true
	}
	base := (sp - sigRedZone - sigInfoSize - sigContextSize - sigMachineSize) &^ 15
	info, context, machine := base, base+sigInfoSize, base+sigInfoSize+sigContextSize

	told := make([]byte, sigInfoSize)
	copy(told, le32(uint32(s.sig), 0, uint32(s.code), s.pid, s.uid, uint32(s.status)))
	le.PutUint64(told[24:], pc)
	far, esr := uint64(0), uint64(sigSyscallESR)
	if s.fault { // where it was; what the exception was, as the chip words it, is left out
		le.PutUint64(told[24:], s.addr)
		far, esr = s.addr, 0
		if s.sig == sigTrap {
			far = 0
		}
	}
	stackFlags := uint32(0)
	if wasOnStack {
		stackFlags = ssOnStack
	}
	uc := append(le32(uint32(truth(wasOnStack)), interrupted), le64(sp, stackSize, uint64(stackFlags), 0, sigMachineSize, machine)...)
	state := le64(far, esr) // the address of a fault, and the exception the thread last took
	for i := uint32(0); i < 31; i++ {
		state = append(state, le64(p.xr(i))...)
	}
	state = append(state, le64(p.uc.reg(regSP), pc)...)
	state = append(state, le32(uint32(p.uc.reg(regNZCV)), sigNoPtrauth)...)
	vector := make([]byte, 16)
	for i := 0; i < 32; i++ {
		p.uc.regBytes(regV0+i, vector)
		state = append(state, vector...)
	}
	state = append(state, le32(uint32(p.uc.reg(regFPSR)), uint32(p.uc.reg(regFPCR)), 0, 0)...)
	p.write(info, append(append(told, uc...), state...))

	t.sigmask |= act.mask
	if act.flags&saNoDefer == 0 {
		t.sigmask |= 1 << (s.sig - 1)
	}
	if act.flags&saResetHand != 0 {
		p.sigactions[s.sig] = sigAction{}
	}
	style := uint64(sigStyleHandler)
	if act.flags&saSigInfo != 0 {
		style = sigStyleInfo
	}
	for i, v := range []uint64{act.handler, style, s.sig, info, context, context ^ sigToken} {
		p.setX(i, v)
	}
	p.uc.setReg(regSP, base)
	p.uc.setReg(regLR, 0)
	p.jump(act.tramp)
	p.note("  signal %d: its handler at %#x is run, with its frame at %#x", s.sig, act.handler, base)
}

// jump sends the thread that has the CPU somewhere else than where it was going.
func (p *Process) jump(pc uint64) {
	p.uc.setReg(regPC, pc)
	p.current.pc = pc
	p.resume = true
	p.uc.stop()
}

// sysSigreturn is sigreturn(context, style, token): a handler has returned, and the thread goes
// on with what it was doing, as the context has it, which the handler may have changed. With no
// context it only says whether the thread is on its stack for signals (longjmp says so).
func sysSigreturn(p *Process, a []uint64) int64 {
	t := p.current
	switch style := uint32(a[1]); {
	case a[0] == 0 && style == sigResetStack:
		t.onstack = false
		return 0
	case a[0] == 0 && style == sigSetAltStack:
		t.onstack = true
		return 0
	case a[0] == 0 || a[2] != a[0]^sigToken:
		p.unimplemented("sigreturn with a context the kernel did not make", a)
		return -eINVAL
	}
	uc := p.read(a[0], sigContextSize)
	if le.Uint64(uc[40:]) != sigMachineSize {
		p.unimplemented("sigreturn with a machine state of another size", a)
		return -eINVAL
	}
	state := p.read(le.Uint64(uc[48:]), sigMachineSize)
	t.onstack, t.sigmask = le.Uint32(uc) != 0, le.Uint32(uc[4:])&^(1<<(sigKill-1)|1<<(sigStop-1))
	regs := state[16:]
	for i := uint32(0); i < 31; i++ {
		p.setXR(i, le.Uint64(regs[8*i:]))
	}
	flags := uint64(le.Uint32(regs[264:]))
	p.uc.setReg(regSP, le.Uint64(regs[248:]))
	p.uc.setReg(regNZCV, flags)
	vectors := regs[272:]
	for i := 0; i < 32; i++ {
		p.uc.setRegBytes(regV0+i, vectors[16*i:16*i+16])
	}
	p.uc.setReg(regFPSR, uint64(le.Uint32(vectors[512:])))
	p.uc.setReg(regFPCR, uint64(le.Uint32(vectors[516:])))
	// Where the answer of a call goes is the context's too: it is put back after this one's.
	t.answer = &result{x0: le.Uint64(regs), carry: flags>>29&1 != 0}
	p.jump(le.Uint64(regs[256:]))
	return 0
}

// endBySignal ends a process as a signal does: at once if it is the one that has the CPU, and
// otherwise before anything else runs.
func (p *Process) endBySignal(sig int) {
	if p.exited {
		return
	}
	p.endSignal = sig
	if p == p.m.running && p.m.stepping {
		p.exited = true // as exit does: the machine sees to the rest when the call returns
		return
	}
	p.m.deferred = append(p.m.deferred, func() {
		if !p.exited {
			p.m.exit(p, fmt.Sprintf("by signal %d", sig))
		}
	})
}

// sysPthreadCanceled is __pthread_canceled(what): a thread asking whether it was cancelled (0),
// which libc does when a call of its is interrupted, or saying that it may be (1) or may not (2).
// No thread is ever cancelled here, as nothing marks one (__pthread_markcancel is not there).
func sysPthreadCanceled(p *Process, a []uint64) int64 {
	if a[0] == 1 || a[0] == 2 {
		return 0
	}
	return -eINVAL
}
