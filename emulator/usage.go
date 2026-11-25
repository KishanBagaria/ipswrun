package emulator

import (
	"slices"
	"time"
)

// usage is what a process has used of the machine, as getrusage reports it. It is the machine's
// own account, as its clock is its own: the time the process's threads had the emulated CPU, by
// the machine's clock, which stands still while the host fetches; the pages brought in for it;
// and how often a thread of its gave the CPU up to wait.
type usage struct {
	user, system time.Duration
	since        time.Time // when the thread that has the CPU got to where it is; zero if none has
	kernel       bool      // it is in the kernel: a system call, a trap, or a fault
	minor, major uint64    // faults that brought pages in, and those of them that read a file
	peak         int       // the most pages the process has had
}

// start is a thread of the process getting the CPU, and stop its giving it up.
func (u *usage) start(c *clock) { u.since, u.kernel = c.now(), false }

func (u *usage) stop(c *clock) {
	u.enter(c, false)
	u.since = time.Time{}
}

// enter is the thread going into the kernel, or back out of it.
func (u *usage) enter(c *clock, kernel bool) {
	if u.since.IsZero() {
		return
	}
	now := c.now()
	if u.kernel {
		u.system += now.Sub(u.since)
	} else {
		u.user += now.Sub(u.since)
	}
	u.since, u.kernel = now, kernel
}

// fault counts a fault that brought pages in, of which the process now has so many.
func (u *usage) fault(read bool, pages int) {
	if read {
		u.major++
	} else {
		u.minor++
	}
	u.peak = max(u.peak, pages)
}

// sysGetrusage is getrusage(who, &usage). Of the process itself (0) it is the machine's account
// of it. What a Mac's kernel reports as zero whatever a process does is zero: the sizes of its
// text, data and stack, swaps, block input and output, and messages. Signals are never delivered
// here, and no thread is ever made to give the CPU up. Of its children (-1) it is all zeros, as
// a process here has none.
func sysGetrusage(p *Process, a []uint64) int64 {
	const self, children = 0, -1
	out := make([]byte, 144)
	switch int32(a[0]) {
	case children:
		if len(p.m.procs) > 0 && slices.ContainsFunc(p.m.procs, func(q *Process) bool { return q.parent == p }) {
			p.unimplemented("getrusage of the children of a process that has started some", a)
			return -eINVAL
		}
	case self:
		out = p.usageBytes()
	default:
		return -eINVAL
	}
	p.write(a[1], out)
	return 0
}

// usageBytes is the struct rusage of a process, up to now.
func (p *Process) usageBytes() []byte {
	out := make([]byte, 144)
	u := p.usage
	u.enter(p.m.clock, true)
	timeval := func(at int, d time.Duration) {
		le.PutUint64(out[at:], uint64(d/time.Second))
		le.PutUint32(out[at+8:], uint32(d%time.Second/time.Microsecond))
	}
	timeval(0, u.user)
	timeval(16, u.system)
	counts := []uint64{uint64(max(u.peak, len(p.mapped))) * pageSize, 0, 0, 0, u.minor, u.major, 0, 0, 0, 0, 0, 0, p.waits, 0}
	copy(out[32:], le64(counts...))
	return out
}
