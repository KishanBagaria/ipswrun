package emulator

import (
	"syscall"
	"time"
)

// A pair of connected sockets (socketpair) is two descriptors with bytes in the kernel between
// them each way: what is sent into one waits to be received from the other. A receiver with
// nothing to receive waits for a sender, and a sender with no room for a receiver, as with a
// pipe, and either can say that it will send no more. What a Mac's kernel does was measured with
// tests/kernel/sockets/socketpair.c, for a pair of stream sockets of the local domain. There is no other
// socket here: no datagrams, no names to connect to, no network.

const (
	socketSize  = 8192 // what waits to be received at most (SO_SNDBUF and SO_RCVBUF)
	socketLeast = 2048 // the room there has to be before part of what is sent is taken (SO_SNDLOWAT)

	domainLocal    = 1 // AF_UNIX
	domainInternet = 2 // AF_INET
	socketStream   = 1 // SOCK_STREAM

	// How a send or a receive is asked for.
	messagePeek     = 0x2     // MSG_PEEK: what is received stays to be received
	messageWaitAll  = 0x40    // MSG_WAITALL: not less than was asked for, while more can come
	messageDontWait = 0x80    // MSG_DONTWAIT
	messageNoSignal = 0x80000 // MSG_NOSIGNAL

	levelSocket = 0xFFFF // SOL_SOCKET

	eNOTSOCK   = 38
	eNOTCONN   = 57
	eOPNOTSUPP = 102
)

// socketPair is what the two sockets of a pair have in common.
type socketPair struct {
	id      uint64 // among the pipes' (see Machine.pipes): what a thread that waits on either waits on
	changes uint64 // how often something has happened to either
}

type socket struct {
	pair *socketPair
	peer *socket
	data []byte // what was sent to it and waits to be received
	// What it can no longer do: send, since it said so or the other has gone, and receive
	// more than is there already, since the other said so or has gone.
	cantSend, cantReceive bool
	second                bool // of the two, the one made second
	// events, of the socket the kernel tells a program of its events on, is which it wants
	// (see sysSocket): such a socket has for its other end one that never sends and never goes.
	events *eventFilter
	// least is the room there has to be before part of what is sent is taken and before the
	// socket is said to be one that can be written (SO_SNDLOWAT), where it was set.
	least int
	// Of a socket made by socket() and not as one of a pair (see localsockets.go): that it is
	// connected to no other yet, the address it was bound to as it was given, and, of one
	// that listens, for how many and the ends made for those who connected and were not yet
	// accepted.
	// rights are the descriptors that came with what waits to be received, each where the
	// message it came with starts (see messages.go).
	rights []*rights
	// datagram, of a datagram socket, is what it has that a stream socket has not (see
	// datagrams.go).
	datagram  *datagrams
	lone      bool
	name      []byte
	bound     bool // the name is its own, and not that of the one that accepted it
	listening bool
	backlog   int
	pending   []*socket
}

// sendLeast is that room: what was set, or what a socket has at first.
func (s *socket) sendLeast() int {
	if s.least != 0 {
		return s.least
	}
	return socketLeast
}

// space is how much can be sent from a socket before the other has received any.
// What came beside the bytes takes room too until it is received, as measured: a descriptor
// sent with all a socket holds leaves 16 bytes unsent.
func (s *socket) space() int {
	room := socketSize - len(s.peer.data)
	for _, r := range s.peer.rights {
		room -= r.size
	}
	return room
}

// eventFilter is which of the kernel's events a program wants: a vendor's, of a class and a
// subclass, each any if 0 (and the vendor any if all ones, as it is at first).
type eventFilter struct {
	vendor, class, subclass uint32
	room                    uint32 // how much it holds of what has not been read (SO_RCVBUF): 32768 at first, as measured
}

const (
	domainSystem   = 32 // PF_SYSTEM
	socketRaw      = 3
	socketDatagram = 2 // SOCK_DGRAM
	protocolEvent  = 1 // SYSPROTO_EVENT

	ePROTOTYPE      = 41
	ePROTONOSUPPORT = 43

	// What is asked of an event socket by ioctl ('e', with a 32-bit number or a filter of 12 bytes).
	ioctlEventID     = 0x40046501 // SIOCGKEVID
	ioctlEventFilter = 0x800c6502 // SIOCSKEVFILT
	ioctlEventWanted = 0x400c6503 // SIOCGKEVFILT
	ioctlEventVendor = 0xc0cc6504 // SIOCGKEVVENDOR
	ioctlNonblocking = 0x8004667e // FIONBIO
)

