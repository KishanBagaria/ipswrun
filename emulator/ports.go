package emulator

import (
	"cmp"
	"maps"
	"slices"
	"time"
)

// Ports are real. A port is the kernel's: a queue of messages, with one process holding the right
// to receive from it. A process names the rights it holds, and a name means nothing in another
// process: a right sent in a message leaves its sender's names when the message is sent, and is
// given a name by whoever receives the message, which may be another process. As in Mach, a
// process has one name for a port's receive right and its send rights together, with a count of
// the send rights, and a name of its own for each right to send once. Everything else a message
// can be sent to is the kernel's (the task, the host, a clock) and is answered at once, as a MIG
// routine.

// port is a port, or a port set.
type port struct {
	m       *Machine
	owner   *Process // who holds its receive right: nobody while a message carries it, or launchd holds it
	name    uint32   // what the owner calls it
	set     bool
	in      *port         // the set it is a member of, if any
	queue   []machMessage // as they were sent
	due     time.Time     // of a timer (mk_timer) that is armed: when; zero otherwise
	context uint64        // what it was made or guarded with, which its owner reads back
	dead    bool          // its receive right is gone
	memory  []byte        // of a port that names memory (see migMakeMemoryEntry): a copy of it

	sends     int    // send rights there are, in every process and in messages not yet received
	made      uint32 // send rights ever made from the receive right
	noSenders *port  // told when the last send right goes, if anyone asked
}

// right is what a name stands for in one process.
type right struct {
	port     *port
	receive  bool  // the receive right, or the set itself
	sends    int   // the send rights under this name
	once     bool  // a right to send once
	deadName *port // told when the port loses its receive right, if the process asked
}

// carried is a right in a message on its way: the port, how the receiver gets it, and where in
// the message (or in the array of names sent beside it, memory[in]) its name goes.
type carried struct {
	port        *port
	disposition uint32
	at, in      int
	// name is what the sender called it, for a name that is no right the kernel keeps (a task, a
	// thread, a semaphore): it stays what it was inside one process, and is nothing in another.
	name uint32
}

// outOfLine is memory a message carries beside itself: what it holds, and where in the message
// its address goes once the receiver has it.
type outOfLine struct {
	at   int
	data []byte
}

// machMessage is a queued message, and who sent it: a guest, the kernel (pid 0) or launchd (1).
// A guest's message has its rights and memory still to be given to whoever receives it. What
// the kernel and launchd send is written for the process that holds the port already.
type machMessage struct {
	data   []byte
	reply  carried // the right to answer it with, if it came with one
	rights []carried
	memory []outOfLine
	aux    []byte // what was sent beside it, if anything
	sender uint32
	from   *Process
	moved  bool // the send right it was sent with was given up for it, and ends when it is received
}

