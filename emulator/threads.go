package emulator

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The threads of a process share its engine, and the threads of every process take turns: one
// runs at a time (see Machine.schedule). A thread runs until it makes a call that
// waits for something (a lock, a condition, a semaphore, a deadline), offers its turn, or ends;
// nothing takes the engine away from it in between. Code that only makes progress when another
// thread runs while it spins without a system call therefore never does.

type threadState uint8

const (
	runnable threadState = iota
	waiting              // for what the wait fields say
	parked               // a work queue thread with nothing to do
	dead
)

type waitKind uint8

const (
	waitNone waitKind = iota
	waitUlock
	waitMutex
	waitCond
	waitSemaphore
	waitSleep
	waitWorkloop
	waitSignal
	waitRWLock
	waitPort
	waitLock
	waitPipe
	waitSelect
	waitChild
	waitSpawn
	waitKevent
	waitResolver
)

var waitNames = [...]string{"nothing", "a lock word", "a mutex", "a condition", "a semaphore", "a deadline", "its turn on a workloop", "a signal", "a read-write lock", "a Mach message", "a file lock", "a pipe", "a descriptor to be ready", "a child to end", "a process it is starting", "an event of a kqueue", "the kernel to ask who a user is"}

// thread is one guest thread. Its registers are in ctx whenever it is not the one running.
type thread struct {
	id int
	// What it has had of the CPU, as the user and in the kernel: its part of the process's usage.
	user, system time.Duration
	port         uint32 // what mach_thread_self names it
	name         string // what it named itself (see procctl.go)
	self         *port  // what that names: the thread, as the kernel answers for it
	ctx          uintptr
	pc           uint64
	tsd          uint64
	state        threadState

	kind     waitKind
	key      uint64    // the address or port waited on
	order    uint64    // waiters on one key are woken in the order they came
	deadline time.Time // zero: none
	late     result    // what the wait returns once the deadline passes

	// answer, when set, is what the call the thread is in returns instead of its handler's value.
	// It is put in the registers when the thread next has the engine.
	answer *result
	// relaunched says the registers in ctx are a new start, not where the thread left off.
	relaunched bool

	reply     uint32      // the port it waits on for the answer to a synchronous message, once it asked for one
	receiving *message    // of a thread waiting for a Mach message: where it wants it
	pipeOp    *pipeOp     // of a thread waiting on a pipe: what it is in the middle of
	selecting *selecting  // of a thread waiting in select: what it asked about
	polling   *polling    // or in poll
	kevent    *keventWait // of a thread waiting in kevent: on which kqueue, and where its events go
	cwd       string      // its working directory, if it has one of its own (see sysPthreadChdir)
	waiting4  *waiting4   // of a thread waiting for a child to end: which, and where to say how

	sigmask   uint32    // the signals it blocks
	altstack  [2]uint64 // its alternate signal stack, if it has one: where, and its size
	onstack   bool      // a handler of its is running on that stack
	suspended *uint32   // of a thread in sigsuspend: the mask it had before, which it has again after
	sleeping  bool      // that call is the one libc sleeps with, which a signal interrupts
	arg0      uint64    // the first argument of the system call it is in, which the answer is put in place of

	pthread uint64  // of a work queue thread: its pthread, which sits on top of its stack
	serving *kqueue // of a work queue thread: whose events it was started for, if any
}

// result is what a call returns: x0, and the carry flag by which a BSD call says x0 is an errno.
type result struct {
	x0    uint64
	carry bool
}

var timedOut = result{eTIMEDOUT, true}