// sysSocket is socket(domain, type, protocol). There is no network here, and a socket of the
// network or of the local domain's names is refused (EACCES), which a program takes as a machine
// that lets it reach nobody. The kernel's own socket is another matter: on one of the system's
// domain, raw, of the events protocol, the kernel tells a program what happens to the machine (a
// network interface coming up, a volume mounted), and configd, which opens one as it starts, has
// no way on without it. It is given as measured on macOS 27.2.1 (tests/kernel/sockets/kernevents.c): a
// program may say which events it wants and is told back, and nothing is ever there to read, as
// nothing of the kind happens here. A stream of that protocol is EPROTOTYPE and a protocol there
// is not EPROTONOSUPPORT, as measured; any other socket of the system's domain (a control
// socket, by which a program talks to an extension of the kernel) stops the run.
func sysSocket(p *Process, a []uint64) int64 {
	domain, kind, protocol := a[0], a[1], a[2]
	switch {
	case domain == domainLocal && kind == socketStream && protocol == 0:
		return p.localSocket(false)
	case domain == domainLocal && kind == socketDatagram && protocol == 0:
		return p.localSocket(true)
	case domain != domainSystem:
		return -eACCES
	case kind == socketStream && protocol == protocolEvent:
		return -ePROTOTYPE
	case kind == socketRaw && protocol == 77:
		return -ePROTONOSUPPORT
	case kind != socketRaw || protocol != protocolEvent:
		p.unimplemented("a socket of the system's domain other than the one for the kernel's events", a)
		return -eNOSYS
	case p.full():
		return -eMFILE
	}
	p.m.pipes++
	pair := &socketPair{id: p.m.pipes}
	one, never := &socket{pair: pair, events: &eventFilter{vendor: ^uint32(0), room: 32768}}, &socket{pair: pair, second: true}
	one.peer, never.peer = never, one
	const readWrite = 2 // O_RDWR
	return int64(p.newFD(&fd{kind: socketFD, sock: one, flags: readWrite, refs: 1}))
}

// eventIoctl is what is asked of an event socket by ioctl: the filter set and read back, as the
// kernel's source has them and as measured, and ENOTSUP for an event request there is not. Stopping,
// not having been measured: the number of the last event, and a vendor's number by its name.
func (p *Process) eventIoctl(f *fd, a []uint64) int64 {
	s := f.sock
	switch uint32(a[1]) {
	case ioctlNonblocking: // of any socket: whether it waits, as its flags then say
		if p.u32(a[2]) != 0 {
			f.flags |= flagNonblock
		} else {
			f.flags &^= flagNonblock
		}
		return 0
	case ioctlEventFilter:
		b := p.read(a[2], 12)
		s.events.vendor, s.events.class, s.events.subclass = le.Uint32(b), le.Uint32(b[4:]), le.Uint32(b[8:])
		return 0
	case ioctlEventWanted:
		p.write(a[2], le32(s.events.vendor, s.events.class, s.events.subclass))
		return 0
	case ioctlEventID, ioctlEventVendor:
		p.unimplemented("ioctl of the kernel's event socket that was not measured", a)
		return -eNOSYS
	}
	if uint32(a[1])>>8&0xFF != 'e' { // one of a socket's own, which the socket and not its protocol answers
		p.unimplemented("ioctl of the kernel's event socket that was not measured", a)
		return -eNOSYS
	}
	if request := uint32(a[1]); request&0x80000000 != 0 && request>>16&0x1FFF != 0 {
		p.read(a[2], uint64(request>>16&0x1FFF)) // what it gives is read first, by the source: EFAULT of nowhere
	}
	return -eNOTSUP // an event request there is not, as the source has it and as measured of one
}