const (
	machSendMsg          = 0x1
	machRcvMsg           = 0x2
	machRcvLarge         = 0x4
	machRcvLargeIdentity = 0x8
	machRcvTimeout       = 0x100
	msgVector            = 1 << 32 // MACH64_MSG_VECTOR: the message is given as a vector of two

	rcvInvalidName = 0x10004002
	rcvTooLarge    = 0x10004004
	rcvInvalidData = 0x10004008
	rcvPortDied    = 0x10004009

	bitsComplex = 0x80000000
	portDead    = 0xFFFFFFFF // MACH_PORT_DEAD: the name of a right to a port that is gone

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

// newReceive makes a port the process can receive from.
func (p *Process) newReceive() *port {
	q := &port{m: p.m, owner: p, name: p.newPort()}
	p.names[q.name] = &right{port: q, receive: true}
	p.named[q] = q.name
	return q
}

// owned is the port or set the process holds the receive right of under a name.
func (p *Process) owned(name uint32) *port {
	if r := p.names[name]; r != nil && r.receive {
		return r.port
	}
	return nil
}

// ownedPorts are the ports and sets the process can receive from, lowest name first.
func (p *Process) ownedPorts() []*port {
	var out []*port
	for _, r := range p.names {
		if r.receive {
			out = append(out, r.port)
		}
	}
	slices.SortFunc(out, func(a, b *port) int { return cmp.Compare(a.name, b.name) })
	return out
}

// forget drops a name that no right is left under.
func (p *Process) forget(name uint32) {
	if r := p.names[name]; r != nil && !r.receive && r.sends == 0 && !r.once {
		delete(p.names, name)
		if p.named[r.port] == name {
			delete(p.named, r.port)
		}
	}
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
	for _, member := range p.ownedPorts() {
		if member.in == q && len(member.queue) > 0 {
			return member
		}
	}
	return nil
}

// destination is the port a message to name goes to, if the process has a right to send there.
// A right to send once is used up.
func (p *Process) destination(name uint32) *port {
	r := p.names[name]
	switch {
	case r == nil:
		return nil
	case r.once:
		delete(p.names, name)
	}
	if r.port.set || r.port.dead {
		return nil
	}
	return r.port
}

// takeIn accounts for a right a process names in a message it sends, and returns it as it
// travels: what was moved is no longer the sender's.
func (p *Process) takeIn(name, disposition uint32) carried {
	c := carried{disposition: arriving(disposition), name: name, in: -1}
	r := p.names[name]
	if r == nil {
		return c
	}
	q := r.port
	switch {
	case disposition == moveSendOnce && r.once:
		delete(p.names, name)
	case disposition == makeSendOnce && r.receive:
	case disposition == makeSend && r.receive:
		q.sends++
		q.made++
	case disposition == copySend && (r.sends > 0 || r.receive):
		q.sends++
	case disposition == moveSend && r.sends > 0:
		r.sends--
		p.forget(name)
	case disposition == moveReceive && r.receive && !q.set:
		r.receive = false
		p.forget(name)
		q.owner, q.in = nil, nil
		for _, t := range p.waiters(waitPort, uint64(name)) {
			t.answer = &result{x0: rcvPortDied}
			t.wake()
		}
	default:
		return c // not a right the process has to give
	}
	c.port = q
	return c
}

// giveOut gives the process a right that arrived for it, and returns the name it has there.
func (p *Process) giveOut(c carried, from *Process) uint32 {
	q := c.port
	switch {
	case q == nil && from == p:
		return c.name
	case q == nil:
		if c.name != 0 {
			p.note("  a right that is no port (%#x in the sender) arrives as nothing", c.name)
		}
		return 0
	case q.dead && c.disposition != moveReceive:
		return portDead
	case c.disposition == moveSendOnce:
		name := p.newPort()
		p.names[name] = &right{port: q, once: true}
		return name
	}
	name, ok := p.named[q]
	if !ok {
		name = p.newPort()
		p.names[name], p.named[q] = &right{port: q}, name
	}
	if r := p.names[name]; c.disposition == moveReceive {
		r.receive, q.owner, q.name = true, p, name
	} else {
		r.sends++
	}
	return name
}

// sendOnce names, for the process, a new right to send to q once.
func (p *Process) sendOnce(q *port) uint32 {
	return p.giveOut(carried{port: q, disposition: moveSendOnce}, nil)
}

// enqueue queues on q a message that is not a guest's own: the kernel's (sender 0) or launchd's.
func enqueue(q *port, bits, id uint32, body []byte, sender uint32) {
	post(q, machMessage{data: append(header(bits, uint32(24+len(body)), 0, id), body...), sender: sender})
}

// notify sends q one of the kernel's notifications, if it is still there to be told.
func notify(q *port, id uint32, body []byte) {
	if q != nil && !q.dead {
		enqueue(q, moveSendOnce<<8, id, body, 0)
	}
}

// release ends one send right to q, and tells who asked to know when the last one has gone.
func release(q *port) {
	if q.sends > 0 {
		q.sends--
	}
	if q.sends == 0 && q.noSenders != nil {
		to := q.noSenders
		q.noSenders = nil
		notify(to, notifyNoSenders, migCode(q.made).body)
	}
}

// releaseSend gives up one of the send rights the process has under a name.
func (p *Process) releaseSend(name uint32) {
	if r := p.names[name]; r != nil && r.sends > 0 {
		r.sends--
		p.forget(name)
		release(r.port)
	}
}

// discard ends the rights a message carries that nobody will receive.
func discard(m machMessage) {
	for _, c := range append([]carried{m.reply}, m.rights...) {
		switch {
		case c.port == nil:
		case c.disposition == moveSend:
			release(c.port)
		case c.disposition == moveSendOnce:
			notify(c.port, notifySendOnce, nil)
		case c.disposition == moveReceive:
			destroy(c.port)
		}
	}
}

// destroy ends a port's receive right: what was queued is gone, whoever waits on it is told, and
// so is every process that asked to know that its send rights now name nothing.
func destroy(q *port) {
	if q == nil || q.dead {
		return
	}
	q.dead = true
	queue := q.queue
	q.queue = nil
	if owner := q.owner; owner != nil {
		for _, member := range owner.ownedPorts() {
			if member.in == q {
				member.in = nil
			}
		}
		for _, t := range owner.waiters(waitPort, uint64(q.name)) {
			t.answer = &result{x0: rcvPortDied}
			t.wake()
		}
		if r := owner.names[q.name]; r != nil && r.port == q {
			r.receive = false
			owner.forget(q.name)
		}
	}
	for _, other := range q.m.procs {
		for _, name := range slices.Sorted(maps.Keys(other.names)) {
			switch r := other.names[name]; {
			case r.port != q:
			case r.once:
				delete(other.names, name)
			case r.deadName != nil:
				to := r.deadName
				r.deadName = nil
				notify(to, notifyDeadName, migCode(name).body)
			}
		}
	}
	q.owner, q.in = nil, nil
	for _, m := range queue {
		discard(m)
	}
}

// reap ends every right of a process that has exited: its ports die, and the ports it could
// send to lose those senders.
func (p *Process) reap() {
	for _, name := range slices.Sorted(maps.Keys(p.names)) {
		r := p.names[name]
		if r == nil {
			continue
		}
		q := r.port
		if r.once {
			notify(q, notifySendOnce, nil)
		}
		for ; r.sends > 0; r.sends-- {
			release(q)
		}
		if r.receive {
			destroy(q)
		}
	}
	p.names, p.named = map[uint32]*right{}, map[*port]uint32{}
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
		p.giveOut(p.takeIn(q.name, makeSend), p)
	}
	p.write(a[3], le32(q.name))
	return 0
}