const (
	kernTimedOut       = 49 // KERN_OPERATION_TIMED_OUT
	mainThreadPort     = 0x103
	workerStackSize    = 512 << 10
	workerEvents       = 16 // WQ_KEVENT_LIST_LEN
	maxThreads         = 256
	ulockNoErrno       = 0x01000000
	ulockWakeAll       = 0x100
	ulockWakeThread    = 0x200
	pthreadStartCustom = 0x01000000
	pthreadStartTSDSet = 0x10000000 // told to the thread: its thread data register is already set
	condIncrement      = 0x100      // one waiter in a condition variable's sequence words

	// What a work queue thread is told about itself when it starts.
	wqPrioQOS      = 0x00004000
	wqOvercommit   = 0x00010000
	wqReuse        = 0x00020000
	wqNewSPI       = 0x00040000
	wqKevent       = 0x00080000 // started for events
	wqEventManager = 0x00100000
	wqTSDBaseSet   = 0x00200000
	wqWorkloop     = 0x00400000 // started for a workloop's events
	wqCooperative  = 0x01000000 // of the pool Swift concurrency runs on

	// Flags of a pthread_priority_t, beside its class.
	priorityOvercommit   = 0x80000000
	priorityCooperative  = 0x08000000
	priorityEventManager = 0x02000000 // for the thread that manages libdispatch's events
)

func (p *Process) newThread() *thread {
	if len(p.threads) >= maxThreads {
		return nil
	}
	p.nextThread++
	t := &thread{id: p.nextThread, ctx: p.uc.newContext()}
	t.port = p.threadSelf(t)
	p.threads = append(p.threads, t)
	return t
}

// launch points a thread that is not running at an entry point, with the arguments in x0…
func (p *Process) launch(t *thread, pc, sp uint64, args ...uint64) {
	p.uc.save(t.ctx) // the system registers every thread shares; the rest is set here
	for i := 0; i < 31; i++ {
		p.uc.setContextReg(t.ctx, xrID(uint32(i)), 0)
	}
	for i, a := range args {
		p.uc.setContextReg(t.ctx, regX0+i, a)
	}
	p.uc.setContextReg(t.ctx, regSP, sp)
	p.uc.setContextReg(t.ctx, regTPIDRRO, t.tsd)
	t.pc, t.state, t.answer, t.relaunched = pc, runnable, nil, true
}

// wait takes the calling thread off the engine until wake, or until the deadline if there is one.
func (p *Process) wait(kind waitKind, key uint64, deadline time.Time, late result) {
	p.waits++
	t := p.current
	t.state, t.kind, t.key, t.order, t.deadline, t.late = waiting, kind, key, p.waits, deadline, late
}

// waiters are the threads waiting on key, first come first.
func (p *Process) waiters(kind waitKind, key uint64) []*thread {
	var out []*thread
	for _, t := range p.threads {
		if t.state == waiting && t.kind == kind && t.key == key {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *thread) int { return int(a.order) - int(b.order) })
	return out
}

// firstWaiter is the thread that has waited longest on key, nil if none waits.
func (p *Process) firstWaiter(kind waitKind, key uint64) *thread {
	var first *thread
	for _, t := range p.threads {
		if t.state == waiting && t.kind == kind && t.key == key && (first == nil || t.order < first.order) {
			first = t
		}
	}
	return first
}

func (t *thread) wake() { t.state = runnable }

// wakeOne wakes the thread that has waited longest on key, if any has.
func (p *Process) wakeOne(kind waitKind, key uint64) bool {
	t := p.firstWaiter(kind, key)
	if t != nil {
		t.wake()
	}
	return t != nil
}

// deliver puts the running thread's answer, if it has one, where its call's result goes.
func (p *Process) deliver() {
	if t := p.current; t.answer != nil && t.state == runnable {
		p.setX(0, t.answer.x0)
		p.carry(t.answer.carry)
		t.answer = nil
	}
}

// reschedule reports whether the running thread has to give up the engine: it waits, is parked or
// has ended, or it offered its turn.
func (p *Process) reschedule() bool { return p.current.state != runnable || p.yield }

// charge counts what the process has used since it was last counted as the running thread's:
// one thread has the CPU at a time, so what the process used in between is that thread's.
func (p *Process) charge() {
	u := p.usage
	u.enter(p.m.clock, true)
	p.current.user, p.current.system = p.current.user+u.user-p.charged[0], p.current.system+u.system-p.charged[1]
	p.charged = [2]time.Duration{u.user, u.system}
}