// sysSocketpair is socketpair(domain, type, protocol, where the two descriptors go).
func sysSocketpair(p *Process, a []uint64) int64 {
	switch {
	case a[0] == domainInternet:
		return -eOPNOTSUPP
	case a[0] != domainLocal || a[1] != socketStream && a[1] != socketDatagram || a[2] != 0:
		p.unimplemented("socketpair of anything but stream or datagram sockets of the local domain", a)
		return -eNOSYS
	case p.allocFD(p.allocFD(0)+1) >= p.fdLimit():
		return -eMFILE
	}
	if a[1] == socketDatagram {
		return p.datagramPair(a[3])
	}
	p.m.pipes++
	pair := &socketPair{id: p.m.pipes}
	one, two := &socket{pair: pair}, &socket{pair: pair, second: true}
	one.peer, two.peer = two, one
	const readWrite = 2 // O_RDWR
	first := p.newFD(&fd{kind: socketFD, sock: one, flags: readWrite, refs: 1})
	second := p.newFD(&fd{kind: socketFD, sock: two, flags: readWrite, refs: 1})
	p.write(a[3], le32(uint32(first), uint32(second)))
	return 0
}

func (p *Process) socketChanged(s *socket) {
	s.pair.changes++
	p.m.streamChanged(s.pair.id)
}

// socketClosed is the last descriptor of a socket closing: what waited in it to be received is
// gone, and the other can neither send nor receive more than it has.
func (p *Process) socketClosed(f *fd) {
	if f.refs--; f.refs > 0 {
		return // a duplicate is still open, or another process's
	}
	s := f.sock
	if s.datagram != nil {
		p.datagramClosed(s)
	}
	s.data = nil
	for _, r := range s.rights { // what was sent to it and never received is nobody's now
		for _, file := range r.files {
			p.dropFD(file)
		}
	}
	s.rights = nil
	s.cantSend, s.cantReceive = true, true
	s.peer.cantSend, s.peer.cantReceive = true, true
	// Those who connected to one that listened and were never accepted have nobody now.
	for _, end := range s.pending {
		end.cantSend, end.cantReceive = true, true
		end.peer.cantSend, end.peer.cantReceive = true, true
		p.socketChanged(end)
	}
	s.pending, s.listening = nil, false
	p.socketChanged(s)
}

// socketReceive reads from a socket: what is there, up to what was asked for. With nothing there it is
// the end if no more can come, and otherwise a wait, or for one that does not wait an error.
func (p *Process) socketReceive(f *fd, into []stretch, flags uint64) int64 {
	return p.socketReceiveInto(f, &pipeOp{f: f, into: into, flags: flags})
}

// socketReceiveInto is socketReceive for an operation made already, which a recvmsg's is.
func (p *Process) socketReceiveInto(f *fd, op *pipeOp) int64 {
	if f.sock.datagram != nil {
		return p.datagramReceive(f, op)
	}
	flags := op.flags
	waits := f.flags&flagNonblock == 0 && flags&messageDontWait == 0
	switch {
	case f.sock.lone: // connected to nobody, as measured
		return -eNOTCONN
	case flags&^(messagePeek|messageWaitAll|messageDontWait) != 0,
		flags&messageWaitAll != 0 && (flags&messagePeek != 0 || !waits): // the second waits, on a Mac, and for how long was not measured
		p.unimplemented("a receive from a socket asked for in a way that was not measured", []uint64{flags})
		return -eNOSYS
	}
	return p.receiveOp(op, waits)
}

// receiveOp is socketReceive for an operation that may also say where what comes beside the
// bytes is wanted (recvmsg).
func (p *Process) receiveOp(op *pipeOp, waits bool) int64 {
	f := op.f
	for _, s := range op.into {
		op.room += s.size
	}
	if op.room == 0 {
		// Into no room nothing is received, but descriptors at the head of what waits are
		// had all the same, as measured.
		if s := f.sock; len(s.rights) > 0 && s.rights[0].at == 0 {
			with := s.rights[0]
			s.rights = s.rights[1:]
			p.takeRights(op, with)
			p.socketChanged(s)
			return p.received(op)
		}
		return 0
	}
	if r, ok := p.tryReceive(op); ok {
		return r
	}
	if !waits {
		return -eAGAIN
	}
	p.current.pipeOp = op
	p.wait(waitPipe, f.sock.pair.id, time.Time{}, result{})
	return 0
}

