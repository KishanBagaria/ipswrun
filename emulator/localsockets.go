package emulator

import (
	"bytes"
	"syscall"
	"time"
)

// Sockets of the local domain with a name in the file system: one is bound to a path and
// listens, others connect to the path and are accepted, and each accepted end and the one that
// connected are then as the two of a pair (see sockets.go). They are as the kernel's source has
// them (unp_bind, unp_connect and unp_listen in bsd/kern/uipc_usrreq.c; solisten and sonewconn
// in uipc_socket.c and uipc_socket2.c) and as measured on macOS 27.2.1 (tests/kernel/sockets/localsock.c).
// Stream sockets only: a datagram socket, and a socket of any other domain but the system's
// one for events, is refused as before, there being no network. The name is a file of the
// host's under the home or a mount that the machine knows to be a socket, as a named pipe's
// is (see sysMkfifo).

const (
	eDESTADDRREQ  = 39
	eAFNOSUPPORT  = 47
	eADDRINUSE    = 48
	eISCONN       = 56
	eCONNABORTED  = 53
	eCONNREFUSED  = 61
	mostWaiting   = 128 // kern.ipc.somaxconn: what a backlog is at most, and when none is given
	localAddress  = 106 // a sockaddr_un: its length, the family, and 104 bytes of path
	mostOfAddress = 255 // SOCK_MAXADDRLEN
)

// localSocket is socket(AF_UNIX, SOCK_STREAM, 0): a socket connected to nobody. It has for its
// other end one that is nobody's until it is connected.
func (p *Process) localSocket(datagram bool) int64 {
	if p.full() {
		return -eMFILE
	}
	p.m.pipes++
	pair := &socketPair{id: p.m.pipes}
	one, nobody := &socket{pair: pair, lone: !datagram}, &socket{pair: pair, second: true}
	if datagram {
		one.datagram = &datagrams{}
	}
	one.peer, nobody.peer = nobody, one
	const readWrite = 2 // O_RDWR
	return int64(p.newFD(&fd{kind: socketFD, sock: one, flags: readWrite, refs: 1}))
}

// localPath is the path an address of the local domain names, and the address as it was given,
// with its length said in it as the kernel says it. As measured: an address with nowhere to
// read it from is EDESTADDRREQ to bind and EINVAL to connect, and one that names no family is
// taken as one of this domain. From the source: one longer than an address may be is
// ENAMETOOLONG, one too short to hold a path EINVAL, and one of another family EAFNOSUPPORT.
func (p *Process) localPath(addr, size uint64, binding bool) (string, []byte, int64) {
	switch {
	case size > mostOfAddress:
		return "", nil, -eNAMETOOLONG
	case addr == 0 && binding:
		return "", nil, -eDESTADDRREQ
	case addr == 0, size < 3:
		return "", nil, -eINVAL
	}
	given := p.read(addr, size)
	if given[1] != domainLocal && given[1] != 0 {
		return "", nil, -eAFNOSUPPORT
	}
	given[0] = byte(size)
	path := given[2:]
	if end := bytes.IndexByte(path, 0); end >= 0 {
		path = path[:end]
	}
	return string(path), given, 0
}

// sysBind is bind(socket, address, its size): the socket is given a name, a file that is made
// for it and may not be there already (EADDRINUSE, whatever is there). One that has a name is
// given no other (EINVAL), as measured. The file's mode is what the process's mask leaves of
// all permissions, as measured. Not measured: a name outside the home and the mounts, where
// nothing can be made here (EACCES, as for a named pipe), and binding one of a pair, which stops.
func sysBind(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	s := f.sock
	if s.events != nil || !s.lone && s.datagram == nil && s.name == nil {
		p.unimplemented("bind of a socket that is one of a pair, or the kernel's for events", a)
		return -eNOSYS
	}
	path, given, errno := p.localPath(a[1], a[2], true)
	switch {
	case errno != 0:
		return errno
	case s.bound:
		return -eINVAL
	case s.name != nil:
		// An end that was accepted is named as the one that listened and has no file of its
		// own: the source would bind it, which was not measured.
		p.unimplemented("bind of a socket that was accepted", a)
		return -eNOSYS
	case path == "":
		p.unimplemented("bind of a socket to no path", a)
		return -eNOSYS
	}
	t, ok := p.writableTarget(path)
	if !ok {
		return p.notMade(path, -eADDRINUSE, false)
	}
	h, err := openInRoot(t.root, t.rel, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY, 0o600)
	if err != nil {
		if errno := hostErrno(err); errno != -eEXIST {
			return errno
		}
		return -eADDRINUSE
	}
	defer syscall.Close(h)
	var st syscall.Stat_t
	if err := syscall.Fstat(h, &st); err != nil {
		return hostErrno(err)
	}
	p.m.sockets[hostFileOf(&st)] = s
	s.name, s.bound = given, true
	p.created(t)
	return hostCall(syscall.Fchmod(h, 0o777&^p.umask()))
}

// sysListen is listen(socket, how many may wait): one with a name takes those who connect to
// it from then on. One with no name is refused (EINVAL), as measured. How many may wait is at
// most 128, and that when none or less is said, by the source; as many as that and no more
// may wait, as measured.
func sysListen(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	s := f.sock
	switch {
	case s.datagram != nil: // nobody connects to one of these to be accepted, as measured
		return -eOPNOTSUPP
	case s.events != nil || !s.lone:
		p.unimplemented("listen on a socket that is connected, or the kernel's for events", a)
		return -eNOSYS
	case !s.bound:
		return -eINVAL
	}
	s.listening, s.backlog = true, int(int32(a[1]))
	if s.backlog <= 0 || s.backlog > mostWaiting {
		s.backlog = mostWaiting
	}
	return 0
}