// switchTo gives a thread of the process the engine: the registers of the one that had it are
// put away, unless they are still its own, and the answer the thread was waiting for is delivered.
func (p *Process) switchTo(t *thread) {
	p.charge()
	if cur := p.current; t != cur || t.relaunched {
		if t != cur && cur.state != dead && !cur.relaunched {
			p.uc.save(cur.ctx)
		}
		p.uc.restore(t.ctx)
		p.current, t.relaunched = t, false
	}
	p.deliver()
	p.takeSignals()
}

// threadList describes every thread that has not ended, one per line.
func (p *Process) threadList() string {
	var out []string
	for _, t := range p.threads {
		var what string
		switch t.state {
		case dead:
			continue
		case runnable:
			what = "can run"
		case parked:
			what = "work queue thread with nothing to do"
		case waiting:
			what = fmt.Sprintf("waiting for %s (%#x)", waitNames[t.kind], t.key)
			if !t.deadline.IsZero() {
				what += fmt.Sprintf(", for at most %.1fs more", t.deadline.Sub(p.m.clock.now()).Seconds())
			}
		}
		pc, lr := t.pc, uint64(0)
		if t == p.current {
			pc, lr = p.uc.reg(regPC), p.uc.reg(regLR)
		} else if t.ctx != 0 {
			lr = p.uc.contextReg(t.ctx, regLR)
		}
		out = append(out, fmt.Sprintf("    thread %d: %s, at %s lr %s", t.id, what, p.where(pc), p.where(lr)))
		if t.state == waiting && t != p.current && t.ctx != 0 {
			// Who called what it waits in: the frames above, a few of them.
			var callers []string
			fp := p.uc.contextReg(t.ctx, regX29)
			for i := 0; i < 8 && fp != 0 && p.isMapped(fp) && p.isMapped(fp+15); i++ {
				frame, err := p.uc.memRead(fp, 16)
				if err != nil {
					break
				}
				fp = le.Uint64(frame)
				callers = append(callers, p.where(le.Uint64(frame[8:])&0xFFFFFFFFFFFF))
			}
			if len(callers) != 0 {
				out = append(out, "      called from "+strings.Join(callers, " < "))
			}
		}
	}
	return strings.Join(out, "\n")
}

// relative is a timeout given as seconds and nanoseconds.
func relative(sec, nsec uint64) time.Duration {
	return time.Duration(int64(sec))*time.Second + time.Duration(int32(nsec))
}

// after is the time a relative timeout ends; zero for none.
func (p *Process) after(d time.Duration) time.Time {
	if d <= 0 {
		return time.Time{}
	}
	return p.m.clock.now().Add(d)
}

// ---- creating and ending threads ----

// sysBsdthreadRegister learns where libpthread wants new threads to start.
func sysBsdthreadRegister(p *Process, a []uint64) int64 {
	p.threadStart, p.wqThread, p.pthreadSize = a[0], a[1], a[2]
	// struct _pthread_registration_data says where in a pthread its thread data is, and where in
	// that the slot for mach_thread_self.
	field := func(at, fallback uint64) uint64 {
		if a[3] != 0 && a[4] >= at+4 {
			if offset := uint64(p.u32(a[3] + at)); offset != 0 && offset < p.pthreadSize {
				return offset
			}
		}
		return fallback
	}
	p.tsdOffset, p.portOffset = field(24, 0xE0), field(32, 3*8)
	p.note("  bsdthread_register: pthread size %#x, thread data at %#x", p.pthreadSize, p.tsdOffset)
	return 0x400000FF
}

