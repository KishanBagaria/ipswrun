package emulator

import (
	"testing"
	"time"
)

func TestKeventRoundTrip(t *testing.T) {
	k := kevent{ident: 0x1122334455667788, filter: filterWorkloop, flags: evAdd | evEnable, qos: 0x800008ff, udata: 0xAABB,
		fflags: noteThreadRequest, xflags: 7, data: -5, ext: [4]uint64{1, 2, 3, 4}}
	b := k.bytes()
	if len(b) != keventSize {
		t.Fatalf("a kevent is %d bytes, want %d", len(b), keventSize)
	}
	// The fields at the offsets of struct kevent_qos_s.
	if le.Uint64(b) != k.ident || int16(le.Uint16(b[8:])) != k.filter || le.Uint16(b[10:]) != k.flags || le.Uint32(b[12:]) != k.qos ||
		le.Uint64(b[16:]) != k.udata || le.Uint32(b[24:]) != k.fflags || le.Uint32(b[28:]) != k.xflags || int64(le.Uint64(b[32:])) != k.data ||
		le.Uint64(b[40:]) != 1 || le.Uint64(b[64:]) != 4 {
		t.Errorf("fields are not where struct kevent_qos_s has them: % x", b)
	}
}

func TestThreads(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		const lock, sem = dataAddr + 0x400, 0x5503
		number := func(name string) int64 {
			for n, candidate := range bsdNames {
				if candidate == name {
					return n
				}
			}
			t.Fatalf("no system call named %s", name)
			return 0
		}
		wait, wake := number("ulock_wait"), number("ulock_wake")

		// A lock word that is not what the caller expects is not waited for.
		p.write(lock, le32(7))
		if x0, carry, _ := p.trap(wait, 1, lock, 8, 0); x0 != 0 || carry || p.current.state != runnable {
			t.Errorf("ulock_wait on a changed word: x0 %d carry %v state %d", x0, carry, p.current.state)
		}
		// Nobody to wake is ENOENT: as an errno, or as the result itself when asked.
		if x0, carry, _ := p.trap(wake, 1, lock, 0); x0 != eNOENT || !carry {
			t.Errorf("ulock_wake with no waiter: x0 %d carry %v", x0, carry)
		}
		if x0, carry, _ := p.trap(wake, 1|ulockNoErrno, lock, 0); int64(x0) != -eNOENT || carry {
			t.Errorf("ulock_wake with no waiter, no errno: x0 %d carry %v", int64(x0), carry)
		}
		// A semaphore keeps the signals nobody was waiting for.
		p.trap(-33, sem)
		if x0, _, _ := p.trap(-36, sem); x0 != 0 || p.current.state != runnable {
			t.Errorf("semaphore_wait after a signal: x0 %d state %d", x0, p.current.state)
		}
		if x0, _, _ := p.trap(-38, sem, 0, 0); x0 != kernTimedOut {
			t.Errorf("semaphore_timedwait for no time on an empty semaphore: x0 %d", x0)
		}

		// A second thread: the first waits on the lock word, the scheduler brings in the second
		// with the registers it was launched with, and its wake lets the first go on.
		main, other := p.current, p.newThread()
		other.tsd = 0x7700
		p.launch(other, codeAddr+0x100, dataAddr+0x800, 11, 22)
		p.trap(wait, 1, lock, 7, 0)
		if main.state != waiting || !p.reschedule() {
			t.Fatalf("ulock_wait on the expected word: state %d, reschedule %v", main.state, p.reschedule())
		}
		p.setX(19, 0x1919) // a register no call touches: it has to come back with the thread
		if pc := p.schedule(codeAddr + 4); pc != codeAddr+0x100 || p.current != other {
			t.Fatalf("scheduled thread %d at %#x, want the new thread at its entry", p.current.id, pc)
		}
		if p.x(0) != 11 || p.x(1) != 22 || p.uc.reg(regSP) != dataAddr+0x800 || p.uc.reg(regTPIDRRO) != 0x7700 || p.x(19) != 0 {
			t.Errorf("the new thread has x0 %d x1 %d sp %#x thread data %#x x19 %#x", p.x(0), p.x(1), p.uc.reg(regSP), p.uc.reg(regTPIDRRO), p.x(19))
		}
		if x0, _, _ := p.trap(-27); x0 != uint64(other.port) || other.port == mainThreadPort {
			t.Errorf("thread_self on the new thread is %#x, its port %#x", x0, other.port)
		}
		if x0, carry, _ := p.trap(wake, 1, lock, 0); x0 != 0 || carry || main.state != runnable {
			t.Errorf("ulock_wake: x0 %d carry %v, waiter state %d", x0, carry, main.state)
		}
		p.end(other)
		if pc := p.schedule(0); pc != codeAddr+4 || p.current != main || p.x(19) != 0x1919 || len(p.threads) != 1 {
			t.Errorf("after the new thread ended: thread %d at %#x, x19 %#x, %d threads", p.current.id, pc, p.x(19), len(p.threads))
		}

		// A wait with a deadline ends with the timeout's result once nothing else can run.
		started := time.Now()
		p.trap(-38, sem, 0, 30_000_000) // 30 ms
		if main.state != waiting {
			t.Fatalf("semaphore_timedwait on an empty semaphore: state %d", main.state)
		}
		p.schedule(codeAddr + 4)
		if waited := time.Since(started); p.x(0) != kernTimedOut || main.state != runnable || waited < 25*time.Millisecond {
			t.Errorf("after the deadline: x0 %d state %d, waited %v", p.x(0), main.state, waited)
		}

		// A timer on the work queue's kqueue is due when its deadline on the guest's clock is.
		q := p.kqueue(0)
		p.changeTimer(q, kevent{ident: 1, filter: filterTimer, flags: evAdd | evOneshot, fflags: noteAbsolute | noteMachTime,
			data: int64(machTicks() + uint64(time.Second/tickNanos))})
		if due := time.Until(q.timers[1].deadline); due < 900*time.Millisecond || due > 1100*time.Millisecond {
			t.Errorf("a timer a second of ticks away is due in %v", due)
		}
		if next := p.serve(time.Now()); next.IsZero() || len(q.ready) != 0 {
			t.Errorf("a timer that is not due: next %v, %d events ready", next, len(q.ready))
		}
	})
}
