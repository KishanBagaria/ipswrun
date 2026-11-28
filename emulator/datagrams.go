package emulator

import (
	"syscall"
	"time"
)

// Datagram sockets of the local domain: each send is one message, received whole or cut short,
// to the other of a pair, to the socket it was connected to, or to the one bound to a path it
// names. As the kernel's source has them (uipc_send, unp_connect, unp_disconnect and unp_drop
// in bsd/kern/uipc_usrreq.c; sosend, soreceive and soconnectlock in uipc_socket.c; sbappendaddr
// in uipc_socket2.c) and as measured on macOS 27.2.1 (tests/kernel/sockets/localdgram.c).
//
// How many messages a socket holds is the one thing here not known for every release. A
// message is taken while there is room for its bytes and its sender's address both in the
// bytes a socket holds (4096) and in the memory the kernel keeps them in (32768), where each
// message takes a buffer for the address and one for the bytes, with a larger one when the
// bytes are many. On macOS 27.2.1 that was measured to fit a buffer of 512 bytes with room for
// 288 in it; the source has 256 for kernels built another way, and which a release is cannot
// be read from it. So the bytes always decide, and the memory decides only where the least a
// message can take and the most agree; where they do not, the run stops and says so.

const (
	datagramMost   = 2048  // the most bytes in one message (SO_SNDBUF)
	datagramRoom   = 4096  // the bytes a socket holds, addresses counted (SO_RCVBUF)
	datagramMemory = 32768 // the memory they may take: eight times that
	bufferLeast    = 256   // what a buffer takes at least, of the two sizes the source has
	bufferMost     = 512   // and at most
	clusterSize    = 2048  // the larger buffer for many bytes
	messageShort   = 0x10  // MSG_TRUNC: the message was longer than the room for it

	eCONNRESET = 54
	eNOBUFS    = 55
)

// datagrams is what a datagram socket has: the messages that wait to be received, with how
// many bytes they and their senders' addresses come to; the socket it sends to when none is
// named, and those that send to it so; that something went wrong it has yet to say (the one it
// was connected to closed); and that it is closed itself, its name perhaps still there.
type datagrams struct {
	messages []datagram
	bytes    int
	to       *socket
	from     []*socket
	reset    bool
	wasReset bool
	closed   bool
}

type datagram struct {
	data []byte
	from []byte // the sender's address as it was bound, nil of one with no name
}

// address is a socket's address as it is said: the one it was bound to, or the 16 bytes of one
// with no name.
func address(name []byte) []byte {
	if name == nil {
		return append([]byte{16, domainLocal}, make([]byte, 14)...)
	}
	return name
}

// datagramPair is socketpair of datagram sockets: two, each sending to the other.
func (p *Process) datagramPair(out uint64) int64 {
	var ends [2]*socket
	for i := range ends {
		p.m.pipes++
		pair := &socketPair{id: p.m.pipes}
		ends[i] = &socket{pair: pair, datagram: &datagrams{}}
		nobody := &socket{pair: pair, second: true}
		ends[i].peer, nobody.peer = nobody, ends[i]
	}
	ends[0].datagram.to, ends[1].datagram.to = ends[1], ends[0]
	ends[0].datagram.from, ends[1].datagram.from = []*socket{ends[1]}, []*socket{ends[0]}
	const readWrite = 2 // O_RDWR
	first := p.newFD(&fd{kind: socketFD, sock: ends[0], flags: readWrite, refs: 1})
	second := p.newFD(&fd{kind: socketFD, sock: ends[1], flags: readWrite, refs: 1})
	p.write(out, le32(uint32(first), uint32(second)))
	return 0
}

// datagramAt is the datagram socket bound to a path: ENOENT if nothing is there, ECONNREFUSED
// if the one that was has closed, as measured; ENOTSOCK of a file that is no socket's and
// EPROTOTYPE of a stream socket's, from the source.
func (p *Process) datagramAt(path string) (*socket, int64) {
	t, ok := p.writableTarget(path)
	if !ok {
		if _, there := p.resolve(path); there {
			return nil, -eNOTSOCK
		}
		return nil, -eNOENT
	}
	var st syscall.Stat_t
	if err := statInRoot(t.root, t.rel, true, &st); err != nil {
		return nil, hostErrno(err)
	}
	switch to := p.m.sockets[hostFileOf(&st)]; {
	case to == nil:
		return nil, -eNOTSOCK
	case to.datagram == nil && to.cantSend && to.cantReceive: // closed: nobody has it, which is said first, by the source
		return nil, -eCONNREFUSED
	case to.datagram == nil:
		return nil, -ePROTOTYPE
	case to.datagram.closed:
		return nil, -eCONNREFUSED
	default:
		return to, 0
	}
}

// disconnect has a datagram socket send to nobody when none is named.
func (s *socket) disconnect() {
	d := s.datagram
	if d.to == nil {
		return
	}
	others := d.to.datagram
	for i, other := range others.from {
		if other == s {
			others.from = append(others.from[:i:i], others.from[i+1:]...)
			break
		}
	}
	d.to = nil
}

// datagramConnect is connect of a datagram socket to a path: what it sends to from then on
// when none is named. One connected already is first connected to nobody, by the source, so
// that a path that is not there (ENOENT, as measured) leaves it so.
func (p *Process) datagramConnect(s *socket, path string) int64 {
	to, errno := p.datagramAt(path)
	if errno != 0 {
		return errno
	}
	s.datagram.to = to
	to.datagram.from = append(to.datagram.from, s)
	return 0
}