// sysBsdthreadCreate starts a thread on the stack and pthread libpthread allocated for it.
func sysBsdthreadCreate(p *Process, a []uint64) int64 {
	function, arg, stack, pthread, flags := a[0], a[1], a[2], a[3], a[4]
	if flags&pthreadStartCustom == 0 || p.threadStart == 0 {
		return -eNOTSUP // the kernel allocating the stack: no libpthread since macOS 10.14 asks
	}
	t := p.newThread()
	if t == nil {
		return -eAGAIN
	}
	t.sigmask = p.current.sigmask // a new thread blocks what the one that made it does
	p.adopt(t, pthread)
	p.launch(t, p.threadStart, stack&^15, pthread, uint64(t.port), function, arg, 0, flags|pthreadStartTSDSet)
	p.note("  thread %d created: function %s", t.id, p.where(function))
	return int64(pthread)
}

// sysBsdthreadTerminate ends the calling thread, freeing its stack and telling who waits for it.
func sysBsdthreadTerminate(p *Process, a []uint64) int64 {
	if a[1] != 0 {
		sysMunmap(p, a)
	}
	if sem := uint32(a[3]); sem != 0 {
		p.signal(sem)
	}
	p.end(p.current)
	return 0
}

// adopt gives a thread its pthread: where its thread data is, and in it, as the kernel leaves it
// for anyone who looks before the thread has run, its port.
func (p *Process) adopt(t *thread, pthread uint64) {
	t.tsd = pthread + p.tsdOffset
	p.write(t.tsd+p.portOffset, le32(t.port))
}

func (p *Process) end(t *thread) {
	p.note("  thread %d ended", t.id)
	t.state = dead
	destroy(t.self) // the names there are for it are dead names now
	p.uc.freeContext(t.ctx)
	t.ctx = 0
	if q := t.serving; q != nil && q.servicer == t {
		q.servicer = nil
	}
	if t != p.threads[0] { // the first is kept: it is where a run with every thread gone ends
		p.threads = slices.DeleteFunc(p.threads, func(o *thread) bool { return o == t })
	}
}

// sysYield offers the caller's turn, which is taken if anything else could use it: another thread
// that can run, or a deadline, timer or event that may have come due.
func sysYield(p *Process, a []uint64) int64 {
	p.yield = p.yield || p.contended()
	return 0
}

// contended reports whether anything but the running thread could use a turn.
func (p *Process) contended() bool {
	for _, t := range p.threads {
		if t != p.current && (t.state == runnable || t.state == waiting && !t.deadline.IsZero()) {
			return true
		}
	}
	for _, q := range p.kqueues {
		if q.request != nil || len(q.ready) > 0 || len(q.timers) > 0 || len(q.ports) > 0 {
			return true
		}
	}
	for _, r := range p.names {
		if r.receive && !r.port.due.IsZero() {
			return true
		}
	}
	// Another process may be able to run, and this one may be waiting for what it does.
	for _, other := range p.m.procs {
		if other != p && !other.exited && slices.ContainsFunc(other.threads, func(t *thread) bool { return t.state == runnable }) {
			return true
		}
	}
	return false
}

// sysSigsuspend waits for a signal. None is ever sent, so the thread is never heard from again.
// sysSigsuspend is sigsuspend(mask): the thread blocks those signals and no others, and waits
// for one it does not block to have its handler run, or to end the process. sysSigwait waits
// for one of a set of signals to be sent, with no handler run, which nothing here ends.
func sysSigsuspend(p *Process, a []uint64) int64 {
	t := p.current
	before := t.sigmask
	t.suspended, t.sigmask = &before, uint32(a[0])&^(1<<(sigKill-1)|1<<(sigStop-1))
	p.wait(waitSignal, 0, time.Time{}, result{})
	// One that waited already is taken now.
	for _, s := range p.pending {
		if s.takes(t) {
			t.state = runnable
			t.answer = &result{x0: eINTR, carry: true}
			break
		}
	}
	return 0
}

func sysSigwait(p *Process, a []uint64) int64 {
	p.wait(waitSignal, 0, time.Time{}, result{})
	return 0
}

