package emulator

import (
	"sync/atomic"
	"testing"
	"time"
)

// slowFile is a file of the build that the host has to fetch: every read takes a while.
type slowFile struct {
	delay time.Duration
	reads int
}

func (f *slowFile) Size() int64 { return 1 << 20 }

func (f *slowFile) Pread(off, n int64) ([]byte, error) {
	f.reads++
	time.Sleep(f.delay)
	data := make([]byte, n)
	data[0] = 0x5A
	return data, nil
}

// mapSlow gives a process a mapping of a slow file, no page of which has been touched.
func mapSlow(t *testing.T, p *Process, f *slowFile) uint64 {
	addr, ok := p.alloc(blockSize, allocation{kind: fileRegion, file: f, name: "slow"})
	if !ok {
		t.Fatal("no room for a mapping")
	}
	return addr
}

// The clock does not move while the host is away, and nothing that is due by it comes due.
func TestClockStandsStillWhileAway(t *testing.T) {
	const away, soon = 400 * time.Millisecond, 100 * time.Millisecond
	c := newClock()
	var rings atomic.Int32
	now, ticks := c.now(), c.ticks()
	off := c.alarm(now.Add(soon), func() { rings.Add(1) })
	started := time.Now()
	c.away(func() {
		c.away(func() { time.Sleep(away) }) // what is fetched may fetch
		if during := c.now().Sub(now); during >= soon {
			t.Errorf("the clock moved %v while the host was away", during)
		}
	})
	if rings.Load() != 0 {
		t.Errorf("an alarm %v away rang while the host was away", soon)
	}
	if passed := c.now().Sub(now); passed >= away/2 {
		t.Errorf("%v passed on the clock over %v the host was away", passed, away)
	}
	if passed := tickDuration(int64(c.ticks() - ticks)); passed >= away/2 {
		t.Errorf("%v of Mach time passed over %v the host was away", passed, away)
	}
	// And it goes on from where it was: the alarm rings when the rest of its time has passed.
	for rings.Load() == 0 && time.Since(started) < 5*time.Second {
		time.Sleep(time.Millisecond)
	}
	if !off() || time.Since(started) < away+soon/2 {
		t.Errorf("the alarm rang %d times, %v after it was set", rings.Load(), time.Since(started))
	}
	if late := c.tickTime(ticks).Sub(now); late < -time.Millisecond || late > time.Millisecond {
		t.Errorf("the time of a tick is %v off after the host was away", late)
	}
}

// A page that takes the host a while to fetch takes the guests no time: not on the clocks they
// read, and not from a wait that another thread is in.
func TestFetchTakesNoGuestTime(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		const sem, out = 0x5503, dataAddr + 0x400
		const delay, patience = 500 * time.Millisecond, 200 * time.Millisecond
		slow := &slowFile{delay: delay}
		addr := mapSlow(t, p, slow)
		clocks := func() (wall time.Time, ticks, absolute uint64) {
			p.trap(number(t, "gettimeofday"), out, 0, out+16)
			ticks, _, _ = p.trap(-3) // mach_absolute_time
			return time.Unix(int64(p.u64(out)), int64(p.u64(out+8))*1000), p.u64(out + 16), ticks
		}

		// The main thread waits for a semaphore, for less time than the fetch will take.
		main, other := p.current, p.newThread()
		p.launch(other, codeAddr+0x100, dataAddr+0x800, 0, 0)
		p.trap(-38, sem, 0, uint64(patience)) // semaphore_timedwait
		if main.state != waiting {
			t.Errorf("semaphore_timedwait on an empty semaphore: state %d", main.state)
			return // not Fatalf: this is not the test's goroutine
		}
		p.current.pc, p.m.running = codeAddr+4, p
		p.m.schedule()
		if p.current != other {
			t.Errorf("thread %d has the turn, not the new one", p.current.id)
			return // not Fatalf: this is not the test's goroutine
		}

		// The other touches the page, as a system call given its address does.
		wall, ticks, absolute := clocks()
		started := time.Now()
		if got := p.read(addr, 1); got[0] != 0x5A || slow.reads != 1 || time.Since(started) < delay {
			t.Errorf("read %#x after %d reads and %v", got[0], slow.reads, time.Since(started))
			return // not Fatalf: this is not the test's goroutine
		}
		wallAfter, ticksAfter, absoluteAfter := clocks()
		for what, passed := range map[string]time.Duration{"gettimeofday": wallAfter.Sub(wall), "gettimeofday's Mach time": tickDuration(int64(ticksAfter - ticks)),
			"mach_absolute_time": tickDuration(int64(absoluteAfter - absolute))} {
			if passed < 0 || passed >= delay/2 {
				t.Errorf("%v passed by %s over a fetch of %v", passed, what, delay)
			}
		}

		// The wait is not over: its time has passed for the host only.
		p.current.pc = codeAddr + 0x100
		p.m.schedule()
		if main.state != waiting || p.current != other {
			t.Errorf("after a fetch of %v, a wait of %v: state %d, thread %d has the turn", delay, patience, main.state, p.current.id)
			return // not Fatalf: this is not the test's goroutine
		}
		// It ends when what it waits for comes, or when its own time has passed.
		p.trap(-33, sem) // semaphore_signal
		if main.state != runnable {
			t.Errorf("after the signal: state %d", main.state)
		}
		p.end(other)
		p.m.schedule()
		if p.current != main || p.x(0) != 0 {
			t.Errorf("the wait ended with %d on thread %d", p.x(0), p.current.id)
		}
		p.trap(-38, sem, 0, uint64(30*time.Millisecond))
		p.current.pc = codeAddr + 4
		p.m.schedule()
		if p.x(0) != kernTimedOut {
			t.Errorf("a wait nothing ends: x0 %d", p.x(0))
		}
	})
}

