package emulator

import (
	"slices"
	"time"
)

// Ports the guest holds the receive right of are real: a message sent to one waits in its queue
// until a thread receives it, and a thread that receives from an empty one waits for a message.
// There is one process and so one name space, as for a task that talks to itself: a port's
// receive right and its send rights share a name, the send rights are counted, and each right to
// send once has a name of its own. Everything else a message can be sent to is the kernel's (the
// task, the host, a clock) and is answered at once, as a MIG routine.

// port is a receive right, or a port set.
type port struct {
	name    uint32
	set     bool
	in      uint32        // the set it is a member of, if any
	queue   []machMessage // as their receiver will see them
	armed   bool          // of a timer (mk_timer): due at
	due     time.Time
	context uint64 // what it was made or guarded with, which its owner reads back

	sends     int    // send rights there are, those in messages not yet received among them
	made      uint32 // send rights ever made from the receive right
	noSenders *port  // told when the last send right goes, if anyone asked
	deadName  *port  // told when the receive right goes while send rights remain
}

// machMessage is a queued message, and who sent it: the guest, the kernel (pid 0) or launchd (1).
type machMessage struct {
	data   []byte
	aux    []byte // what was sent beside it, if anything
	sender uint32
	moved  bool // the send right it was sent with was given up for it, and ends when it is received
}

const (
	machSendMsg          = 0x1
	machRcvMsg           = 0x2
	machRcvLarge         = 0x4
	machRcvLargeIdentity = 0x8
	machRcvTimeout       = 0x100
	msgVector            = 1 << 32 // MACH64_MSG_VECTOR: the message is given as a vector of two

	rcvTooLarge    = 0x10004004
	rcvInvalidData = 0x10004008
	rcvPortDied    = 0x10004009

	bitsComplex = 0x80000000

	// How a right is named in a message: what the sender does with its own, and what arrives.
	moveReceive  = 16
	moveSend     = 17
	moveSendOnce = 18
	copySend     = 19
	makeSend     = 20
	makeSendOnce = 21

	rightSend     = 0
	rightReceive  = 1
	rightSendOnce = 2
	rightPortSet  = 3

	notifyNoSenders = 70
	notifySendOnce  = 71
	notifyDeadName  = 72

	insertSendRight = 0x10 // MPO_INSERT_SEND_RIGHT
)

// newReceive makes a port the guest can receive from.
func (p *Process) newReceive() *port {
	q := &port{name: p.newPort()}
	p.ports[q.name] = q
	return q
}

// receiver is the port or set a name the guest receives from stands for. A name nothing made is
// taken to be a port of its own that nothing was sent to yet.
func (p *Process) receiver(name uint32) *port {
	q := p.ports[name]
	if q == nil {
		q = &port{name: name}
		p.ports[name] = q
	}
	return q
}

// readable is the port with the next message for a receive from q: itself, or for a set the
// member with the lowest name that has one.
func (p *Process) readable(q *port) *port {
	if !q.set {
		if len(q.queue) > 0 {
			return q
		}
		return nil
	}
	var first *port
	for _, member := range p.ports {
		if member.in == q.name && len(member.queue) > 0 && (first == nil || member.name < first.name) {
			first = member
		}
	}
	return first
}

// destination is the port a message to name goes to: one of the guest's own by its name, or by
// the name of a right to send to it once, which is then used up.
func (p *Process) destination(name uint32) *port {
	if q := p.once[name]; q != nil {
		delete(p.once, name)
		return q
	}
	if q := p.ports[name]; q != nil && !q.set {
		return q
	}
	return nil
}

// sendOnce names a new right to send to q once.
func (p *Process) sendOnce(q *port) uint32 {
	name := p.newPort()
	p.once[name] = q
	return name
}

// notify sends q one of the kernel's notifications.
func (p *Process) notify(q *port, id uint32, body ...byte) {
	if p.ports[q.name] == q {
		p.post(q, machMessage{data: append(header(moveSendOnce<<8, uint32(24+len(body)), q.name, id), body...)})
	}
}