func (p *Process) tryReceive(op *pipeOp) (int64, bool) {
	s := op.f.sock
	if s.datagram != nil {
		return p.tryDatagram(op)
	}
	// What was sent with descriptors beside it starts a message of its own, as measured: a
	// receive takes nothing from before it together with it, and the descriptors come with the
	// first of its bytes. What was sent alone after it is part of it.
	there, with := len(s.data), (*rights)(nil)
	switch {
	case len(s.rights) > 0 && op.flags&messageWaitAll != 0:
		p.unimplemented("a receive that waits for all it asked for (MSG_WAITALL) where descriptors wait with the bytes", nil)
		return -eNOSYS, true
	case len(s.rights) > 0 && s.rights[0].at > 0:
		there = s.rights[0].at
	case len(s.rights) > 0:
		if with = s.rights[0]; len(s.rights) > 1 {
			there = s.rights[1].at
		}
	}
	if n := min(uint64(there), op.room-uint64(op.done)); n > 0 {
		if with != nil && op.flags&messagePeek != 0 {
			p.unimplemented("a look at what a socket has to receive (MSG_PEEK) where descriptors came with it", nil)
			return -eNOSYS, true
		}
		p.put(op, s.data[:n])
		if op.flags&messagePeek != 0 {
			return int64(n), true
		}
		s.data = s.data[n:]
		if with != nil {
			s.rights = s.rights[1:]
			p.takeRights(op, with)
		}
		for _, r := range s.rights {
			r.at -= int(n)
		}
		op.done += int64(n)
		p.socketChanged(s)
		if with != nil { // no more is waited for once descriptors have come
			return p.received(op), true
		}
	}
	switch {
	case uint64(op.done) == op.room, op.done > 0 && op.flags&messageWaitAll == 0, s.cantReceive:
		return p.received(op), true
	}
	return 0, false
}

// socketSend writes to a socket: all of it, waiting for room as often as it takes, or for one that
// does not wait as much as there is room for. From one that can send no more it is an error, and
// a signal. Written says that it came by write, which is remembered of the descriptor.
func (p *Process) socketSend(f *fd, data []byte, flags uint64, written bool) int64 {
	switch {
	case f.sock.events != nil: // the kernel takes nothing on its event socket
		return -eOPNOTSUPP
	case f.sock.lone: // connected to nobody, as measured
		return -eNOTCONN
	case flags&^(messageDontWait|messageNoSignal) != 0:
		p.unimplemented("a send to a socket asked for in a way that was not measured", []uint64{flags})
		return -eNOSYS
	case len(data) == 0:
		p.unimplemented("a send of nothing to a socket", nil)
		return -eNOSYS
	}
	return p.sendOp(&pipeOp{f: f, data: data, sending: true, flags: flags}, written)
}

// sendOp is socketSend for an operation that may have descriptors to go with the bytes (sendmsg).
func (p *Process) sendOp(op *pipeOp, written bool) int64 {
	f, flags := op.f, op.flags
	if f.sock.datagram != nil {
		if op.files != nil {
			for _, file := range op.files {
				p.dropFD(file)
			}
			p.unimplemented("descriptors sent over a datagram socket", nil)
			return -eNOSYS
		}
		return p.datagramSend(f, op.data, flags, 0, 0)
	}
	fail := func(errno int64) int64 {
		for _, file := range op.files {
			p.dropFD(file)
		}
		op.files = nil
		return errno
	}
	switch {
	case f.sock.events != nil:
		return fail(-eOPNOTSUPP)
	case f.sock.lone:
		return fail(-eNOTCONN)
	case flags&^(messageDontWait|messageNoSignal) != 0:
		p.unimplemented("a send to a socket asked for in a way that was not measured", []uint64{flags})
		return fail(-eNOSYS)
	case len(op.data) == 0:
		p.unimplemented("a send of nothing to a socket", nil)
		return fail(-eNOSYS)
	}
	r, ok := p.trySend(op)
	if written && op.done > 0 {
		f.flags |= flagWritten
	}
	if ok {
		return r
	}
	if f.flags&flagNonblock != 0 || flags&messageDontWait != 0 {
		if op.done > 0 {
			return op.done
		}
		return fail(-eAGAIN)
	}
	p.current.pipeOp = op
	p.wait(waitPipe, f.sock.pair.id, time.Time{}, result{})
	return 0
}

