package emulator

import (
	"sync"
	"time"
)

// clock is the machine's time: the one the guests read, wait by and are timed out on. It is the
// host's with what the host spent away taken out: a guest's pages and files are fetched on first
// touch, over the network when the host has not kept them, and a fetch of seconds or minutes is
// not time any guest had. The clock stands still for it (see away), so a guest that waits five
// seconds for another gets the same five seconds whether the host had to download or not.
//
// A time of the machine's is a time.Time like the host's, and only ever compared with another of
// the machine's: now, not time.Now, is what a deadline is made from and held against. The wall
// clock the guests see is behind the host's by as much as the host was away.
//
// Mach time is the same clock in the ticks of an Apple silicon Mac: 24 MHz, which
// mach_timebase_info reports as 125/3 ns a tick and newer system libraries take for granted. It
// starts at the host's uptime. The emulated CPU's own counter runs at another rate, so the
// commpage tells libsystem not to read it and to ask the kernel instead (see interrupt).
type clock struct {
	mu      sync.Mutex // the alarm of a stretch of emulation reads the clock from a goroutine of its own
	lost    time.Duration
	stopped time.Time // by the host's clock, while the host is away; zero otherwise
	boot    time.Time // when Mach time was zero
}

func newClock() *clock {
	return &clock{boot: time.Now().Add(-time.Duration(uptimeNanos()))}
}

// now is the machine's time.
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.stopped
	if t.IsZero() {
		t = time.Now()
	}
	return t.Add(-c.lost)
}

// away is what the host fetches for a guest, and not the guest's time: what backs its memory and
// its system files. The clock is where it was when that returns.
func away[T any](c *clock, fetch func() (T, error)) (T, error) {
	c.mu.Lock()
	c.stopped = time.Now()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.lost += time.Since(c.stopped)
		c.stopped = time.Time{}
		c.mu.Unlock()
	}()
	return fetch()
}

// ticks is the guests' mach_absolute_time now.
func (c *clock) ticks() uint64 { return uint64(c.now().Sub(c.boot)) * 3 / 125 }

// tickDuration is how long a number of ticks lasts.
func tickDuration(ticks int64) time.Duration { return time.Duration(ticks * 125 / 3) }

// tickTime is when the guests' mach_absolute_time is, or was, ticks.
func (c *clock) tickTime(ticks uint64) time.Time { return c.boot.Add(tickDuration(int64(ticks))) }

// alarm calls ring once the clock has reached t, and again every millisecond for as long as it
// is left on: what it rings for may not have started yet. The function returned turns it off,
// and says whether it rang.
func (c *clock) alarm(t time.Time, ring func()) (off func() bool) {
	quit, rang := make(chan struct{}), make(chan bool)
	go func() {
		due := false
		for {
			left := t.Sub(c.now())
			if left <= 0 {
				due = true
				ring()
			}
			select {
			case <-quit:
				rang <- due
				return
			case <-time.After(max(left, time.Millisecond)):
			}
		}
	}()
	return func() bool {
		close(quit)
		return <-rang
	}
}
