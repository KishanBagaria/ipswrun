package emulator

import (
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// The standard input a run was started with is the host's, and where it is no terminal the host
// is asked whether it can be read: a file always can, a pipe when something is in it or nobody
// is left to write, and of nothing (/dev/null) poll says it is no descriptor to ask of while
// select finds it ready both ways. As measured on macOS 27.2.1 (examples/input.c, and by hand
// of a file and of pipes). A pipe with nothing in it yet is not known to be ready, and a wait
// for it stops the run: nothing here wakes a thread when the host's input comes.

type hostInput int

const (
	inputOther hostInput = iota // a terminal, or what was not measured
	inputFile
	inputNothing
	inputPipe
)

var nullDevice = sync.OnceValue(func() uint64 {
	var st syscall.Stat_t
	if syscall.Stat("/dev/null", &st) != nil {
		return ^uint64(0)
	}
	return uint64(st.Rdev)
})

// input says what kind of thing the run's own standard input is, for a descriptor of it; and
// that a descriptor is of /dev/null, however it was come by.
func (f *fd) input() hostInput {
	if f.kind != hostFD && (f.kind != stdioFD || f.host != 0) {
		return inputOther
	}
	var st syscall.Stat_t
	if syscall.Fstat(f.host, &st) != nil {
		return inputOther
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		if f.kind == stdioFD {
			return inputFile
		}
	case syscall.S_IFCHR:
		if uint64(st.Rdev) == nullDevice() {
			return inputNothing
		}
	case syscall.S_IFIFO:
		if f.kind == stdioFD {
			return inputPipe
		}
	}
	return inputOther
}

// hostReadable asks the host whether its pipe can be read now, and whether nobody is left to
// write to it. Hosts differ in what they say of an ended pipe with nothing in it; either way it
// can be read, of nothing.
func (f *fd) hostReadable() (readable, ended bool) {
	asked := []unix.PollFd{{Fd: int32(f.host), Events: unix.POLLIN}}
	if n, err := unix.Poll(asked, 0); err != nil || n == 0 {
		return false, false
	}
	ended = asked[0].Revents&unix.POLLHUP != 0
	return ended || asked[0].Revents&unix.POLLIN != 0, ended
}

// inputPolled is what poll answers of the run's standard input, or of /dev/null; false where
// nothing here says.
func (f *fd) inputPolled(events uint16) (uint16, bool) {
	switch f.input() {
	case inputFile:
		return events & (pollReading | pollWriting), true
	case inputNothing:
		return pollInvalid, true
	case inputPipe:
		readable, ended := f.hostReadable()
		switch {
		case ended:
			return events&pollReading | pollHangup, true
		case readable:
			return events & pollReading, true
		}
	}
	return 0, false
}

// inputSelected says whether select finds the run's standard input, or /dev/null, ready to be
// read or written, and whether anything here says: a pipe is found writable though it is the
// end that is read, as measured.
func (f *fd) inputSelected(writing bool) (ready, known bool) {
	switch f.input() {
	case inputFile, inputNothing:
		return true, true
	case inputPipe:
		if writing {
			return true, true
		}
		readable, _ := f.hostReadable()
		return readable, true
	}
	return false, false
}

func (f *fd) inputReady(writing bool) bool {
	ready, _ := f.inputSelected(writing)
	return ready
}