// trySend puts in what there is room for, and says whether that is the end of the send. A
// part of what is sent goes in only where there is room for the least a socket takes.
func (p *Process) trySend(op *pipeOp) (int64, bool) {
	s := op.f.sock
	if s.cantSend {
		for _, file := range op.files { // they go nowhere
			p.dropFD(file)
		}
		op.files = nil
		return p.brokenPipe(op.f, op.flags&messageNoSignal != 0), true
	}
	if op.files != nil {
		// What has descriptors beside it starts a message, and goes in when there is room for
		// all of it, or for what is beside and for the least a socket takes (the source's
		// sosend), the rest following as bytes alone do.
		space := s.space()
		if space < len(op.data)+op.besideSize && (space < s.sendLeast() || space < op.besideSize) {
			return 0, false
		}
		s.peer.rights = append(s.peer.rights, &rights{at: len(s.peer.data), files: op.files, size: op.besideSize})
		op.files = nil
	}
	for len(op.data) > 0 {
		space := s.space()
		if space < len(op.data) && space < s.sendLeast() {
			return 0, false
		}
		n := min(space, len(op.data))
		s.peer.data = append(s.peer.data, op.data[:n]...)
		op.data, op.done = op.data[n:], op.done+int64(n)
		p.socketChanged(s)
		if s.cantSend { // whoever that woke has closed the other
			return p.brokenPipe(op.f, op.flags&messageNoSignal != 0), true
		}
	}
	return op.done, true
}

// ready says whether a socket can be received from, or sent to, without waiting, as select has
// it: also when it never will be again, which a receive or a send says at once.
func (s *socket) ready(sending bool) bool {
	if d := s.datagram; d != nil {
		// It can always be sent on, as measured, and received from when a message waits, or
		// (from the source) when it has something gone wrong to say.
		return sending || len(d.messages) > 0 || d.reset
	}
	if s.lone {
		// One that listens can be read when someone has connected and waits to be accepted,
		// as measured; one connected to nobody can be neither read nor written, by the
		// kernel's source (soreadable and sowriteable want a connection of a stream socket).
		return !sending && len(s.pending) > 0
	}
	if sending {
		return s.cantSend || s.space() >= s.sendLeast()
	}
	return len(s.data) > 0 || s.cantReceive
}

// polled is what poll answers of a socket for the events it was asked about. A Mac's kernel
// asks a kqueue's two filters, the one for reading first: a filter that says the other end has
// gone makes it a hangup, after which everything asked about reading is answered and nothing
// about writing.
func (s *socket) polled(events uint16) uint16 {
	var answer uint16
	if s.datagram != nil {
		if s.ready(false) {
			answer |= events & (pollIn | pollRdNorm)
		}
		return answer | events&pollWriting
	}
	if events&pollReading != 0 && s.ready(false) {
		if answer |= events & (pollIn | pollRdNorm); s.cantReceive {
			answer |= pollHangup | events&pollReading
		}
	}
	if events&pollWriting != 0 && s.ready(true) {
		if s.cantSend {
			answer |= pollHangup
		}
		if answer&pollHangup == 0 {
			answer |= events & pollWriting
		}
	}
	return answer
}

// socketStat is the struct stat64 of a socket: it can be read while more can come or something
// is there, and written while it can send.
func (p *Process) socketStat(f *fd) []byte {
	s := f.sock
	mode := uint16(syscall.S_IFSOCK)
	if !s.cantReceive || len(s.data) > 0 {
		mode |= 0o444
	}
	if !s.cantSend {
		mode |= 0o222
	}
	b := make([]byte, 144)
	le.PutUint32(b, 0xFFFFFFFF) // of no device
	le.PutUint16(b[4:], mode)
	// Its number is one the kernel makes up for each socket.
	le.PutUint64(b[8:], 0x9E3779B97F4A7C15*(2*s.pair.id+uint64(truth(s.second))))
	le.PutUint32(b[16:], p.uid)
	le.PutUint32(b[20:], p.gid)
	le.PutUint64(b[96:], uint64(len(s.data)))
	le.PutUint32(b[112:], socketSize)
	return b
}

// socketOf is the socket a descriptor is of, or the error a call for sockets answers of one
// that is none.
func (p *Process) socketOf(n uint64) (*fd, int64) {
	switch f := p.fds[n]; {
	case f == nil:
		return nil, -eBADF
	case f.kind != socketFD:
		return nil, -eNOTSOCK
	default:
		return f, 0
	}
}

