package emulator

import (
	"maps"
	"slices"
	"time"
)

// libdispatch gets its events from the kernel as kevents, and gets threads to handle them on from
// the work queue. Three kinds of event are emulated: timers, a Mach message arriving at a port
// (see ports.go), and the requests a serial queue (a "workloop") makes for a thread to drain it or
// for a turn at owning it. Each goes to the one work queue kqueue every process has, or to the
// kqueue of one workloop, named by its id.

// kevent is struct kevent_qos_s.
type kevent struct {
	ident  uint64
	filter int16
	flags  uint16
	qos    uint32
	udata  uint64
	fflags uint32
	xflags uint32
	data   int64
	ext    [4]uint64
}

const (
	keventSize = 72

	filterSignal   = -6
	filterTimer    = -7
	filterMachport = -8
	filterUser     = -10
	filterMemory   = -14 // memory pressure: never happens here
	filterWorkloop = -17

	evAdd      = 0x1
	evDelete   = 0x2
	evEnable   = 0x4
	evDisable  = 0x8
	evOneshot  = 0x10
	evDispatch = 0x80  // disabled once it has happened, until enabled again
	evVanished = 0x200 // asked for: to be told if what is watched goes away. In an event: it did

	// Units and clocks of a timer.
	noteSeconds  = 0x1
	noteUseconds = 0x2
	noteNseconds = 0x4
	noteAbsolute = 0x8
	noteMachTime = 0x100

	noteTrigger = 0x01000000 // makes a user event happen

	// What a workloop event asks for.
	noteThreadRequest = 0x1
	noteSyncWait      = 0x4
	noteSyncWake      = 0x8

	keventWorkq = 0x20 // KEVENT_FLAG_WORKQ: the changes are for the work queue's kqueue
)

func (p *Process) readKevents(addr, n uint64) []kevent {
	out := make([]kevent, min(n, 64))
	all := p.read(addr, uint64(len(out))*keventSize)
	for i := range out {
		b := all[i*keventSize:]
		out[i] = kevent{ident: le.Uint64(b), filter: int16(le.Uint16(b[8:])), flags: le.Uint16(b[10:]), qos: le.Uint32(b[12:]),
			udata: le.Uint64(b[16:]), fflags: le.Uint32(b[24:]), xflags: le.Uint32(b[28:]), data: int64(le.Uint64(b[32:])),
			ext: [4]uint64{le.Uint64(b[40:]), le.Uint64(b[48:]), le.Uint64(b[56:]), le.Uint64(b[64:])}}
	}
	return out
}

func (p *Process) writeKevents(addr uint64, events []kevent) {
	b := make([]byte, 0, len(events)*keventSize)
	for _, event := range events {
		b = append(b, event.bytes()...)
	}
	p.write(addr, b)
}

func (k kevent) bytes() []byte {
	b := le64(k.ident, 0, k.udata, 0, uint64(k.data), k.ext[0], k.ext[1], k.ext[2], k.ext[3])
	le.PutUint16(b[8:], uint16(k.filter))
	le.PutUint16(b[10:], k.flags)
	le.PutUint32(b[12:], k.qos)
	le.PutUint32(b[24:], k.fflags)
	le.PutUint32(b[28:], k.xflags)
	return b
}

// kqueue is the work queue's kqueue (id 0) or a workloop's.
type kqueue struct {
	id      uint64
	timers  map[uint64]*timer
	users   map[uint64]kevent   // events that happen when the guest says so
	signals map[uint64]kevent   // the signals it is to be told of, by number
	ports   map[watchKey]kevent // the ports and sets it watches (see ports.go)
	ready   []kevent            // events that happened and have not been handed to a thread yet

	// Of a workloop: its request for a thread while that waits to be answered, the thread that
	// answered the last one, and the turns given to threads that had not asked yet.
	request  *kevent
	servicer *thread
	woken    map[uint64]bool
}

type timer struct {
	event    kevent
	deadline time.Time
	armed    bool
}