// machPortGuard guards a port with a value, which is also its context.
func machPortGuard(p *Process, a []uint64) int64 {
	if q := p.owned(uint32(a[1])); q != nil {
		q.context = a[2]
	}
	return 0
}

func migPortGetContext(p *Process, body []byte) migReply {
	var context uint64
	if q := p.owned(le.Uint32(body[32:])); q != nil {
		context = q.context
	}
	return migData(le64(context))
}

// machPortDestruct ends a receive right, and with it some of the send rights that share its name.
func machPortDestruct(p *Process, a []uint64) int64 {
	name := uint32(a[1])
	q := p.owned(name)
	if q == nil {
		return 0
	}
	for delta := int32(a[2]); delta < 0; delta++ {
		p.releaseSend(name)
	}
	destroy(q)
	return 0
}

// machPortInsertRight is how the holder of a receive right makes itself a send right.
func machPortInsertRight(p *Process, a []uint64) int64 {
	p.giveOut(p.takeIn(uint32(a[2]), uint32(a[3])), p)
	return 0
}

// machPortModRefs adds or takes away references to one kind of right under a name.
func machPortModRefs(p *Process, a []uint64) int64 {
	name, delta := uint32(a[1]), int32(a[3])
	r := p.names[name]
	switch {
	case a[2] == rightSendOnce && delta < 0:
		p.dropOnce(name)
	case r == nil:
	case a[2] == rightReceive && delta < 0 && r.receive:
		destroy(r.port)
	case a[2] == rightSend && delta > 0:
		r.sends += int(delta)
		r.port.sends += int(delta)
	case a[2] == rightSend:
		for ; delta < 0; delta++ {
			p.releaseSend(name)
		}
	}
	return 0
}

