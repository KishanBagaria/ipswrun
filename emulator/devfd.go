package emulator

import (
	pathpkg "path"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The descriptors a process has, as files: /dev/fd/3 is what descriptor 3 is, and /dev/stdin,
// /dev/stdout and /dev/stderr are 0, 1 and 2. Opening one is duplicating the descriptor, for no
// more than the descriptor may do, and asking of one is asking of the descriptor, as a Mac's
// kernel was measured to answer (examples/devfd.c).

const devFDs = "/dev/fd"

// descriptorAt is the descriptor a path names, from where the process is, if it names one.
func (p *Process) descriptorAt(path string) (uint64, bool) { return descriptorNamed(p.absolute(path)) }

// descriptorNamed is the descriptor a whole path under /dev names, if it names one.
func descriptorNamed(path string) (uint64, bool) {
	switch path = pathpkg.Clean(path); path {
	case "/dev/stdin":
		return 0, true
	case "/dev/stdout":
		return 1, true
	case "/dev/stderr":
		return 2, true
	}
	digits, ok := strings.CutPrefix(path, devFDs+"/")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 31)
	return n, err == nil
}

// accessMode is what a descriptor was opened to do: 0 to read, 1 to write, 2 for both. Of a host
// file, and of what the run itself was given to read and write, it is what the host says. Shared
// memory keeps what shm_open was asked, and a kqueue is for both, as a Mac's says. The files of
// the build and the directories are only ever read.
func (f *fd) accessMode() uint64 {
	switch {
	case f.kind == hostFD || f.kind == stdioFD:
		flags, err := unix.FcntlInt(uintptr(f.host), unix.F_GETFL, 0)
		if err != nil {
			panic(fault{"the access mode of a host descriptor: " + err.Error()})
		}
		return uint64(flags & unix.O_ACCMODE)
	case f.streams() || f.shared != nil:
		return uint64(f.flags & 3)
	case f.kq != nil:
		return 2
	}
	return 0
}

// openDescriptor is open of a descriptor's name: another descriptor for the same open file.
// What it is opened for must be what the descriptor may do (EACCES), and nothing else that is
// asked of an open (to empty the file, to make it) is done.
func (p *Process) openDescriptor(n, flags uint64) int64 {
	f := p.fds[n]
	if f == nil {
		return -eBADF
	}
	if have, want := f.accessMode(), flags&3; have != 2 && have != want {
		return -eACCES
	}
	return p.dupFD(n, 0, false)
}