// sysConnect is connect(socket, address, its size) to the socket bound to a path that listens
// there: an end is made for the one that listens to accept, and the two are connected at once,
// accepted or not, as measured. As measured: ENOENT of a path that is not there, ECONNREFUSED
// of a socket that does not listen or has been closed, EISCONN of one connected already. From
// ECONNREFUSED when as many wait as may. From the source, not measured: ENOTSOCK of a path that
// is no socket's and EOPNOTSUPP of a socket that itself listens. Whether the one that connects
// may write to the file is not asked, owners and modes not being enforced here.
func sysConnect(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	s := f.sock
	if s.events != nil {
		p.unimplemented("connect of the kernel's event socket", a)
		return -eNOSYS
	}
	if s.datagram != nil {
		// One connected already is connected to nobody before the address is judged, by the
		// source, but for an address that cannot be read at all.
		switch {
		case a[2] > mostOfAddress:
			return -eNAMETOOLONG
		case a[1] == 0:
			return -eINVAL
		}
		s.disconnect()
		path, _, errno := p.localPath(a[1], a[2], false)
		if errno != 0 {
			return errno
		}
		return p.datagramConnect(s, path)
	}
	path, _, errno := p.localPath(a[1], a[2], false) // the address is looked at first, by the source
	switch {
	case errno != 0:
		return errno
	case s.listening:
		return -eOPNOTSUPP
	case !s.lone && s.cantSend && s.cantReceive:
		// One whose other end has gone is connected no more, to the source, and may connect
		// again, which was not measured.
		p.unimplemented("connect of a socket whose other end has gone", a)
		return -eNOSYS
	case !s.lone:
		return -eISCONN
	}
	t, ok := p.writableTarget(path)
	if !ok {
		if _, there := p.resolve(path); there {
			return -eNOTSOCK
		}
		return -eNOENT
	}
	var st syscall.Stat_t
	if err := statInRoot(t.root, t.rel, true, &st); err != nil {
		return hostErrno(err)
	}
	to := p.m.sockets[hostFileOf(&st)]
	switch {
	case to == nil:
		return -eNOTSOCK
	case to.datagram != nil && to.datagram.closed: // nobody has it, which is said first, by the source
		return -eCONNREFUSED
	case to.datagram != nil: // a socket of another kind, as measured
		return -ePROTOTYPE
	case !to.listening, len(to.pending) >= to.backlog: // as many wait as may, as measured
		return -eCONNREFUSED
	}
	end := &socket{pair: s.pair, second: true, name: to.name, peer: s}
	s.peer, s.lone = end, false
	to.pending = append(to.pending, end)
	p.socketChanged(to)
	p.socketChanged(s) // it can be written now, to who watches for that
	return 0
}

// sysAccept is accept(socket, where the address of who connected goes, its size): a new
// descriptor for the end made for the one that connected first and was not yet accepted. With
// nobody there it waits, or for one that does not wait is EAGAIN; of a socket that does not
// listen it is EINVAL, as measured. The new descriptor waits or not as the one that listens
// does, by the source.
func sysAccept(p *Process, a []uint64) int64 {
	f, errno := p.socketOf(a[0])
	if errno != 0 {
		return errno
	}
	if a[1] != 0 {
		p.u32(a[2]) // where the address's size is said is read before anything else, by the source
	}
	switch {
	case f.sock.events != nil: // the kernel's own takes nobody, by the source
		return -eOPNOTSUPP
	case f.sock.datagram != nil: // nor does a datagram socket, as measured
		return -eOPNOTSUPP
	case !f.sock.listening:
		return -eINVAL
	}
	op := &pipeOp{f: f, accepting: true, address: a[1], length: a[2]}
	if r, ok := p.tryAccept(op); ok {
		return r
	}
	if f.flags&flagNonblock != 0 {
		return -eAGAIN
	}
	p.current.pipeOp = op
	p.wait(waitPipe, f.sock.pair.id, time.Time{}, result{})
	return 0
}

// tryAccept gives the first that waits a descriptor, if any does.
func (p *Process) tryAccept(op *pipeOp) (int64, bool) {
	s := op.f.sock
	switch {
	case !s.listening: // closed by another thread meanwhile: from the source, not measured
		return -eCONNABORTED, true
	case len(s.pending) == 0:
		return 0, false
	}
	end := s.pending[0]
	s.pending = s.pending[1:]
	if p.full() {
		// With no descriptor to give, the one that waited is let go, by the source: who
		// connected finds the other end gone.
		end.cantSend, end.cantReceive = true, true
		end.peer.cantSend, end.peer.cantReceive = true, true
		p.socketChanged(end)
		return -eMFILE, true
	}
	const readWrite = 2 // O_RDWR
	n := p.newFD(&fd{kind: socketFD, sock: end, flags: readWrite | op.f.flags&flagNonblock, refs: 1, noSigpipe: op.f.noSigpipe})
	if op.address != 0 && op.length != 0 {
		p.socketName([]uint64{0, op.address, op.length}, end.peer.name)
	}
	p.socketChanged(s)
	return int64(n), true
}
