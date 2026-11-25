package emulator

import "fmt"

// Signals, as far as they go here: one that ends the process it is sent to, and one that does
// nothing, by what the process has said of it or by what it does unasked. A process is sent one
// by itself or by another (kill), a thread by its process (pthread_kill, which is how abort and
// raise send theirs), and a writer by a pipe nobody reads. A signal a process has a handler for
// is not delivered: no handler is ever run, and sending one stops the run as unimplemented. What
// a Mac's kernel does was measured with examples/signals.c.

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

// sysKill is kill(pid, signal): to a process, by its pid.
func sysKill(p *Process, a []uint64) int64 {
	pid, sig := int32(a[0]), a[1]
	if sig >= numSignals {
		return -eINVAL
	}
	if pid <= 0 {
		p.unimplemented("kill of a process group, or of every process", a)
		return -eSRCH
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
		if ended := p.m.ended(uint32(pid)); ended != nil && ended.parent != nil && !ended.reaped {
			return 0
		}
		return -eSRCH
	}
	if p.uid != 0 && target.uid != p.uid {
		return -ePERM
	}
	if sig != 0 {
		p.sendSignal(target, nil, sig, a)
	}
	return 0
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
		p.sendSignal(p, to, sig, a)
	}
	return 0
}

// sendSignal sends a process a signal, or one of its threads: it ends the process, or does nothing.
// What it would take to do anything else stops the run as unimplemented.
func (p *Process) sendSignal(target *Process, to *thread, sig uint64, a []uint64) {
	target.signalled(sig) // whoever watches for it with a kqueue is told, whatever it then does
	handler := target.sigactions[sig].handler
	switch {
	case sig == sigKill:
	case signalsStop[sig] && (handler == 0 || sig == sigStop):
		p.unimplemented(fmt.Sprintf("signal %d, which stops a process until it is continued", sig), a)
		return
	case handler == sigIgnore, handler == 0 && signalsIgnored[sig]:
		return
	case handler != 0:
		p.unimplemented(fmt.Sprintf("signal %d to a process with a handler for it, which nothing here runs", sig), a)
		return
	default:
		// It waits, if every thread it could go to blocks it: until one does not, which
		// nothing here would notice.
		blocked := func(t *thread) bool { return t.state == dead || t.sigmask>>(sig-1)&1 != 0 }
		if to != nil && blocked(to) || to == nil && !containsThread(target.threads, func(t *thread) bool { return !blocked(t) }) {
			p.unimplemented(fmt.Sprintf("signal %d to a process that blocks it", sig), a)
			return
		}
	}
	target.endBySignal(int(sig))
}

func containsThread(threads []*thread, f func(*thread) bool) bool {
	for _, t := range threads {
		if f(t) {
			return true
		}
	}
	return false
}

// endBySignal ends a process as a signal does: at once if it is the one that has the CPU, and
// otherwise before anything else runs.
func (p *Process) endBySignal(sig int) {
	if p.exited {
		return
	}
	p.endSignal = sig
	if p == p.m.running {
		p.exited = true // as exit does: the machine sees to the rest when the call returns
		return
	}
	p.m.deferred = append(p.m.deferred, func() {
		if !p.exited {
			p.m.exit(p, fmt.Sprintf("by signal %d", sig))
		}
	})
}