// release ends one send right to q, and tells who asked to know when the last one has gone.
func (p *Process) release(q *port) {
	if q.sends > 0 {
		q.sends--
	}
	if q.sends == 0 && q.noSenders != nil {
		to := q.noSenders
		q.noSenders = nil
		p.notify(to, notifyNoSenders, append(slices.Clone(ndr), le32(q.made)...)...)
	}
}

// transfer accounts for a right named in a message that is being sent, and returns the name it
// arrives under: its own, but for a right to send once, which is always a new one.
func (p *Process) transfer(name, disposition uint32) uint32 {
	switch q := p.ports[name]; {
	case disposition == moveSendOnce && p.once[name] != nil:
		return p.sendOnce(p.destination(name))
	case q == nil || q.set:
	case disposition == makeSendOnce:
		return p.sendOnce(q)
	case disposition == makeSend:
		q.sends++
		q.made++
	case disposition == copySend:
		q.sends++
	}
	return name
}

// destroy ends a receive right: what was queued is gone, whoever waits on it is told, and so is
// whoever asked to know that its send rights now name nothing.
func (p *Process) destroy(name uint32) {
	q := p.ports[name]
	if q == nil {
		return
	}
	delete(p.ports, name)
	for once, to := range p.once {
		if to == q {
			delete(p.once, once)
		}
	}
	if q.deadName != nil && q.sends > 0 {
		p.notify(q.deadName, notifyDeadName, append(slices.Clone(ndr), le32(name)...)...)
	}
	for _, member := range p.ports {
		if member.in == name {
			member.in = 0
		}
	}
	for _, t := range p.waiters(waitPort, uint64(name)) {
		t.answer = &result{x0: rcvPortDied}
		t.wake()
	}
}

// ---- traps ----

func machPortAllocate(p *Process, a []uint64) int64 {
	var name uint32
	switch a[1] {
	case rightReceive:
		name = p.newReceive().name
	case rightPortSet:
		q := p.newReceive()
		q.set, name = true, q.name
	default: // a dead name: only a number
		name = p.newPort()
	}
	p.write(a[2], le32(name))
	return 0
}

// machPortConstruct makes a port, with a send right too if asked, and keeps the context it is given.
func machPortConstruct(p *Process, a []uint64) int64 {
	q := p.newReceive()
	q.context = a[2]
	if p.u32(a[1])&insertSendRight != 0 {
		q.sends, q.made = 1, 1
	}
	p.write(a[3], le32(q.name))
	return 0
}

// machPortGuard guards a port with a value, which is also its context.
func machPortGuard(p *Process, a []uint64) int64 {
	p.receiver(uint32(a[1])).context = a[2]
	return 0
}

func migPortGetContext(p *Process, body []byte) migReply {
	var context uint64
	if q := p.ports[le.Uint32(body[32:])]; q != nil {
		context = q.context
	}
	return migData(le64(context))
}

// machPortDestruct ends a receive right, and with it some of the send rights that share its name.
func machPortDestruct(p *Process, a []uint64) int64 {
	if q := p.ports[uint32(a[1])]; q != nil {
		q.sends = max(q.sends+int(int32(a[2])), 0)
	}
	p.destroy(uint32(a[1]))
	return 0
}

// machPortInsertRight is how the holder of a receive right makes itself a send right.
func machPortInsertRight(p *Process, a []uint64) int64 {
	p.transfer(uint32(a[2]), uint32(a[3]))
	return 0
}

// machPortModRefs adds or takes away references to one kind of right under a name.
func machPortModRefs(p *Process, a []uint64) int64 {
	name, delta := uint32(a[1]), int32(a[3])
	q := p.ports[name]
	switch {
	case a[2] == rightSendOnce && delta < 0:
		p.dropOnce(name)
	case q == nil:
	case a[2] == rightReceive && delta < 0:
		p.destroy(name)
	case a[2] == rightSend && delta > 0:
		q.sends += int(delta)
	case a[2] == rightSend:
		for ; delta < 0; delta++ {
			p.release(q)
		}
	}
	return 0
}

// dropOnce ends a right to send once that was never used, which its port is told.
func (p *Process) dropOnce(name uint32) bool {
	q := p.once[name]
	if q == nil {
		return false
	}
	delete(p.once, name)
	p.notify(q, notifySendOnce)
	return true
}