// sysSemwaitSignal is how libc sleeps: a wait on no semaphore, with a relative timeout.
func sysSemwaitSignal(p *Process, a []uint64) int64 {
	cond, mutex, timed := uint32(a[0]), uint32(a[1]), a[2] != 0
	if mutex != 0 {
		p.signal(mutex)
	}
	var deadline time.Time
	if timed {
		deadline = p.m.clock.now().Add(relative(a[4], a[5]))
	}
	switch {
	case cond != 0:
		p.semWait(cond, deadline, timedOut)
	case timed:
		p.wait(waitSleep, 0, deadline, timedOut)
	default:
		return -eINVAL
	}
	return 0
}

// ---- ulock: the wait and wake behind os_unfair_lock, dispatch and pthread_join ----

func ulockWait(p *Process, a []uint64, timeout time.Duration) int64 {
	op, addr, want := a[0], a[1], a[2]
	var value uint64
	if kind := op & 0xFF; kind >= 4 { // the 64-bit kinds
		value = p.u64(addr)
	} else {
		value, want = uint64(p.u32(addr)), want&0xFFFFFFFF
	}
	if value != want {
		return 0 // it changed before the caller got here: nothing to wait for
	}
	p.wait(waitUlock, addr, p.after(timeout), ulockError(op, eTIMEDOUT))
	return 0
}

// ulockError is a ulock call's failure: an errno, or with ULF_NO_ERRNO its negative as the result.
func ulockError(op uint64, errno int64) result {
	if op&ulockNoErrno != 0 {
		return result{x0: uint64(-errno)}
	}
	return result{uint64(errno), true}
}

func sysUlockWait(p *Process, a []uint64) int64 {
	return ulockWait(p, a, time.Duration(uint32(a[3]))*time.Microsecond)
}

func sysUlockWait2(p *Process, a []uint64) int64 { return ulockWait(p, a, time.Duration(a[3])) }

func sysUlockWake(p *Process, a []uint64) int64 {
	op, addr := a[0], a[1]
	woken := 0
	for _, t := range p.waiters(waitUlock, addr) {
		if op&ulockWakeThread != 0 && t.port != uint32(a[2]) {
			continue
		}
		t.wake()
		if woken++; op&ulockWakeAll == 0 {
			break
		}
	}
	if woken == 0 {
		failed := ulockError(op, eNOENT)
		p.current.answer = &failed
	}
	return 0
}

// ---- psynch: what pthread mutexes and condition variables do once they have to wait ----

// A mutex unlocked for a waiter that has not arrived yet lets that waiter straight through.
func sysPsynchMutexwait(p *Process, a []uint64) int64 {
	mutex := a[0]
	if p.handoffs[mutex] > 0 {
		p.handoffs[mutex]--
	} else {
		p.wait(waitMutex, mutex, time.Time{}, result{})
	}
	return int64(a[1] & 0xFFFFFFFF) // the caller takes the lock itself, and waits again if it cannot
}

func (p *Process) mutexDrop(mutex uint64) {
	if !p.wakeOne(waitMutex, mutex) {
		p.handoffs[mutex]++
	}
}

func sysPsynchMutexdrop(p *Process, a []uint64) int64 {
	p.mutexDrop(a[0])
	return 0
}

// sysPsynchCvwait waits on a condition variable; the mutex, if given, still has to be released.
func sysPsynchCvwait(p *Process, a []uint64) int64 {
	cond, mutex := a[0], a[3]
	if mutex != 0 {
		p.mutexDrop(mutex)
	}
	p.wait(waitCond, cond, p.after(relative(a[6], a[7])), timedOut)
	return 0
}

// Signals answer with how many waiters they woke, which libpthread adds to the count of waits
// the condition variable has finished with.
func sysPsynchCvsignal(p *Process, a []uint64) int64 {
	for _, t := range p.waiters(waitCond, a[0]) {
		if port := uint32(a[3]); port == 0 || port == t.port {
			t.wake()
			return condIncrement
		}
	}
	return 0
}