func (p *Process) kqueue(id uint64) *kqueue {
	q := p.kqueues[id]
	if q == nil {
		q = &kqueue{id: id, timers: map[uint64]*timer{}, users: map[uint64]kevent{}, signals: map[uint64]kevent{}, ports: map[watchKey]kevent{}, woken: map[uint64]bool{}}
		p.kqueues[id] = q
	}
	return q
}

// sysKeventQos takes changes for the work queue's kqueue. Other kqueues are not emulated: their
// changes are accepted, and nothing ever happens on them.
func sysKeventQos(p *Process, a []uint64) int64 {
	if int32(a[0]) != -1 || a[7]&keventWorkq == 0 {
		return 0
	}
	return p.kevents(p.kqueue(0), a)
}

func sysKeventID(p *Process, a []uint64) int64 { return p.kevents(p.kqueue(a[0]), a) }

// kevents applies the changes at a[1], then gives the caller what is ready, if it asked.
func (p *Process) kevents(q *kqueue, a []uint64) int64 {
	p.change(q, p.readKevents(a[1], a[2]))
	if p.current.state != runnable || a[3] == 0 || int32(a[4]) <= 0 || q.servicer != p.current {
		return 0
	}
	n := min(len(q.ready), int(int32(a[4])))
	p.writeKevents(a[3], q.ready[:n])
	q.ready = q.ready[n:]
	return int64(n)
}

func (p *Process) change(q *kqueue, changes []kevent) {
	for _, c := range changes {
		p.note("  kevent on %#x: filter %d ident %#x flags %#x qos %#x fflags %#x data %#x", q.id, c.filter, c.ident, c.flags, c.qos, c.fflags, uint64(c.data))
		switch c.filter {
		case filterTimer:
			p.changeTimer(q, c)
		case filterWorkloop:
			p.changeWorkloop(q, c)
		case filterUser:
			registered, ok := q.users[c.ident]
			switch {
			case c.flags&evDelete != 0:
				delete(q.users, c.ident)
			case c.flags&evAdd != 0:
				q.users[c.ident], registered, ok = c, c, true
			}
			if ok && c.fflags&noteTrigger != 0 {
				q.ready = append(q.ready, registered)
			}
		case filterSignal:
			if c.flags&evDelete != 0 {
				delete(q.signals, c.ident)
			} else if c.flags&evAdd != 0 {
				q.signals[c.ident] = c
			}
		case filterMachport:
			p.changeMachport(q, c)
		case filterMemory:
		default:
			p.ignored[int(c.filter)]++
		}
	}
}

func (p *Process) changeTimer(q *kqueue, c kevent) {
	t := q.timers[c.ident]
	switch {
	case c.flags&evDelete != 0:
		delete(q.timers, c.ident)
	case c.flags&evAdd != 0:
		q.timers[c.ident] = &timer{event: c, deadline: p.timerDeadline(c), armed: c.flags&evDisable == 0}
	case t != nil && c.flags&evEnable != 0:
		t.armed = true
	case t != nil && c.flags&evDisable != 0:
		t.armed = false
	}
}

// timerDeadline is when a timer is due, from its data in the unit and on the clock its flags name.
func (p *Process) timerDeadline(c kevent) time.Time {
	var unit time.Duration
	switch {
	case c.fflags&noteMachTime != 0:
	case c.fflags&noteSeconds != 0:
		unit = time.Second
	case c.fflags&noteUseconds != 0:
		unit = time.Microsecond
	case c.fflags&noteNseconds != 0:
		unit = time.Nanosecond
	default:
		unit = time.Millisecond
	}
	switch {
	case c.fflags&noteAbsolute == 0 && unit == 0:
		return p.m.clock.now().Add(tickDuration(c.data))
	case c.fflags&noteAbsolute == 0:
		return p.m.clock.now().Add(time.Duration(c.data) * unit)
	case unit != 0:
		return time.Unix(0, c.data*int64(unit)) // the wall clock
	}
	return p.m.clock.tickTime(uint64(c.data)) // either Mach clock: no time is spent asleep to tell them apart
}