// machPortDeallocate gives up one send right, or a right to send once.
func machPortDeallocate(p *Process, a []uint64) int64 {
	if name := uint32(a[1]); !p.dropOnce(name) {
		if q := p.ports[name]; q != nil {
			p.release(q)
		}
	}
	return 0
}

// machPortRequestNotification asks for a message to notify when name loses its last send right or
// its receive right. Other notifications are accepted and never sent.
func machPortRequestNotification(p *Process, a []uint64) int64 {
	q, to := p.ports[uint32(a[1])], p.ports[uint32(a[4])]
	if a[6] != 0 {
		p.write(a[6], le32(0)) // no earlier request to hand back
	}
	if q == nil {
		return 0
	}
	switch a[2] {
	case notifyNoSenders:
		q.noSenders = to
		if to != nil && q.sends == 0 && uint64(q.made) >= a[3] {
			q.sends = 1
			p.release(q) // there are none already
		}
	case notifyDeadName:
		q.deadName = to
	}
	return 0
}

func machReplyPort(p *Process, a []uint64) int64 { return int64(p.newReceive().name) }

// machSpecialReplyPort is the one port a thread waits on for the answer to a synchronous message.
func machSpecialReplyPort(p *Process, a []uint64) int64 {
	if t := p.current; t.reply == 0 {
		t.reply = p.newReceive().name
	}
	return int64(p.current.reply)
}

// setMember puts a port in a set, or with none takes it out of the one it is in.
func (p *Process) setMember(name, set uint32) int64 {
	q := p.ports[name]
	if q == nil || q.set || set != 0 && (p.ports[set] == nil || !p.ports[set].set) {
		return kernInvalidArg
	}
	q.in = set
	return 0
}

func machPortInsertMember(p *Process, a []uint64) int64 {
	return p.setMember(uint32(a[1]), uint32(a[2]))
}
func machPortExtractMember(p *Process, a []uint64) int64 { return p.setMember(uint32(a[1]), 0) }

// ---- timers that send a message when they are due (mk_timer), which is how a run loop sleeps ----

func machTimerCreate(p *Process, a []uint64) int64 { return int64(p.newReceive().name) }

func machTimerDestroy(p *Process, a []uint64) int64 {
	p.destroy(uint32(a[0]))
	return 0
}

func (p *Process) armTimer(name uint32, ticks uint64) int64 {
	q := p.ports[name]
	if q == nil {
		return kernInvalidArg
	}
	q.armed, q.due = true, tickTime(ticks)
	return 0
}

func machTimerArm(p *Process, a []uint64) int64       { return p.armTimer(uint32(a[0]), a[1]) }
func machTimerArmLeeway(p *Process, a []uint64) int64 { return p.armTimer(uint32(a[0]), a[2]) }

func machTimerCancel(p *Process, a []uint64) int64 {
	q := p.ports[uint32(a[0])]
	if q == nil {
		return kernInvalidArg
	}
	q.armed = false
	if a[1] != 0 {
		p.write(a[1], le64(0))
	}
	return 0
}

// fireTimers sends the message of every timer that is due, and reports when the next one is.
func (p *Process) fireTimers(now time.Time) (next time.Time) {
	var due []*port
	for _, q := range p.ports {
		switch {
		case !q.armed:
		case !q.due.After(now):
			due = append(due, q)
		case next.IsZero() || q.due.Before(next):
			next = q.due
		}
	}
	slices.SortFunc(due, func(a, b *port) int { return a.due.Compare(b.due) })
	for _, q := range due {
		q.armed = false
		p.post(q, machMessage{data: append(header(moveSend<<8, 48, q.name, 0), make([]byte, 24)...)})
	}
	return next
}

// ---- sending ----

// arriving is how the right a sender names in a message is named to its receiver.
func arriving(disposition uint32) uint32 {
	switch disposition {
	case copySend, makeSend:
		return moveSend
	case makeSendOnce:
		return moveSendOnce
	}
	return disposition
}

