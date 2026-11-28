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
// kernel was measured to answer (tests/kernel/dev/devfd.c).

const devFDs = "/dev/fd"

// descriptorAt is the descriptor a path names, from where the process is, if it names one.
func (p *Process) descriptorAt(path string) (uint64, bool) { return descriptorNamed(p.absolute(path)) }

// standardLink is the text of the link that /dev/stdin, /dev/stdout or /dev/stderr is.
func standardLink(path string) (string, bool) {
	switch path {
	case "/dev/stdin":
		return "fd/0", true
	case "/dev/stdout":
		return "fd/1", true
	case "/dev/stderr":
		return "fd/2", true
	}
	return "", false
}

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
	switch have, want := f.accessMode(), flags&3; {
	case flags&(oCreate|oExcl) == oCreate|oExcl:
		return -eEXIST // it is there
	case flags&oDirectory != 0:
		return -eNOTDIR // and is no directory, whatever the descriptor is of
	case have != 2 && have != want:
		return -eACCES
	}
	return p.dupFD(n, 0, false)
}

// A descriptor's name given to a call that takes a path (tests/kernel/dev/devfdpaths.c). A few act on
// the open file, as the call that takes the descriptor does; the rest are told of a file of the
// device file system that is no directory and no link, that is there already, and that is not
// to be run, renamed, linked or removed. Of a descriptor that is not open all are EBADF.
type descriptorPath struct {
	arg    int    // which argument is the path
	as     string // the call that does it to a descriptor, if it acts on the file
	refuse int64  // or what it is told
}

var descriptorPaths = map[string][]descriptorPath{
	"chmod": {{as: "fchmod"}}, "chown": {{as: "fchown"}}, "utimes": {{as: "futimes"}},
	"setxattr": {{refuse: ePERM}}, "getxattr": {{refuse: ePERM}}, "listxattr": {{refuse: ePERM}}, "removexattr": {{refuse: ePERM}},
	"readlink": {{refuse: eINVAL}}, "truncate": {{refuse: eINVAL}}, "pathconf": {{refuse: eINVAL}},
	"unlink": {{refuse: ePERM}}, "rename": {{refuse: eXDEV}, {arg: 1, refuse: eXDEV}}, "link": {{refuse: ePERM}}, "symlink": {{arg: 1, refuse: ePERM}},
	"mkdir": {{refuse: eEXIST}}, "mkfifo": {{refuse: eEXIST}}, "rmdir": {{refuse: eNOTDIR}}, "chdir": {{refuse: eNOTDIR}},
	"execve": {{refuse: eACCES}}, "posix_spawn": {{arg: 1, refuse: eACCES}},
}

// init has each of those calls look for a descriptor's name before it does what it does of any
// other path.
func init() {
	for call, rules := range descriptorPaths {
		of := bsdHandlers[call]
		if of.fn == nil {
			panic("no call " + call + " to take a descriptor's name")
		}
		for i := range rules {
			if as := rules[i].as; as != "" && bsdHandlers[as].fn == nil {
				panic("no call " + as + " to do " + call + " to a descriptor")
			}
		}
		bsdHandlers[call] = handler{of.args, func(p *Process, a []uint64) int64 {
			for _, rule := range rules {
				n, ok := p.descriptorAt(p.cstr(a[rule.arg]))
				if _, link := standardLink(p.absoluteLink(p.cstr(a[rule.arg]))); link && call == "readlink" {
					ok = false // /dev/stdin and the like are links, and are read as links
				}
				switch {
				case !ok:
					continue
				case p.fds[n] == nil:
					return -eBADF
				case rule.as == "":
					return -rule.refuse
				}
				a[rule.arg] = n
				return bsdHandlers[rule.as].fn(p, a)
			}
			return of.fn(p, a)
		}}
	}
}