// datagramClosed is the last descriptor of a datagram socket closing: what waited in it is
// gone, and those that sent to it when none was named send to nobody now and have that to
// say (ECONNRESET, as measured of the other of a pair).
func (p *Process) datagramClosed(s *socket) {
	d := s.datagram
	s.disconnect()
	// Nobody who waits on one of those is woken for it, by the source (unp_drop).
	for _, other := range d.from {
		other.datagram.to, other.datagram.reset = nil, true
	}
	d.from, d.messages, d.bytes, d.closed = nil, nil, 0, true
}

// datagramSend sends one message: to the address named, or to the socket connected to. As
// measured: with nobody to send to it is EDESTADDRREQ, more than 2048 bytes EMSGSIZE, an
// address named by one that is connected EISCONN, and a socket with no room ENOBUFS, at once
// and without waiting. From the source: what went wrong before is said first, and once.
func (p *Process) datagramSend(f *fd, data []byte, flags, addr, size uint64) int64 {
	s := f.sock
	d := s.datagram
	if flags&^(messageDontWait|messageNoSignal) != 0 {
		p.unimplemented("a send to a datagram socket asked for in a way that was not measured", []uint64{flags})
		return -eNOSYS
	}
	if addr != 0 && size > mostOfAddress {
		return -eNAMETOOLONG
	}
	if len(s.name) > bufferLeast-32 {
		// An address this long has no room in the smaller of the buffers the source has, where
		// every send from it would be refused, and has in the larger.
		p.unimplemented("a datagram sent from a socket bound to an address of more than 224 bytes", nil)
		return -eNOSYS
	}
	switch {
	case d.reset:
		d.reset = false
		return -eCONNRESET
	case addr == 0 && d.to == nil:
		return -eDESTADDRREQ
	case len(data) > datagramMost:
		return -eMSGSIZE
	case addr != 0 && d.to != nil:
		return -eISCONN
	}
	to := d.to
	if addr != 0 { // the address is judged last, by the source
		path, _, errno := p.localPath(addr, size, false)
		if errno != 0 {
			return errno
		}
		if to, errno = p.datagramAt(path); errno != 0 {
			return errno
		}
	}
	// Room: for the bytes and the sender's address, in bytes and in memory (see above).
	q := to.datagram
	need := len(data) + len(address(s.name))
	least, most := 0, 0
	for _, m := range q.messages {
		least += 2 * bufferLeast
		if most += 2 * bufferMost; len(m.data) > 0 {
			most += clusterSize
		}
	}
	switch {
	case datagramRoom-q.bytes < need, datagramMemory-least < need:
		return -eNOBUFS
	case datagramMemory-most < need:
		p.unimplemented("a datagram socket with this many messages waiting: whether it has room for more is the kernel's own arithmetic of its buffers, which differs between releases", []uint64{uint64(len(q.messages)), uint64(len(data))})
		return -eNOSYS
	}
	q.messages = append(q.messages, datagram{data: data, from: s.name})
	q.bytes += need
	p.socketChanged(to)
	return int64(len(data))
}

// datagramReceive receives one message: as much of it as there is room for, the rest lost, as
// measured. With none there it waits, or for one that does not wait is EAGAIN.
func (p *Process) datagramReceive(f *fd, op *pipeOp) int64 {
	d := f.sock.datagram
	for _, s := range op.into {
		op.room += s.size
	}
	switch {
	case op.flags&^(messagePeek|messageDontWait) != 0:
		p.unimplemented("a receive from a datagram socket asked for in a way that was not measured", []uint64{op.flags})
		return -eNOSYS
	case op.room == 0:
		p.unimplemented("a receive from a datagram socket into no room", nil)
		return -eNOSYS
	}
	if r, ok := p.tryDatagram(op); ok {
		return r
	}
	switch {
	case f.flags&flagNonblock != 0 || op.flags&messageDontWait != 0:
		return -eAGAIN
	case d.wasReset && d.to == nil:
		p.unimplemented("a wait to receive from a datagram socket whose other has gone and said so", nil)
		return -eNOSYS
	}
	p.current.pipeOp = op
	p.wait(waitPipe, f.sock.pair.id, time.Time{}, result{})
	return 0
}

// tryDatagram gives the first message that waits, if one does, or what went wrong.
func (p *Process) tryDatagram(op *pipeOp) (int64, bool) {
	s := op.f.sock
	d := s.datagram
	if len(d.messages) == 0 {
		if d.reset {
			if op.flags&messagePeek == 0 { // a look leaves it to be said again, by the source
				d.reset, d.wasReset = false, true
			}
			return -eCONNRESET, true
		}
		return 0, false
	}
	m := d.messages[0]
	n := min(uint64(len(m.data)), op.room)
	p.put(op, m.data[:n])
	op.done = int64(n)
	if n < uint64(len(m.data)) {
		op.marks |= messageShort
	}
	op.from, op.named = m.from, true
	// recvfrom: whom it is from, as much as there is room for, and how long all of it is, as
	// measured; with nowhere to say how long, nothing.
	if op.address != 0 && op.length != 0 && op.message == 0 {
		from := address(m.from)
		p.write(op.address, from[:min(len(from), int(p.u32(op.length)))])
		p.write(op.length, le32(uint32(len(from))))
	}
	if op.flags&messagePeek == 0 {
		d.messages = d.messages[1:]
		d.bytes -= len(m.data) + len(address(m.from))
		p.socketChanged(s)
	}
	return p.received(op), true
}