func sysPsynchCvbroad(p *Process, a []uint64) int64 {
	woken := p.waiters(waitCond, a[0])
	for _, t := range woken {
		t.wake()
	}
	return int64(len(woken)) * condIncrement
}

// ---- psynch: read-write locks ----

// libpthread takes and releases a read-write lock by itself for as long as nobody has to wait,
// counting locks (L), unlocks (U) and what the kernel last granted up to (S) in the lock's words.
// It comes here to wait, and to unlock once everything granted has been released: by then every
// lock that is still out is a waiter here, and the oldest of them get the lock: one writer, or
// the readers up to the next writer. What a call returns is how many were let in, with bits, for
// libpthread to merge into those words.

const (
	rwCount   = 0xFFFFFF00
	rwKernel  = 0x01 // K: cannot be taken without asking
	rwExclude = 0x02 // E: a writer has it
	rwWriters = 0x04 // W: writers are waiting; in S, saved by an unlock that saw them
	rwOverlap = 0x40 // M: a reader let in alongside the readers already granted
	rwReset   = 0x80 // I: the lock's words were reset since the kernel last saw it
)

type rwWaiter struct {
	seq uint32
	t   *thread
}

type rwlock struct {
	readers, writers []rwWaiter // in the order of their lock counts
	overlap          bool       // only readers were granted last: more may join them
	next, last       uint32     // S after the last grant, and at the unlock that made it
	owed             int        // an unlock came before this many of its waiters had
	owedS            uint32
}

// rwlock is what is known of the lock at addr, given the L word its caller came with.
func (p *Process) rwlock(addr uint64, lgen uint32) *rwlock {
	l := p.rwlocks[addr]
	if l == nil {
		l = &rwlock{}
		p.rwlocks[addr] = l
	}
	if lgen&rwReset != 0 {
		l.owed = 0
	}
	return l
}

func psynchRWLock(p *Process, a []uint64, write bool) int64 {
	lgen, s := uint32(a[1]), uint32(a[3])
	l := p.rwlock(a[0], lgen)
	if !write && l.overlap && s&rwWriters == 0 && lgen&rwWriters == 0 && (s&rwCount <= l.next&rwCount || l.last&rwCount >= s&rwCount) {
		l.next += condIncrement
		return int64(condIncrement | l.next&0xFF | rwOverlap)
	}
	queue := &l.readers
	if write {
		queue = &l.writers
	}
	*queue = append(*queue, rwWaiter{lgen & rwCount, p.current})
	slices.SortFunc(*queue, func(x, y rwWaiter) int { return int(int32(x.seq - y.seq)) })
	p.wait(waitRWLock, a[0], time.Time{}, result{})
	if l.owed > 0 && len(l.readers)+len(l.writers) >= l.owed {
		l.owed = 0
		return int64(p.rwGrant(l, l.owedS))
	}
	return 0
}

func sysPsynchRWRdlock(p *Process, a []uint64) int64 { return psynchRWLock(p, a, false) }
func sysPsynchRWWrlock(p *Process, a []uint64) int64 { return psynchRWLock(p, a, true) }

func sysPsynchRWUnlock(p *Process, a []uint64) int64 {
	lgen, ugen, s := uint32(a[1]), uint32(a[2]), uint32(a[3])
	l := p.rwlock(a[0], lgen)
	out := int((lgen&rwCount - ugen&rwCount) >> 8)
	if waiting := len(l.readers) + len(l.writers); waiting == 0 || waiting < out {
		l.owed, l.owedS = out, s
		return int64(lgen) // nothing to merge: the waiters that are still to come get the lock
	}
	return int64(p.rwGrant(l, s))
}