// The run's timeout, and the time the others have once the guest has exited, are time the
// guests had: code that touches a page the host is slow to fetch, and then never stops, is
// stopped that long after the fetch.
func TestFetchTakesNoneOfTheTimeout(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		const delay, limit = 600 * time.Millisecond, 250 * time.Millisecond
		q, err := p.m.newProcess(p.exePath, p.exe, GuestExe)
		if err != nil {
			t.Fatal(err)
		}
		// spin runs ldr x0, [x1]; b . in a process, with x1 on a slow page, until it is stopped.
		spin := func(in *Process) (went bool, stopped any, took, guest time.Duration) {
			slow := &slowFile{delay: delay}
			in.write(codeAddr, le32(0xF9400020, 0x14000000))
			in.setX(1, mapSlow(t, in, slow))
			in.current.pc, in.m.running = codeAddr, in
			started, clock := time.Now(), in.m.clock.now()
			defer func() {
				stopped = recover()
				took, guest = time.Since(started), in.m.clock.now().Sub(clock)
				if slow.reads != 1 || in.x(0)&0xFF != 0x5A {
					t.Errorf("the page was read %d times, and x0 is %#x", slow.reads, in.x(0))
				}
			}()
			return in.step(), nil, 0, 0
		}
		check := func(what string, took, guest time.Duration) {
			if took < delay+limit*4/5 || guest < limit*4/5 || guest > took-delay*4/5 {
				t.Errorf("%s: stopped after %v, %v of it the guests', with a fetch of %v and %v to run", what, took, guest, delay, limit)
			}
		}

		p.m.deadline = p.m.clock.now().Add(limit)
		_, stopped, took, guest := spin(q)
		if s, ok := stopped.(stop); !ok || s.kind != StopTimeout {
			t.Fatalf("the run's deadline stopped it with %v", stopped)
		}
		check("the timeout", took, guest)

		// The engine of a process that timed out is not started again: the other one spins.
		p.m.deadline = p.m.clock.now().Add(time.Minute)
		q.exited = true
		p.m.procs[0], p.m.procs[1] = q, p // the guest the run is for has exited, and p is what is left
		p.m.settling = p.m.clock.now().Add(limit)
		went, stopped, took, guest := spin(p)
		p.m.procs[0], p.m.procs[1] = p, q
		if went || stopped != nil {
			t.Fatalf("the end of the settling time: step %v, stopped with %v", went, stopped)
		}
		check("the settling time", took, guest)
	})
}
