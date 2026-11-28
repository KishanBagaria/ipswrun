package emulator

import (
	"fmt"
	"slices"
	"time"
)

// A kqueue of a guest's own, made with kqueue and asked with kevent: events it makes happen
// itself, timers, the ends of pipes, and processes ending. Each thing watched is a note, kept
// as it was asked for; kevent takes changes to the notes and returns the ones that have
// something to say, waiting for one if it is told to. libdispatch's kqueues, which have no
// descriptor and hand their events to threads of the work queue, are in kevent.go. What a Mac's
// kernel does was measured with tests/kernel/io/kqueue.c.

const (
	noteControl = 0xC0000000 // of a user's event: what a change does with its flags
	noteAnd     = 0x40000000
	noteOr      = 0x80000000
	noteCopy    = 0xC0000000
	noteFlags   = 0x00FFFFFF

	keventSize32 = 32 // struct kevent
	keventSize64 = 48 // struct kevent64_s
)

// userKqueue is a kqueue with a descriptor.
type userKqueue struct {
	notes []*knote
	size  uint64 // of the events it was first asked with: it is asked with no other
}

// knote is one thing a kqueue watches.
type knote struct {
	event    kevent // as it was asked for, which is how it is returned
	flags    uint32 // of a user's event, or what to watch a process for
	disabled bool
	active   bool          // a user's event that was made to happen
	deadline time.Time     // of a timer: when it is next due,
	period   time.Duration // how far apart, and
	once     bool          // whether it is due only once, which it then was
	seen     uint64        // of a pipe watched for what is new: how much had happened to it when last told
	never    bool          // nothing here says when it happens (see sysKevent)
	version  uint32        // of a watched process: which of its programs it was when last looked at
}

func sysKqueue(p *Process, a []uint64) int64 {
	if p.full() {
		return -eMFILE
	}
	return int64(p.newFD(&fd{kind: dataFD, kq: &userKqueue{}}))
}

// keventWait is a kevent that waits: on which kqueue, and where its events go.
type keventWait struct {
	q            *userKqueue
	events, room uint64
}

func sysKevent(p *Process, a []uint64) int64 { return p.userKevent(a, keventSize32, a[5]) }

func sysKevent64(p *Process, a []uint64) int64 {
	if a[5] != 0 { // KEVENT_FLAG_IMMEDIATE and the like
		p.unimplemented("kevent64 with flags", a)
		return -eNOSYS
	}
	return p.userKevent(a, keventSize64, a[6])
}

// userKevent is kevent(kqueue, changes, how many, events, room for how many, timeout).
func (p *Process) userKevent(a []uint64, size, timeout uint64) int64 {
	f := p.fds[a[0]]
	if f == nil || f.kq == nil {
		return -eBADF
	}
	q, nchanges, room := f.kq, int(int32(a[2])), int(int32(a[4]))
	if nchanges < 0 || room < 0 || q.size != 0 && q.size != size {
		return -eINVAL
	}
	q.size = size
	// A change that fails, or that asks for a receipt, is answered in the events if there is
	// room, and otherwise the first failure is the call's.
	var answers []kevent
	for i := range nchanges {
		c := p.readKevent(a[1]+uint64(i)*size, size)
		p.note("  kevent on descriptor %d: filter %d ident %#x flags %#x fflags %#x data %#x", a[0], c.filter, c.ident, c.flags, c.fflags, uint64(c.data))
		errno := p.changeNote(q, c)
		switch {
		case errno == 0 && c.flags&evReceipt == 0:
		case len(answers) < room:
			c.flags, c.data = c.flags|evError, -errno
			answers = append(answers, c)
		case errno != 0:
			return errno
		}
	}
	if len(answers) > 0 || room == 0 {
		p.writeUserKevents(a[3], answers, size)
		return int64(len(answers))
	}
	if events := p.pollNotes(q, room); len(events) > 0 {
		p.writeUserKevents(a[3], events, size)
		return int64(len(events))
	}
	var deadline time.Time
	if timeout != 0 {
		if deadline = p.after(relative(p.u64(timeout), p.u64(timeout+8))); deadline.IsZero() {
			return 0
		}
	}
	// To wait for what nothing here makes happen would be to wait for good, where a Mac may not.
	if at := slices.IndexFunc(q.notes, func(n *knote) bool { return n.never && !n.disabled }); at >= 0 {
		p.unimplemented(fmt.Sprintf("kevent that waits for events of filter %d", q.notes[at].event.filter), a)
		return -eNOSYS
	}
	p.current.kevent = &keventWait{q: q, events: a[3], room: uint64(room)}
	p.wait(waitKevent, 0, deadline, result{})
	return 0
}