// rwGrant lets in the oldest waiters and returns what each of them, and the unlock, is told.
func (p *Process) rwGrant(l *rwlock, s uint32) uint32 {
	l.last, l.overlap = s, false
	var granted []rwWaiter
	var bits uint32
	if len(l.writers) > 0 && (len(l.readers) == 0 || int32(l.writers[0].seq-l.readers[0].seq) < 0) {
		granted, l.writers = l.writers[:1], l.writers[1:]
		bits = condIncrement | rwKernel | rwExclude
		if len(l.writers) > 0 {
			bits |= rwWriters
		}
	} else {
		n := len(l.readers)
		if len(l.writers) > 0 {
			n = 0
			for n < len(l.readers) && int32(l.readers[n].seq-l.writers[0].seq) < 0 {
				n++
			}
			bits = rwWriters | rwKernel
		} else {
			l.overlap = true
		}
		granted, l.readers = l.readers[:n], l.readers[n:]
		bits += uint32(n) << 8
	}
	l.next = s&rwCount + bits
	for _, w := range granted {
		w.t.wake()
		if w.t != p.current { // the caller, if it is let in as it arrives, returns them itself
			w.t.answer = &result{x0: uint64(bits)}
		}
	}
	return bits
}

// ---- Mach semaphores ----

// take takes one count of a semaphore if it has any.
func (p *Process) take(sem uint32) bool {
	if p.semaphores[sem] > 0 {
		p.semaphores[sem]--
		return true
	}
	return false
}

func (p *Process) signal(sem uint32) {
	if !p.wakeOne(waitSemaphore, uint64(sem)) {
		p.semaphores[sem]++
	}
}

// semWait takes one count of a semaphore, waiting for one if it has none.
func (p *Process) semWait(sem uint32, deadline time.Time, late result) {
	if !p.take(sem) {
		p.wait(waitSemaphore, uint64(sem), deadline, late)
	}
}

// migSemaphoreCreate is semaphore_create(task, policy, count): a semaphore that as many waits
// as its count pass before one has to wait for a signal. A count below none is refused.
func migSemaphoreCreate(p *Process, body []byte) migReply {
	count := int32(le.Uint32(body[36:]))
	if count < 0 {
		return migCode(kernInvalidArg)
	}
	name := p.newPort()
	p.semaphores[name] = int(count)
	return migPort(name)
}

// isSemaphore is whether a name is a semaphore's, which is a name and a count and no port here.
func (p *Process) isSemaphore(name uint32) bool {
	count, ok := p.semaphores[name]
	return ok && count != semaphoreGone
}

// held is the right a name is, for a call that asks of a name what it is. A semaphore's, which
// is a right on a Mac and none here, stops the run.
func (p *Process) held(name uint32, call string) *right {
	r := p.names[name]
	if r == nil && p.isSemaphore(name) {
		p.unimplemented(call+" of a semaphore", nil)
	}
	return r
}

// semaphoreGone is the count of a semaphore that was destroyed: its name is no semaphore's.
const semaphoreGone = -1

// migSemaphoreDestroy is semaphore_destroy(task, semaphore). What a thread that waits on it is
// then told was not measured.
func migSemaphoreDestroy(p *Process, body []byte) migReply {
	name := le.Uint32(body[28:])
	if p.firstWaiter(waitSemaphore, uint64(name)) != nil {
		p.unimplemented("semaphore_destroy of a semaphore that is waited on", nil)
		return migCode(kernFailure)
	}
	p.semaphores[name] = semaphoreGone
	return migCode(0)
}

func machSemaphoreSignal(p *Process, a []uint64) int64 {
	p.signal(uint32(a[0]))
	return 0
}

func machSemaphoreSignalAll(p *Process, a []uint64) int64 {
	for _, t := range p.waiters(waitSemaphore, a[0]&0xFFFFFFFF) {
		t.wake()
	}
	return 0
}

func machSemaphoreWait(p *Process, a []uint64) int64 {
	if p.semaphores[uint32(a[0])] == semaphoreGone {
		return kernInvalidName
	}
	p.semWait(uint32(a[0]), time.Time{}, result{})
	return 0
}