// sysSendto is sendto(socket, data, size, how, to whom, the size of that): to the other of the
// pair, as these are connected.
func sysSendto(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	if f.sock.datagram != nil {
		if a[2] > datagramMost { // too long to be sent: what it holds is not read
			return p.datagramSend(f, make([]byte, datagramMost+1), a[3], a[4], a[5])
		}
		return p.datagramSend(f, p.read(a[1], a[2]), a[3], a[4], a[5])
	}
	if a[4] != 0 {
		p.unimplemented("sendto with an address, on a stream socket", a)
		return -eNOSYS
	}
	return p.socketSend(f, p.read(a[1], a[2]), a[3], false)
}

// sysRecvfrom is recvfrom(socket, where, room, how, where to say from whom, and how long that is).
func sysRecvfrom(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	if f.sock.datagram != nil {
		return p.socketReceiveInto(f, &pipeOp{f: f, into: []stretch{{a[1], a[2]}}, flags: a[3], address: a[4], length: a[5]})
	}
	if a[4] != 0 {
		p.unimplemented("recvfrom asking whom it is from, on a stream socket", a)
		return -eNOSYS
	}
	return p.socketReceive(f, []stretch{{a[1], a[2]}}, a[3])
}

// sysShutdown is shutdown(socket, which way: receiving, sending or both). To send no more is
// what was measured: the other then receives what was sent and the end. A socket that already
// cannot is not connected, to a Mac's kernel.
func sysShutdown(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	s := f.sock
	const sending = 1 // SHUT_WR
	switch {
	case s.events != nil:
		p.unimplemented("shutdown of the kernel's event socket, which was not measured", a)
		return -eNOSYS
	case s.datagram != nil:
		p.unimplemented("shutdown of a datagram socket", a)
		return -eNOSYS
	case s.lone && a[1] <= 2: // connected to nobody: from the source (soshutdown), not measured
		return -eNOTCONN
	case a[1] > 2:
		return -eINVAL
	case a[1] == sending && s.cantSend, a[1] != sending && s.cantReceive:
		return -eNOTCONN
	case a[1] != sending:
		// What is sent to a socket that receives no more is taken and lost, on a Mac, and how
		// much of it before a sender waits for good was not measured.
		p.unimplemented("shutdown of a socket's receiving", a)
		return -eNOSYS
	}
	s.cantSend, s.peer.cantReceive = true, true
	p.socketChanged(s)
	return 0
}

// What getsockopt and setsockopt are asked about a socket.
const (
	optionSendSize     = 0x1001 // SO_SNDBUF
	optionReceiveSize  = 0x1002 // SO_RCVBUF
	optionSendLeast    = 0x1003 // SO_SNDLOWAT
	optionReceiveLeast = 0x1004 // SO_RCVLOWAT
	optionError        = 0x1007 // SO_ERROR
	optionType         = 0x1008 // SO_TYPE
	optionToRead       = 0x1020 // SO_NREAD
	optionNoSignal     = 0x1022 // SO_NOSIGPIPE
	optionToSend       = 0x1024 // SO_NWRITE
)

// sysGetsockopt is getsockopt(socket, level, option, where its value goes, how much room there
// is and then how much was used).
func sysGetsockopt(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	var value uint32
	switch {
	case a[1] != levelSocket || p.u32(a[4]) != 4:
	case a[2] == optionType && f.sock.events != nil:
		value = socketRaw
	case a[2] == optionReceiveSize && f.sock.events != nil:
		value = f.sock.events.room
	case f.sock.events != nil:
		p.unimplemented("getsockopt of the kernel's event socket other than its type and what it holds", a)
		return -eNOSYS
	case f.sock.datagram != nil && a[2] == optionType:
		value = socketDatagram
	case f.sock.datagram != nil && a[2] == optionSendSize:
		value = datagramMost
	case f.sock.datagram != nil && a[2] == optionReceiveSize:
		value = datagramRoom
	case f.sock.datagram != nil:
		p.unimplemented("getsockopt of a datagram socket other than its kind and its sizes", a)
		return -eNOSYS
	case a[2] == optionSendSize, a[2] == optionReceiveSize:
		value = socketSize
	case a[2] == optionSendLeast:
		value = uint32(f.sock.sendLeast())
	case a[2] == optionReceiveLeast:
		value = 1
	case a[2] == optionError: // nothing here goes wrong with a pair
	case a[2] == optionType:
		value = socketStream
	case a[2] == optionToRead:
		value = uint32(len(f.sock.data))
	case a[2] == optionToSend: // what was sent waits in the other, not in this one
	case a[2] == optionNoSignal:
		value = uint32(truth(f.noSigpipe))
	default:
		a[1] = 0
	}
	if a[1] != levelSocket || p.u32(a[4]) != 4 {
		p.unimplemented("getsockopt of an option that was not measured", a)
		return -eNOSYS
	}
	p.write(a[3], le32(value))
	return 0
}