func (p *Process) readKevent(addr, size uint64) kevent {
	b := p.read(addr, size)
	c := kevent{ident: le.Uint64(b), filter: int16(le.Uint16(b[8:])), flags: le.Uint16(b[10:]), fflags: le.Uint32(b[12:]), data: int64(le.Uint64(b[16:])), udata: le.Uint64(b[24:])}
	if size == keventSize64 {
		c.ext[0], c.ext[1] = le.Uint64(b[32:]), le.Uint64(b[40:])
	}
	return c
}

func (p *Process) writeUserKevents(addr uint64, events []kevent, size uint64) {
	var out []byte
	for _, e := range events {
		b := le64(e.ident, 0, uint64(e.data), e.udata, e.ext[0], e.ext[1])
		le.PutUint16(b[8:], uint16(e.filter))
		le.PutUint16(b[10:], e.flags)
		le.PutUint32(b[12:], e.fflags)
		out = append(out, b[:size]...)
	}
	if len(out) > 0 {
		p.write(addr, out)
	}
}

// changeNote makes one change to a kqueue: a note added, deleted, or told something. It returns
// the error the change fails with.
func (p *Process) changeNote(q *userKqueue, c kevent) int64 {
	at := slices.IndexFunc(q.notes, func(n *knote) bool { return n.event.ident == c.ident && n.event.filter == c.filter })
	switch {
	case c.flags&evDelete != 0:
		if at < 0 {
			return -eNOENT
		}
		q.notes = slices.Delete(q.notes, at, at+1)
		return 0
	case at < 0 && c.flags&evAdd == 0:
		return -eNOENT
	case at < 0:
		n := &knote{event: c, flags: c.fflags, disabled: c.flags&evDisable != 0}
		if errno := p.newNote(n); errno != 0 {
			return errno
		}
		q.notes = append(q.notes, n)
		return 0
	}
	// A note that is there: it keeps the flags it was added with, and is told the rest.
	n := q.notes[at]
	n.event.udata, n.event.ext = c.udata, c.ext
	switch {
	case c.flags&evEnable != 0:
		n.disabled = false
	case c.flags&evDisable != 0:
		n.disabled = true
	}
	switch n.event.filter {
	case filterUser:
		switch value := c.fflags & noteFlags; c.fflags & noteControl {
		case noteAnd:
			n.flags &= value
		case noteOr:
			n.flags |= value
		case noteCopy:
			n.flags = value
		}
		if c.fflags&noteTrigger != 0 {
			n.active = true
		}
	case filterTimer:
		if c.flags&evAdd != 0 { // set again, from now
			n.event.fflags, n.event.data, n.once = c.fflags, c.data, false
			p.armNote(n)
		}
	}
	return 0
}

// newNote sets up a note that was just asked for.
func (p *Process) newNote(n *knote) int64 {
	c := n.event
	switch c.filter {
	case filterUser:
		n.active = c.fflags&noteTrigger != 0
	case filterTimer:
		n.event.flags |= evClear
		p.armNote(n)
	case filterRead, filterWrite:
		f := p.fds[c.ident]
		if f == nil {
			return -eBADF
		}
		// Of a descriptor that is no pipe's, or the end of one that is not the one read, or
		// written, nothing here says when it can be.
		if f.kind == ttyFD && f.own { // /dev/tty is nothing a kqueue watches, as measured
			return -eINVAL
		}
		n.never = !f.watchable()
		if !n.never {
			if _, _, _, applies := f.filtered(c.filter, c); !applies {
				n.never = true
			} else {
				n.seen = f.changes()
			}
		}
	case filterProc:
		if p.m.process(uint32(c.ident)) == nil {
			return -eSRCH
		}
		n.never = c.fflags&^(noteExit|noteExitStatus|noteExec) != 0 || c.fflags&noteExit == 0
		n.version = p.m.process(uint32(c.ident)).version
	case filterSignal, -4, filterMachport: // signals, files and ports: accepted, as they were
		n.never = true
	default:
		return -eINVAL
	}
	if n.never {
		p.ignored[int(c.filter)]++
	}
	return 0
}