// carried is a message as its receiver gets it: the header turned around, the rights named as
// they arrive, and the memory sent out of line in a place of its own. False for one that is not
// what its header says.
func (p *Process) carried(m message, raw []byte) ([]byte, bool) {
	out := slices.Clone(raw)
	bits := m.bits&bitsComplex | arriving(m.bits>>8&0x1F) | arriving(m.bits&0x1F)<<8
	if m.local == 0 {
		bits &^= 0x1F
	}
	copy(out, header(bits, uint32(len(out)), m.remote, m.id))
	le.PutUint32(out[8:], p.transfer(m.local, m.bits>>8&0x1F))
	if m.bits&bitsComplex == 0 {
		return out, true
	}
	if len(out) < 28 {
		return nil, false
	}
	at := 28
	for range le.Uint32(out[24:]) {
		if len(out) < at+12 {
			return nil, false
		}
		size := 16
		switch kind := out[at+11]; kind {
		case 0: // a port
			size = 12
			le.PutUint32(out[at:], p.transfer(le.Uint32(out[at:]), uint32(out[at+10])))
			out[at+10] = byte(arriving(uint32(out[at+10])))
		case 1, 2: // memory, or an array of ports
			if len(out) < at+16 {
				return nil, false
			}
			addr, n := le.Uint64(out[at:]), uint64(le.Uint32(out[at+12:]))
			var data []byte
			if kind == 2 {
				n *= 4
			}
			if n > 0 {
				data = slices.Clone(p.read(addr, n))
			}
			if kind == 2 {
				for i := 0; i < len(data); i += 4 {
					le.PutUint32(data[i:], p.transfer(le.Uint32(data[i:]), uint32(out[at+10])))
				}
				out[at+10] = byte(arriving(uint32(out[at+10])))
			}
			var copied uint64
			if n > 0 {
				copied, _ = p.alloc(n, allocation{kind: fileRegion, data: data, name: "out-of-line"})
				if out[at+8] != 0 { // the sender gives its own away
					p.unmap(addr&^(pageSize-1), align(addr+n, pageSize))
				}
			}
			le.PutUint64(out[at:], copied)
			out[at+8] = 0
		case 3: // a guarded port
			if len(out) < at+16 {
				return nil, false
			}
			le.PutUint32(out[at+12:], p.transfer(le.Uint32(out[at+12:]), uint32(out[at+10])))
			out[at+10] = byte(arriving(uint32(out[at+10])))
		default:
			return nil, false
		}
		at += size
	}
	return out, true
}

// post queues a message on a port, and hands it to a thread that is waiting for one there.
func (p *Process) post(q *port, m machMessage) {
	q.queue = append(q.queue, m)
	for _, name := range []uint32{q.name, q.in} {
		if w := p.waiters(waitPort, uint64(name)); name != 0 && len(w) > 0 {
			t := w[0]
			// The room it gave may not be memory it has: that is its receive failing, not this send.
			into := handler{fn: func(p *Process, _ []uint64) int64 { return p.hand(q, *t.receiving) }}
			t.answer = &result{x0: uint64(p.call(into, nil, rcvInvalidData))}
			t.wake()
			return
		}
	}
}

// ---- receiving ----

// trailerSize is how much the kernel adds after a message, by what the receiver asked to be told.
func trailerSize(option uint64) int {
	switch option >> 24 & 0xF {
	case 0:
		return 8
	case 1: // the sequence number
		return 12
	case 2: // the sender's user and group
		return 20
	case 3: // its audit token
		return 52
	case 4: // the port's context
		return 60
	case 5:
		return 64
	}
	return 68
}