// dropOnce ends a right to send once that was never used, which its port is told.
func (p *Process) dropOnce(name uint32) bool {
	r := p.names[name]
	if r == nil || !r.once {
		return false
	}
	delete(p.names, name)
	notify(r.port, notifySendOnce, nil)
	return true
}

// machPortDeallocate gives up one send right, or a right to send once.
func machPortDeallocate(p *Process, a []uint64) int64 {
	if name := uint32(a[1]); !p.dropOnce(name) {
		p.releaseSend(name)
	}
	return 0
}

// machPortRequestNotification asks for a message to notify when name loses its last send right or
// its receive right. Other notifications are accepted and never sent.
func machPortRequestNotification(p *Process, a []uint64) int64 {
	r, to := p.names[uint32(a[1])], p.owned(uint32(a[4]))
	if a[6] != 0 {
		p.write(a[6], le32(0)) // no earlier request to hand back
	}
	if r == nil {
		return 0
	}
	switch q := r.port; {
	case a[2] == notifyNoSenders && r.receive:
		q.noSenders = to
		if to != nil && q.sends == 0 && uint64(q.made) >= a[3] {
			release(q) // there are none already
		}
	case a[2] == notifyDeadName && q.dead:
		notify(to, notifyDeadName, migCode(uint32(a[1])).body)
	case a[2] == notifyDeadName:
		r.deadName = to
	}
	return 0
}

// machNewReceive is a new port with nothing asked of it: a reply port, or a timer's.
func machNewReceive(p *Process, a []uint64) int64 { return int64(p.newReceive().name) }

// machSpecialReplyPort is the one port a thread waits on for the answer to a synchronous message.
func machSpecialReplyPort(p *Process, a []uint64) int64 {
	if t := p.current; t.reply == 0 {
		t.reply = p.newReceive().name
	}
	return int64(p.current.reply)
}