// armNote sets a timer's note to be due, from now.
func (p *Process) armNote(n *knote) {
	c := n.event
	n.deadline = p.timerDeadline(c)
	n.period = n.deadline.Sub(p.m.clock.now())
	if c.fflags&noteAbsolute != 0 || c.flags&evOneshot != 0 {
		n.period = 0
	}
}

// pollNotes are the events a kqueue has now, up to as many as there is room for, each taken as
// its note says: once, or until it is enabled again, or as often as it is asked for.
func (p *Process) pollNotes(q *userKqueue, room int) []kevent {
	now := p.m.clock.now()
	var events []kevent
	for _, n := range slices.Clone(q.notes) {
		if len(events) == room {
			break
		}
		if n.disabled || n.never {
			continue
		}
		event, edge, gone := n.event, n.event.flags&evClear != 0, false
		event.fflags, event.data = 0, 0
		switch n.event.filter {
		case filterUser:
			if !n.active {
				continue
			}
			event.fflags = n.flags
			if edge {
				n.active = false
			}
		case filterTimer:
			if n.once || n.deadline.After(now) {
				continue
			}
			event.data = 1
			if n.period > 0 {
				late := now.Sub(n.deadline) / n.period
				event.data += int64(late)
				n.deadline = n.deadline.Add((late + 1) * n.period)
			} else {
				n.once = true
			}
		case filterRead, filterWrite:
			f := p.fds[n.event.ident]
			if f == nil || !f.watchable() { // closed since: what watched it went with it
				gone = true
				break
			}
			data, ended, least, _ := f.filtered(n.event.filter, n.event)
			if event.data = data; !ended && data < least {
				continue
			}
			if edge && n.seen == f.changes() {
				continue // nothing new
			}
			n.seen = f.changes()
			if ended {
				event.flags |= evEOF
			}
		case filterProc:
			ended := p.m.ended(uint32(n.event.ident))
			if ended == nil {
				// It goes on: has it become another program since this was last asked?
				now := p.m.process(uint32(n.event.ident))
				if now == nil || n.flags&noteExec == 0 || now.version == n.version {
					continue
				}
				n.version = now.version
				event.flags |= evClear
				event.fflags = noteExec
				break
			}
			event.flags |= evEOF | evOneshot | evClear
			if event.fflags = noteExit; n.flags&noteExitStatus != 0 {
				event.fflags |= noteExitStatus
				event.data = int64(ended.status())
			}
		}
		if gone || event.flags&evOneshot != 0 {
			q.notes = slices.DeleteFunc(q.notes, func(other *knote) bool { return other == n })
		}
		if gone {
			continue
		}
		if n.event.flags&evDispatch != 0 {
			n.disabled = true
		}
		events = append(events, event)
	}
	return events
}

// ended is the process with a pid that has exited, if there is one.
func (m *Machine) ended(pid uint32) *Process {
	for _, p := range m.procs {
		if p.pid == pid && p.exited {
			return p
		}
	}
	return nil
}

// serveKevents gives the threads that wait in kevent the events their kqueues now have, and
// reports when the next timer of a kqueue that is waited on is due.
func (p *Process) serveKevents() (next time.Time) {
	for _, t := range p.threads {
		w := t.kevent
		if t.state != waiting || t.kind != waitKevent || w == nil {
			continue
		}
		if events := p.pollNotes(w.q, int(w.room)); len(events) > 0 {
			p.writeUserKevents(w.events, events, w.q.size)
			t.kevent = nil
			t.answer = &result{x0: uint64(len(events))}
			t.wake()
			continue
		}
		for _, n := range w.q.notes {
			if n.event.filter == filterTimer && !n.disabled && !n.once {
				next = earlier(next, n.deadline)
			}
		}
	}
	return next
}
