package emulator

import "time"

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
// here, and no thread is ever made to give the CPU up. Of its children (-1) it is what those it
// has waited for used, and theirs: nothing of one that has not been waited for.
func sysGetrusage(p *Process, a []uint64) int64 {
	const self, children = 0, -1
	switch int32(a[0]) {
	case children:
		p.write(a[1], p.childrenUsed.bytes())
	case self:
		p.write(a[1], p.used().bytes())
	default:
		return -eINVAL
	}
	return 0
}

// used is what struct rusage says of a process, or of the children it has waited for.
type used struct {
	user, system        time.Duration
	most                uint64 // bytes of memory at the most
	minor, major, waits uint64
}

// used is what a process has used up to now.
func (p *Process) used() used {
	u := p.usage
	u.enter(p.m.clock, true)
	// The times are whole microseconds, as struct rusage has them, so that they add up as a Mac's do.
	return used{u.user.Truncate(time.Microsecond), u.system.Truncate(time.Microsecond), uint64(max(u.peak, len(p.mapped))) * pageSize, u.minor, u.major, p.waits}
}

// add counts in a child that has been waited for, and its own children: the times and counts
// together, and of memory the most that any one had.
func (u *used) add(child used) {
	u.user, u.system = u.user+child.user, u.system+child.system
	u.most = max(u.most, child.most)
	u.minor, u.major, u.waits = u.minor+child.minor, u.major+child.major, u.waits+child.waits
}

func (u used) bytes() []byte {
	out := make([]byte, 144)
	timeval := func(at int, d time.Duration) {
		le.PutUint64(out[at:], uint64(d/time.Second))
		le.PutUint32(out[at+8:], uint32(d%time.Second/time.Microsecond))
	}
	timeval(0, u.user)
	timeval(16, u.system)
	copy(out[32:], le64(u.most, 0, 0, 0, u.minor, u.major, 0, 0, 0, 0, 0, 0, u.waits, 0))
	return out
}