func machSemaphoreTimedwait(p *Process, a []uint64) int64 {
	timeout := relative(a[1]&0xFFFFFFFF, a[2])
	if p.semaphores[uint32(a[0])] == semaphoreGone {
		return kernInvalidName
	}
	if timeout <= 0 && p.semaphores[uint32(a[0])] == 0 {
		return kernTimedOut
	}
	p.semWait(uint32(a[0]), p.m.clock.now().Add(timeout), result{x0: kernTimedOut})
	return 0
}

func machSemaphoreWaitSignal(p *Process, a []uint64) int64 {
	p.signal(uint32(a[1]))
	return machSemaphoreWait(p, a)
}

// ---- the work queue: the threads libdispatch asks the kernel for ----

func sysWorkqKernreturn(p *Process, a []uint64) int64 {
	p.note("  workq_kernreturn options %#x", a[0])
	switch a[0] {
	case 0x20, 0x30: // WQOPS_QUEUE_REQTHREADS, and the one for the cooperative pool: this many threads at this priority
		if p.wqThread == 0 {
			return -eINVAL
		}
		for i := uint64(0); i < a[2]&0xFFFF; i++ {
			if p.startWorker(a[3], nil, nil) == nil {
				break
			}
		}
	case 0x04, 0x40, 0x100: // WQOPS_THREAD_RETURN, and for kevents and workloops: nothing left to do
		if p.current.pthread == 0 { // not a work queue thread
			return -eINVAL
		}
		p.finish(p.current, a[1], a[2]&0xFFFF)
	}
	return 0
}

// startWorker sends a work queue thread, a parked one if there is one, to libpthread's entry
// point for them with the quality of service it was asked for: to drain libdispatch's queues of
// that class, or with events, to handle those for the kqueue they are from.
func (p *Process) startWorker(priority uint64, q *kqueue, events []kevent) *thread {
	if p.wqThread == 0 {
		return nil // libpthread has not said where they start
	}
	flags := uint64(wqNewSPI | wqPrioQOS | wqTSDBaseSet)
	// A pthread_priority_t has one bit set for its class: bit 8 is the lowest (maintenance, 1).
	for class := uint64(1); class <= 6; class++ {
		if priority>>(7+class)&1 != 0 {
			flags |= class
		}
	}
	if flags&0xF == 0 {
		flags |= 4 // none asked for: the default class
	}
	if priority&priorityOvercommit != 0 {
		flags |= wqOvercommit
	}
	if priority&priorityEventManager != 0 {
		flags |= wqEventManager
	}
	if priority&priorityCooperative != 0 {
		flags |= wqCooperative
	}
	var t *thread
	for _, candidate := range p.threads {
		if candidate.state == parked {
			t, flags = candidate, flags|wqReuse
			break
		}
	}
	if t == nil {
		if t = p.newThread(); t == nil {
			return nil
		}
		// Lowest first: a guard page, the stack, and the pthread libpthread fills in.
		size := pageSize + workerStackSize + align(p.pthreadSize, pageSize)
		base, ok := p.alloc(size, allocation{name: fmt.Sprintf("thread %d", t.id)})
		if !ok {
			p.end(t)
			return nil
		}
		t.pthread = base + pageSize + workerStackSize
	}
	p.adopt(t, t.pthread)
	// The events go on the stack under the pthread, a workloop's id in the word before them. There
	// is room for workerEvents whatever their number: libdispatch keeps its own changes there.
	top := t.pthread - workerEvents*keventSize
	var list uint64
	if q != nil {
		list = top
		flags |= wqKevent
		if q.id != 0 {
			flags |= wqWorkloop
		}
		p.write(list-8, le64(q.id))
		p.writeKevents(list, events)
	}
	t.serving = q
	p.launch(t, p.wqThread, (top-64)&^15, t.pthread, uint64(t.port), t.pthread-workerStackSize, list, flags, uint64(len(events)))
	return t
}