// sysSetsockopt is setsockopt(socket, level, option, its value, the size of that). A socket
// that can neither send nor receive takes none.
func sysSetsockopt(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	switch {
	case errno != 0:
		return errno
	case f.sock.cantSend && f.sock.cantReceive:
		return -eINVAL
	case f.sock.events != nil:
		// Of the kernel's event socket, how much it holds of what has not been read, which
		// configd sets: at least one byte (EINVAL), as measured. The most a socket may hold is
		// the machine's own (8 MB on the Mac measured, where more is made that): more stops.
		if a[1] != levelSocket || a[2] != optionReceiveSize || a[4] != 4 {
			p.unimplemented("setsockopt of the kernel's event socket other than what it holds", a)
			return -eNOSYS
		}
		switch room := int32(p.u32(a[3])); {
		case room < 1:
			return -eINVAL
		case room > 8<<20:
			p.unimplemented("more room for a socket than was measured to be the most", a)
			return -eNOSYS
		default:
			f.sock.events.room = uint32(room)
		}
		return 0
	case a[1] == levelSocket && a[2] == optionSendLeast:
		// The least room it wants before it is written to, which rsync sets: as measured, no
		// more than a socket holds, and at least one byte (EINVAL, as for a number of another
		// size). A thread that waits to send is looked at again, as it may now have room enough.
		if a[4] != 4 || int32(p.u32(a[3])) < 1 {
			return -eINVAL
		}
		f.sock.least = min(int(p.u32(a[3])), socketSize)
		p.socketChanged(f.sock)
		return 0
	case a[1] != levelSocket || a[2] != optionNoSignal || a[4] != 4:
		p.unimplemented("setsockopt of an option that was not measured", a)
		return -eNOSYS
	}
	f.noSigpipe = p.u32(a[3]) != 0
	return 0
}

// socketName answers getsockname and getpeername with an address: the one a socket was bound
// to, as it was given and as long as it was said to be (as measured: 106 bytes of a
// sockaddr_un given whole), or for one with no name the 16 bytes that say so. As much of it as
// there is room for is written, and how long the whole of it is, as measured.
func (p *Process) socketName(a []uint64, name []byte) int64 {
	if name == nil {
		name = append([]byte{16, domainLocal}, make([]byte, 14)...)
	}
	n := min(uint64(p.u32(a[2])), uint64(len(name)))
	p.write(a[1], name[:n])
	p.write(a[2], le32(uint32(len(name))))
	return 0
}

func sysGetsockname(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	if f.sock.events != nil {
		p.unimplemented("the name of the kernel's event socket, which was not measured", a)
		return -eNOSYS
	}
	return p.socketName(a, f.sock.name)
}

// sysGetpeername is the other's name, which a socket that can neither send nor receive has
// none of.
func sysGetpeername(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	switch {
	case errno != 0:
		return errno
	case f.sock.events != nil:
		p.unimplemented("the name of what the kernel's event socket is connected to, which was not measured", a)
		return -eNOSYS
	case f.sock.datagram != nil && f.sock.datagram.to == nil:
		return -eNOTCONN
	case f.sock.datagram != nil:
		return p.socketName(a, f.sock.datagram.to.name)
	case f.sock.lone: // connected to nobody, as measured
		return -eNOTCONN
	case f.sock.cantSend && f.sock.cantReceive:
		return -eINVAL
	}
	return p.socketName(a, f.sock.peer.name)
}
