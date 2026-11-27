package emulator

// Messages over a socket (sendmsg, recvmsg): what is sent from several places and received
// into several, and the descriptors a program sends beside the bytes for another to have
// (SCM_RIGHTS). As measured on macOS 27.2.1 (examples/passfd.c) and as the kernel's source has
// it (unp_internalize and unp_externalize in bsd/kern/uipc_usrreq.c, soreceive in
// uipc_socket.c): a descriptor sent is one more for the same open file, held by the socket
// until it is received; whoever receives the first bytes of its message is given all of them
// as new descriptors, the lowest free, whether there is room to be told their numbers or not;
// with room for part of that, as much as fits is told and the message is marked as cut short;
// with none asked for, or by recv or read, nothing is told and the descriptors are open all
// the same. What is sent and never received goes when the socket does.

// rights are the descriptors sent with a message, and where in what waits to be received that
// message starts.
type rights struct {
	at    int
	files []*fd
	size  int // the room it takes in the socket until it is received
}

const (
	messageCut   = 0x20 // MSG_CTRUNC: not all that came beside the bytes was told
	rightsKind   = 1    // SCM_RIGHTS
	besideHeader = 12   // a struct cmsghdr: its length, its level and its kind
	eMSGSIZE     = 40
)

// messageHeader is a struct msghdr as a program gives it.
type messageHeader struct {
	name, parts, beside   uint64
	nameSize, count, room uint32
}

func (p *Process) messageHeader(addr uint64) messageHeader {
	b := p.read(addr, 48)
	return messageHeader{name: le.Uint64(b), nameSize: le.Uint32(b[8:]), parts: le.Uint64(b[16:]), count: le.Uint32(b[24:]), beside: le.Uint64(b[32:]), room: le.Uint32(b[40:])}
}

// stretches are the places a message's bytes are sent from or received to.
func (p *Process) stretches(h messageHeader) []stretch {
	var out []stretch
	for i := uint64(0); i < uint64(h.count); i++ {
		out = append(out, stretch{p.u64(h.parts + 16*i), p.u64(h.parts + 16*i + 8)})
	}
	return out
}

// sysSendmsg is sendmsg(socket, message, how). As measured: a message with no places for
// bytes is EMSGSIZE whatever else it has; what is beside the bytes has to be one lot of
// descriptors at the socket's level and as long as it is said to be (EINVAL), and each
// descriptor open (EBADF). From the source: the places are counted before the socket is looked
// at, what is beside is looked at after the socket is found able to send, a kqueue is not to be
// sent (EINVAL), nor more than 254 descriptors. Not measured, and stopping: a message to an
// address, descriptors with no bytes, a heading with no descriptors, and a session with the
// network's policy sent.
func sysSendmsg(p *Process, a []uint64) int64 {
	h := p.messageHeader(a[1])
	if int32(h.count) <= 0 || h.count > 1024 {
		return -eMSGSIZE
	}
	f, errno := p.socketOf(a[0])
	switch {
	case errno != 0:
		return errno
	case f.sock.datagram != nil:
		if h.beside != 0 {
			p.unimplemented("something beside the bytes of a message sent over a datagram socket", a)
			return -eNOSYS
		}
		var data []byte
		for _, part := range p.stretches(h) {
			data = append(data, p.read(part.addr, part.size)...)
		}
		return p.datagramSend(f, data, a[2], h.name, uint64(h.nameSize))
	case h.name != 0:
		p.unimplemented("sendmsg to an address, on a stream socket", a)
		return -eNOSYS
	case f.sock.events != nil:
		return -eOPNOTSUPP
	case f.sock.lone:
		return -eNOTCONN
	case f.sock.cantSend:
		return p.brokenPipe(f, a[2]&messageNoSignal != 0)
	}
	var data []byte
	for _, part := range p.stretches(h) {
		data = append(data, p.read(part.addr, part.size)...)
	}
	op := &pipeOp{f: f, data: data, sending: true, flags: a[2]}
	if h.beside != 0 {
		if h.room < besideHeader || h.room > besideHeader+4*254+4 {
			return -eINVAL
		}
		beside := p.read(h.beside, uint64(h.room))
		size, level, kind := le.Uint32(beside), le.Uint32(beside[4:]), le.Uint32(beside[8:])
		if level != levelSocket || kind != rightsKind || (size+3)&^3 != (h.room+3)&^3 || size < besideHeader {
			return -eINVAL
		}
		var numbers []uint64
		for i := uint32(besideHeader); i+4 <= size; i += 4 {
			numbers = append(numbers, uint64(int64(int32(le.Uint32(beside[i:])))))
		}
		for _, n := range numbers {
			switch file := p.fds[n]; {
			case file == nil:
				return -eBADF
			case file.kq != nil:
				return -eINVAL
			case file.necp != nil:
				p.unimplemented("a session with the network's policy sent over a socket", a)
				return -eNOSYS
			}
		}
		if len(numbers) == 0 || len(data) == 0 {
			p.unimplemented("a message with a heading and no descriptors, or descriptors and no bytes", a)
			return -eNOSYS
		}
		for _, n := range numbers {
			dup, err := p.fds[n].duplicate()
			if err != nil {
				return hostErrno(err)
			}
			op.files = append(op.files, dup)
		}
		op.besideSize = int(h.room)
	}
	return p.sendOp(op, false)
}

// sysRecvmsg is recvmsg(socket, message, how): the bytes into the message's places, and what
// came beside them where the message says, with how much of it and whether it was cut short.
// Nobody is named as whom it is from, the socket being connected.
func sysRecvmsg(p *Process, a []uint64) int64 {
	h := p.messageHeader(a[1])
	if int32(h.count) <= 0 || h.count > 1024 { // counted before the socket is looked at, as measured
		return -eMSGSIZE
	}
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	return p.socketReceiveInto(f, &pipeOp{f: f, into: p.stretches(h), flags: a[2], message: a[1]})
}

// takeRights gives the receiving process the descriptors that came with a message, and keeps
// what a recvmsg is to be told of them.
func (p *Process) takeRights(op *pipeOp, with *rights) {
	told := le32(uint32(besideHeader+4*len(with.files)), levelSocket, rightsKind)
	for _, file := range with.files {
		n := p.allocFD(0)
		if n >= p.fdLimit() {
			p.unimplemented("descriptors received over a socket with no room in the table for them", nil)
			p.dropFD(file)
			continue
		}
		p.fds[n] = file
		p.holds(n)
		told = append(told, le32(uint32(n))...)
	}
	op.beside = told
}

// received ends a receive: a recvmsg is told in its message how the receive was asked for and
// whether what came beside the bytes was cut short, how much of that there was where there is
// a place for it, and that nobody is named where there is a place for a name, as measured.
func (p *Process) received(op *pipeOp) int64 {
	if op.message == 0 {
		return op.done
	}
	h := p.messageHeader(op.message)
	marks := uint32(op.flags) | op.marks
	if h.beside != 0 {
		fits := min(len(op.beside), int(h.room))
		if fits > 0 {
			p.write(h.beside, op.beside[:fits])
		}
		if fits < len(op.beside) && h.room != 0 {
			marks |= messageCut
		}
		p.write(op.message+40, le32(uint32(fits)))
	}
	if h.name != 0 {
		var from []byte
		if op.named { // of a datagram: whom it was from, as much as there is room for
			from = address(op.from) // how long all of it is is said, as measured of recvfrom
			p.write(h.name, from[:min(len(from), int(h.nameSize))])
		}
		p.write(op.message+8, le32(uint32(len(from))))
	}
	p.write(op.message+44, le32(marks))
	return op.done
}
