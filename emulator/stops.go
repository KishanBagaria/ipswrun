package emulator

import (
	"maps"
	"slices"
)

// A process stopped and set going again. A signal that stops (SIGSTOP always; SIGTSTP, SIGTTIN
// and SIGTTOU where they are neither caught nor ignored) stops every thread of the process where
// it is, until SIGCONT comes: its parent is told (SIGCHLD, unless it asked not to be, and wait4
// with WUNTRACED), a call it was in the middle of goes on afterwards, what it catches waits
// until then, and what would end it ends it at once. SIGCONT is told to a parent that asks
// (WCONTINUED), whether the process was stopped or not, and not by SIGCHLD. The three a
// terminal sends are not sent at all to a group with nobody to see to it. As measured on macOS
// 27.2.1 (tests/kernel/signals/stops.c) and as the source has it (kern_sig.c).

const (
	sigTerminalStop  = 18 // SIGTSTP
	codeChildStopped = 5  // CLD_STOPPED
	noChildStop      = 8  // SA_NOCLDSTOP
)

// stop stops the process for a signal, and tells its parent.
func (p *Process) stop(sig uint64) {
	if p.stopped {
		return
	}
	p.stopped, p.stopSignal, p.stopTold, p.continued = true, sig, false, false
	p.yield = true // if it is the one running, it runs no further
	parent := p.parent
	if parent == nil || parent.exited {
		return
	}
	if parent.sigactions[sigChild].flags&noChildStop == 0 {
		// The status it is told is the signal for SIGSTOP and none for the others, as measured.
		told := pendingSignal{sig: sigChild, code: codeChildStopped, pid: p.pid, uid: p.uid}
		if sig == sigStop {
			told.status = sigStop
		}
		parent.sendSignal(told)
	}
	p.m.childChanged(p)
}

// goOn is SIGCONT sent: the process goes on if it was stopped, and its parent can be told it
// was sent either way.
func (p *Process) goOn() {
	was := p.stopped
	p.stopped, p.continued = false, true
	p.m.childChanged(p)
	if was { // what was sent it while it was stopped brings it back from a call it still waits in
		for _, s := range slices.Clone(p.pending) {
			p.nudge(s)
		}
	}
}

// orphanedStops is a process having ended: a group that has nobody left to see to it and has a
// stopped process in it is told the terminal has hung up and set going (SIGHUP, then SIGCONT,
// to each of it), as the source's orphanpg has it and as measured of a stopped child whose
// parent ended.
func (m *Machine) orphanedStops(ended *Process) {
	groups := map[uint32]bool{ended.group(): true}
	for _, q := range m.procs {
		if q.parent == ended && !q.exited {
			groups[q.group()] = true
		}
	}
	for _, group := range slices.Sorted(maps.Keys(groups)) {
		in := m.inGroup(group)
		if !m.orphaned(group) || !slices.ContainsFunc(in, func(q *Process) bool { return !q.exited && q.stopped }) {
			continue
		}
		for _, q := range in {
			if !q.exited {
				q.sendSignal(pendingSignal{sig: sigHangup, status: sigHangup})
				q.sendSignal(pendingSignal{sig: sigCont, status: sigCont})
			}
		}
	}
}

// changed is what wait4 says of a child that has not ended, to a parent that asks for those
// that were stopped or set going: its status, once for each time.
func (w *waiting4) changed(child *Process) (uint32, bool) {
	switch {
	case child.exited:
	case w.options&waitStopped != 0 && child.stopped && !child.stopTold:
		child.stopTold = true
		return uint32(child.stopSignal)<<8 | 0x7f, true
	case w.options&waitContinued != 0 && child.continued:
		child.continued = false
		return sigCont<<8 | 0x7f, true
	}
	return 0, false
}

// childChanged gives the threads of a parent that wait for a child one that was stopped or set
// going, if they asked for that.
func (m *Machine) childChanged(child *Process) {
	parent := child.parent
	if parent == nil || parent.exited {
		return
	}
	for _, t := range parent.waiters(waitChild, 0) {
		w := t.waiting4
		if w == nil || !w.wants(child) {
			continue
		}
		if status, ok := w.changed(child); ok {
			if w.status != 0 {
				parent.write(w.status, le32(status))
			}
			t.waiting4 = nil
			t.answer = &result{x0: uint64(child.pid)}
			t.wake()
		}
	}
}

// anyStopped says whether a process that has not ended is stopped.
func (m *Machine) anyStopped() bool {
	for _, p := range m.procs {
		if !p.exited && p.stopped {
			return true
		}
	}
	return false
}