func (p *Process) changeWorkloop(q *kqueue, c kevent) {
	switch {
	case c.fflags&noteThreadRequest != 0 && c.flags&evDelete != 0:
		q.request = nil
	case c.fflags&noteThreadRequest != 0 && p.stale(c):
		// The queue is no longer in the state its owner asked for a thread in: another thread
		// took the work meanwhile, and one started now would find a queue that is not its own.
		p.note("  the workloop's state has changed since: no thread")
	case c.fflags&noteThreadRequest != 0:
		request := c
		q.request = &request
	case c.fflags&noteSyncWake != 0:
		// The thread given the workloop may not have asked for it yet.
		for _, t := range p.waiters(waitWorkloop, q.id) {
			if uint64(t.port) == c.ident {
				t.wake()
				return
			}
		}
		q.woken[c.ident] = true
	case c.fflags&noteSyncWait != 0:
		if q.woken[c.ident] {
			delete(q.woken, c.ident)
		} else {
			p.wait(waitWorkloop, q.id, time.Time{}, result{})
		}
	}
}

// stale reports whether a workloop event is out of date. Such an event may say where the state
// of its queue is, and what it was, under a mask, when the event was made: the kernel acts on
// the event only if it still is.
func (p *Process) stale(c kevent) bool {
	addr, mask, value := c.ext[1], c.ext[2], c.ext[3]
	return addr != 0 && p.u64(addr)&mask != value&mask
}

// signalled tells every kqueue of the process that watches for a signal that it was sent, and
// reports whether any does. The only signal there is anyone to send is the one that asks a
// process to end (see Machine.exit): nothing is done to a process that does not watch for it.
func (p *Process) signalled(number uint64) bool {
	told := false
	for _, q := range p.kqueues {
		if event, ok := q.signals[number]; ok {
			event.flags &^= evAdd | evEnable
			event.data = 1 // how many times
			q.ready = append(q.ready, event)
			told = true
		}
	}
	return told
}

// serve starts work queue threads for the kqueues that have something for one, and reports when
// the next timer is due (zero: none is armed).
func (p *Process) serve(now time.Time) (next time.Time) {
	next = p.fireTimers(now)
	for _, id := range slices.Sorted(maps.Keys(p.kqueues)) {
		q := p.kqueues[id]
		for ident, t := range q.timers {
			if !t.armed {
				continue
			}
			if t.deadline.After(now) {
				next = earlier(next, t.deadline)
				continue
			}
			fired := t.event
			fired.data = 1 // how many times it fired
			q.ready = append(q.ready, fired)
			switch {
			case t.event.flags&evOneshot != 0:
				delete(q.timers, ident)
			case t.event.fflags&noteAbsolute != 0:
				t.armed = false
			default:
				t.deadline = p.timerDeadline(t.event)
			}
		}
		p.watched(q)
		if q.servicer != nil || q.request == nil && len(q.ready) == 0 {
			continue
		}
		events, priority := q.ready, uint64(0)
		if q.request != nil {
			events = append([]kevent{*q.request}, events...)
		}
		later := events[min(len(events), workerEvents):] // more than a thread is given at once
		events = events[:len(events)-len(later)]
		for _, event := range events {
			priority = max(priority, uint64(event.qos))
		}
		if t := p.startWorker(priority, q, events); t != nil {
			q.ready, q.request = slices.Clone(later), nil
			if q.id != 0 {
				q.servicer = t
			}
		}
	}
	return next
}

// finish is a work queue thread saying it is done with what it was started for, with the changes
// it has for the kqueue it served.
func (p *Process) finish(t *thread, changes, n uint64) {
	q := t.serving
	if q == nil {
		q = p.kqueue(0)
	}
	p.change(q, p.readKevents(changes, n))
	if q.servicer == t {
		q.servicer = nil
	}
	t.serving = nil
	if t.state == runnable { // unless one of the changes was to wait
		t.state = parked
	}
}