// hand gives the first message of q to a receive, if it has the room.
func (p *Process) hand(q *port, m message) int64 {
	next := q.queue[0]
	trailer := trailerSize(m.option)
	if uint64(len(next.data)+trailer) > m.rcvSize {
		if m.option&machRcvLarge == 0 {
			q.queue = q.queue[1:] // lost, as the kernel loses it
			return rcvTooLarge
		}
		// Told how large it is, and from which port of a set, to come back for it.
		if m.rcvSize >= 24 {
			p.write(m.rcvMsg+4, le32(uint32(len(next.data))))
			if m.option&machRcvLargeIdentity != 0 {
				p.write(m.rcvMsg+12, le32(q.name))
			}
		}
		return rcvTooLarge
	}
	q.queue = q.queue[1:]
	if next.moved {
		defer p.release(q)
	}
	var uid, gid uint32 // the kernel and launchd are root
	if next.sender == guestPID {
		uid, gid = guestUID, guestGID
	}
	// The format (0) and size; a sequence number; the sender's user and group; its audit token;
	// the port's context; and nothing of the access checks the last words are for.
	tail := append(le32(0, uint32(trailer), 0, uid, gid, 0, uid, gid, uid, gid, next.sender, 100000, guestExecution), le64(q.context, 0)...)
	p.write(m.rcvMsg, append(slices.Clone(next.data), tail[:trailer]...))
	switch {
	case uint64(len(next.aux)) <= m.auxRcvSize && len(next.aux) > 0:
		p.write(m.auxRcv, next.aux)
	case m.auxRcvSize >= 8:
		p.write(m.auxRcv, make([]byte, 8)) // its header, saying there is none
	}
	p.note("  mach_msg id %d received from port %#x: %d bytes", le.Uint32(next.data[20:]), q.name, len(next.data))
	return 0
}

// receive takes the next message of a port or set, waiting for one if the caller will.
func (p *Process) receive(m message) int64 {
	q := p.receiver(m.rcvName)
	if from := p.readable(q); from != nil {
		return p.hand(from, m)
	}
	var deadline time.Time
	if m.option&machRcvTimeout != 0 {
		if m.timeout == 0 {
			return rcvTimedOut
		}
		deadline = time.Now().Add(time.Duration(m.timeout) * time.Millisecond)
	}
	p.note("  mach_msg waits for a message on %#x", m.rcvName)
	p.current.receiving = &m
	p.wait(waitPort, uint64(m.rcvName), deadline, result{x0: rcvTimedOut})
	return 0
}

// ---- telling libdispatch that a port has a message (EVFILT_MACHPORT) ----

// watch is a kqueue's interest in a port or set.
type watch struct {
	event   kevent
	enabled bool
}

type watchKey struct{ ident, udata uint64 }

func (p *Process) changeMachport(q *kqueue, c kevent) {
	key := watchKey{c.ident, c.udata}
	w := q.ports[key]
	switch {
	case c.flags&evDelete != 0:
		delete(q.ports, key)
	case c.flags&evAdd != 0:
		q.ports[key] = &watch{event: c, enabled: c.flags&evDisable == 0}
	case w != nil && c.flags&evEnable != 0:
		w.enabled = true
	case w != nil && c.flags&evDisable != 0:
		w.enabled = false
	}
}

// watched makes an event of every port a kqueue watches that has a message nobody waits for
// directly. One that asked for the message itself is told it is too large for the room it gave,
// which is none, and comes for it with a receive: libdispatch's own path for a large message.
func (p *Process) watched(q *kqueue) {
	keys := make([]watchKey, 0, len(q.ports))
	for key := range q.ports {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b watchKey) int { return int(a.ident) - int(b.ident) })
	for _, key := range keys {
		w := q.ports[key]
		target := p.ports[uint32(key.ident)]
		if !w.enabled || target == nil {
			continue
		}
		from := p.readable(target)
		if from == nil {
			continue
		}
		if slices.ContainsFunc(q.ready, func(e kevent) bool { return e.filter == filterMachport && e.ident == key.ident && e.udata == key.udata }) {
			continue // told already, and not yet heard
		}
		event := w.event
		event.flags &^= evAdd | evEnable | evVanished // what was asked for, not what happened
		event.data = int64(from.name)
		if event.fflags&machRcvMsg != 0 {
			if p.eventData == 0 {
				p.eventData, _ = p.alloc(pageSize, allocation{name: "kevent data"})
			}
			event.fflags, event.ext = rcvTooLarge, [4]uint64{p.eventData, uint64(len(from.queue[0].data))}
		} else {
			event.fflags = 0
		}
		q.ready = append(q.ready, event)
		switch {
		case w.event.flags&evOneshot != 0:
			delete(q.ports, key)
		case w.event.flags&evDispatch != 0:
			w.enabled = false
		}
	}
}