// setMember puts a port in a set, or with none takes it out of the one it is in.
func (p *Process) setMember(name, setName uint32) int64 {
	q, set := p.owned(name), p.owned(setName)
	if q == nil || q.set || setName != 0 && (set == nil || !set.set) {
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

func machTimerDestroy(p *Process, a []uint64) int64 {
	destroy(p.owned(uint32(a[0])))
	return 0
}

func (p *Process) armTimer(name uint32, ticks uint64) int64 {
	q := p.owned(name)
	if q == nil {
		return kernInvalidArg
	}
	q.due = tickTime(ticks)
	return 0
}

func machTimerArm(p *Process, a []uint64) int64       { return p.armTimer(uint32(a[0]), a[1]) }
func machTimerArmLeeway(p *Process, a []uint64) int64 { return p.armTimer(uint32(a[0]), a[2]) }

func machTimerCancel(p *Process, a []uint64) int64 {
	q := p.owned(uint32(a[0]))
	if q == nil {
		return kernInvalidArg
	}
	q.due = time.Time{}
	if a[1] != 0 {
		p.write(a[1], le64(0))
	}
	return 0
}

// fireTimers sends the message of every timer that is due, and reports when the next one is.
func (p *Process) fireTimers(now time.Time) (next time.Time) {
	var due []*port
	for _, q := range p.ownedPorts() {
		switch {
		case q.due.IsZero():
		case !q.due.After(now):
			due = append(due, q)
		case next.IsZero() || q.due.Before(next):
			next = q.due
		}
	}
	slices.SortFunc(due, func(a, b *port) int { return a.due.Compare(b.due) })
	for _, q := range due {
		q.due = time.Time{}
		enqueue(q, moveSend<<8, 0, make([]byte, 24), 0)
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

// portDescriptor is a port right as a message carries it out of line.
func portDescriptor(name uint32, disposition byte) []byte {
	return append(le32(name, 0), 0, 0, disposition, 0)
}

// descriptors calls visit with the place and kind of each descriptor of a complex message: a port
// (0), memory (1), an array of ports (2) or a guarded port (3). It returns where the body after
// them starts; false for a message that ends before they do, or has one of another kind.
func descriptors(raw []byte, visit func(at int, kind byte)) (int, bool) {
	if len(raw) < 28 {
		return 0, false
	}
	at := 28
	for range le.Uint32(raw[24:]) {
		if len(raw) < at+12 {
			return 0, false
		}
		kind, size := raw[at+11], 16
		if kind == 0 {
			size = 12
		}
		if kind > 3 || len(raw) < at+size {
			return 0, false
		}
		visit(at, kind)
		at += size
	}
	return at, true
}

// outgoing turns a message as it was sent into what waits in the queue: the header turned around,
// and the rights and the memory it names taken from the sender, to be given to whoever receives
// it. False for a message that is not what its header says.
func (p *Process) outgoing(m message, out []byte) (machMessage, bool) {
	bits := m.bits&bitsComplex | arriving(m.bits>>8&0x1F) | arriving(m.bits&0x1F)<<8
	if m.local == 0 {
		bits &^= 0x1F
	}
	copy(out, header(bits, uint32(len(out)), 0, m.id))
	sent := machMessage{data: out, sender: p.pid, from: p, moved: m.bits&0x1F == moveSend}
	if m.bits&bitsComplex != 0 {
		if _, ok := descriptors(out, func(int, byte) {}); !ok {
			return sent, false // before anything is taken from the sender
		}
	}
	sent.reply = p.takeIn(m.local, m.bits>>8&0x1F)
	if m.bits&bitsComplex == 0 {
		return sent, true
	}
	// right takes the right whose name is at a place in the message, or in memory sent beside it.
	right := func(name uint32, disposition byte, at, in int) {
		c := p.takeIn(name, uint32(disposition))
		c.at, c.in = at, in
		sent.rights = append(sent.rights, c)
	}
	descriptors(out, func(at int, kind byte) {
		switch kind {
		case 0:
			right(le.Uint32(out[at:]), out[at+10], at, -1)
		case 3:
			right(le.Uint32(out[at+12:]), out[at+10], at+12, -1)
		default:
			addr, n := le.Uint64(out[at:]), uint64(le.Uint32(out[at+12:]))
			if kind == 2 {
				n *= 4 // names
			}
			if n > 0 {
				data := p.read(addr, n)
				for i := 0; kind == 2 && i < len(data); i += 4 {
					right(le.Uint32(data[i:]), out[at+10], i, len(sent.memory))
				}
				sent.memory = append(sent.memory, outOfLine{at, data})
				if out[at+8] != 0 { // the sender gives its own away
					p.unmap(addr&^(pageSize-1), align(addr+n, pageSize))
				}
			}
			le.PutUint64(out[at:], 0)
			out[at+8] = 0
		}
		if kind != 1 {
			out[at+10] = byte(arriving(uint32(out[at+10])))
		}
	})
	return sent, true
}

// post queues a message on a port, and hands it to a thread that is waiting for one there.
func post(q *port, m machMessage) {
	if q.dead {
		discard(m)
		return
	}
	q.queue = append(q.queue, m)
	owner := q.owner
	if owner == nil {
		return // it waits for whoever gets the port
	}
	for _, at := range []*port{q, q.in} {
		if at == nil {
			continue
		}
		if w := owner.waiters(waitPort, uint64(at.name)); len(w) > 0 {
			t := w[0]
			// The room it gave may not be memory it has: that is its receive failing, not this send.
			into := handler{fn: func(p *Process, _ []uint64) int64 { return p.hand(q, *t.receiving) }}
			t.answer = &result{x0: uint64(owner.call(into, nil, rcvInvalidData))}
			t.wake()
			return
		}
	}
}

// ---- receiving ----

// trailerSize is how much the kernel adds after a message, by how much the receiver asked to be
// told: nothing, the sequence number, the sender's user and group, its audit token, the port's
// context, or everything.
func trailerSize(option uint64) int {
	sizes := [...]int{8, 12, 20, 52, 60, 64}
	if asked := option >> 24 & 0xF; asked < uint64(len(sizes)) {
		return sizes[asked]
	}
	return 68
}

// hand gives the first message of q to a receive of the process, if it has the room: the rights
// it carries get their names here, and the memory a place.
func (p *Process) hand(q *port, m message) int64 {
	next := q.queue[0]
	trailer := trailerSize(m.option)
	if uint64(len(next.data)+trailer) > m.rcvSize {
		if m.option&machRcvLarge == 0 {
			q.queue = q.queue[1:] // lost, as the kernel loses it
			discard(next)
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
	p.ensure(m.rcvMsg, uint64(len(next.data)+trailer)) // before anything is given that could not be taken back
	q.queue = q.queue[1:]
	if next.moved {
		defer release(q)
	}
	data := slices.Clone(next.data)
	le.PutUint32(data[12:], q.name)
	if next.from != nil {
		le.PutUint32(data[8:], p.giveOut(next.reply, next.from))
	}
	for _, c := range next.rights {
		if c.in < 0 {
			le.PutUint32(data[c.at:], p.giveOut(c, next.from))
		} else {
			le.PutUint32(next.memory[c.in].data[c.at:], p.giveOut(c, next.from))
		}
	}
	for _, memory := range next.memory {
		addr, _ := p.alloc(uint64(len(memory.data)), allocation{kind: fileRegion, data: memory.data, name: "out-of-line"})
		le.PutUint64(data[memory.at:], addr)
	}
	var uid, gid uint32 // the kernel and launchd are root
	if next.from != nil {
		uid, gid = next.from.uid, next.from.gid
	}
	// The format (0) and size; a sequence number; the sender's user and group; its audit token;
	// the port's context; and nothing of the access checks the last words are for.
	tail := append(le32(0, uint32(trailer), 0, uid, gid, 0, uid, gid, uid, gid, next.sender, 100000, guestExecution), le64(q.context, 0)...)
	p.write(m.rcvMsg, append(data, tail[:trailer]...))
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
	q := p.owned(m.rcvName)
	if q == nil {
		return rcvInvalidName
	}
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

// watchKey names a kqueue's interest in a port or set, which is kept as the event that asked for
// it: disabled while that has EV_DISABLE.
type watchKey struct{ ident, udata uint64 }

func (p *Process) changeMachport(q *kqueue, c kevent) {
	key := watchKey{c.ident, c.udata}
	w, ok := q.ports[key]
	switch {
	case c.flags&evDelete != 0:
		delete(q.ports, key)
	case c.flags&evAdd != 0:
		q.ports[key] = c
	case ok && c.flags&evEnable != 0:
		w.flags &^= evDisable
		q.ports[key] = w
	case ok && c.flags&evDisable != 0:
		w.flags |= evDisable
		q.ports[key] = w
	}
}

// watched makes an event of every port a kqueue watches that has a message nobody waits for
// directly. One that asked for the message itself is told it is too large for the room it gave,
// which is none, and comes for it with a receive: libdispatch's own path for a large message.
func (p *Process) watched(q *kqueue) {
	for _, key := range slices.SortedFunc(maps.Keys(q.ports), func(a, b watchKey) int { return cmp.Compare(a.ident, b.ident) }) {
		w := q.ports[key]
		target := p.owned(uint32(key.ident))
		if w.flags&evDisable != 0 || target == nil {
			continue
		}
		from := p.readable(target)
		if from == nil {
			continue
		}
		if slices.ContainsFunc(q.ready, func(e kevent) bool { return e.filter == filterMachport && e.ident == key.ident && e.udata == key.udata }) {
			continue // told already, and not yet heard
		}
		event := w
		event.flags &^= evAdd | evEnable | evVanished // what was asked for, not what happened
		event.data, event.fflags = int64(from.name), 0
		if w.fflags&machRcvMsg != 0 {
			// Where the message would be is a page that is never written: the event only has to
			// say that there is one.
			if p.eventData == 0 {
				p.eventData, _ = p.alloc(pageSize, allocation{name: "kevent data"})
			}
			event.fflags, event.ext = rcvTooLarge, [4]uint64{p.eventData, uint64(len(from.queue[0].data))}
		}
		q.ready = append(q.ready, event)
		switch {
		case w.flags&evOneshot != 0:
			delete(q.ports, key)
		case w.flags&evDispatch != 0:
			w.flags |= evDisable
			q.ports[key] = w
		}
	}
}
