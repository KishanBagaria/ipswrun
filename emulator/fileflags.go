package emulator

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// What the kernel keeps of an open file besides what it was opened to do, as fcntl reads it
// (F_GETFL) and sets it (F_SETFL): whether writes go to the end, whether it waits, and the like,
// and whether it has been written. It is the open file's and not a descriptor's, so a duplicate
// and a copy of the process have the same one, as a Mac's kernel was measured to keep it
// (examples/fileflags.c). A pipe's and a socket's are their own (see pipes.go).
type status struct {
	flags       uint32
	descriptors int // of a file of the host's: how many descriptors it has, in every process (see rangelocks.go)
}

const (
	flagFsync   = 0x80     // O_FSYNC
	flagEvtOnly = 0x8000   // O_EVTONLY
	flagNoCache = 0x40000  // what it reads and writes is not to be kept in memory (F_NOCACHE)
	flagNoAhead = 0x80000  // nor read ahead of where it is read (F_RDAHEAD)
	flagDsync   = 0x400000 // O_DSYNC
	oTruncate   = 0x400

	// Those of an open's flags that the file keeps, and those that may be set afterwards: the
	// rest of what F_SETFL is given is left alone and is no error.
	keptAtOpen   = flagNonblock | flagAppend | flagAsync | flagFsync | flagEvtOnly | flagDsync
	fileSettable = flagNonblock | flagAppend | flagAsync | flagFsync | flagDsync
)

// status is the open file's, made when it is first asked for.
func (f *fd) status() *status {
	if f.state == nil {
		f.state = &status{}
	}
	return f.state
}

// opened notes what an open asked that the file keeps. One that was emptied has been written.
func (f *fd) opened(flags uint64) {
	if f.status().flags = uint32(flags) & keptAtOpen; flags&oTruncate != 0 {
		f.wrote()
	}
}

// fileFlags is F_GETFL of what is no pipe and no socket.
func (f *fd) fileFlags() int64 {
	return int64(f.accessMode() | uint64(f.status().flags))
}

// setFileFlags is F_SETFL of what is no pipe and no socket. Appending is the host's to do, for a
// file of the host's; the rest is only kept, a file being always ready and never signalling.
// What the run itself was given to read and write may wait, and not waiting on it stops the run.
func (p *Process) setFileFlags(f *fd, arg uint64) int64 {
	if f.kind == stdioFD && arg&flagNonblock != 0 {
		p.unimplemented("not waiting (O_NONBLOCK) on what the run was given to read and write", nil)
		return -eINVAL
	}
	s := f.status()
	s.flags = s.flags&^fileSettable | uint32(arg)&fileSettable
	if f.kind == hostFD {
		host, err := unix.FcntlInt(uintptr(f.host), unix.F_GETFL, 0)
		if err != nil {
			return hostErrno(err)
		}
		if host &^= unix.O_APPEND; s.flags&flagAppend != 0 {
			host |= unix.O_APPEND
		}
		if _, err := unix.FcntlInt(uintptr(f.host), unix.F_SETFL, host); err != nil {
			return hostErrno(err)
		}
	}
	return 0
}

// wrote notes that a file has been written, or cut to a length.
func (f *fd) wrote() { f.status().flags |= flagWritten }

// hostDup is another descriptor of the host's for the same open file of the host's, for a
// duplicate or a copy of the process: its own number, and the open file's one status.
func (f *fd) hostDup() (*fd, error) {
	h, err := syscall.Dup(f.host)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(h)
	f.status().descriptors++
	return &fd{kind: hostFD, host: h, path: f.path, state: f.state}, nil
}

// hostClose closes a descriptor of the host's that hostDup or an open made. The locks the open
// file holds on parts of files go with its last descriptor.
func (p *Process) hostClose(f *fd) {
	closeHost(f.host)
	s := f.status()
	if s.descriptors--; s.descriptors == 0 {
		p.m.unlockRanges(s)
	}
}
