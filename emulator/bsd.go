package emulator

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/KishanBagaria/ipswrun/dyldcache"
)

// handler implements one system call or Mach trap. args is how many registers it takes, which is
// also how many the trace shows; fn returns a result, or a negated errno (BSD calls only).
type handler struct {
	args int
	fn   func(p *Process, a []uint64) int64
}

func constant(v int64) func(*Process, []uint64) int64 {
	return func(*Process, []uint64) int64 { return v }
}

var bsdHandlers map[string]handler

func init() {
	bsdHandlers = map[string]handler{
		// files
		"stat64": {2, sysStat64}, "lstat64": {2, sysLstat64}, "fstatat64": {4, sysFstatat64}, "fstat64": {2, sysFstat64},
		"open": {3, sysOpen}, "openat": {4, sysOpenat}, "guarded_open_np": {5, sysGuardedOpen}, "open_dprotected_np": {5, sysOpenDprotected}, "guarded_open_dprotected_np": {7, sysGuardedOpenDprotected}, "guarded_close_np": {2, sysGuardedClose},
		"guarded_write_np": {4, sysGuardedWrite}, "guarded_pwrite_np": {5, sysGuardedPwrite}, "close": {1, sysClose},
		"read": {3, sysRead}, "pread": {4, sysPread}, "write": {3, sysWrite}, "writev": {3, sysWritev}, "readv": {3, sysReadv}, "socketpair": {4, sysSocketpair}, "sendto": {6, sysSendto}, "recvfrom": {6, sysRecvfrom}, "shutdown": {2, sysShutdown}, "getsockopt": {5, sysGetsockopt}, "setsockopt": {5, sysSetsockopt}, "getsockname": {3, sysGetsockname}, "getpeername": {3, sysGetpeername}, "sendmsg": {3, sysSendmsg}, "recvmsg": {3, sysRecvmsg}, "bind": {3, sysBind}, "listen": {2, sysListen}, "connect": {3, sysConnect}, "accept": {3, sysAccept}, "pwrite": {4, sysPwrite}, "lseek": {3, sysLseek},
		"ftruncate": {2, sysFtruncate}, "fsync": {1, sysFsync}, "sync": {0, sysSync}, "umask": {1, sysUmask}, "iopolicysys": {2, sysIopolicysys}, "memorystatus_get_level": {1, sysMemorystatusGetLevel}, "getpriority": {2, sysGetpriority}, "setpriority": {3, sysSetpriority}, "access": {2, sysAccess}, "unlink": {1, sysUnlink},
		"mkdir": {2, sysMkdir}, "rename": {2, sysRename}, "readlink": {3, sysReadlink}, "readlinkat": {4, sysReadlinkat},
		"mkdirat": {3, sysMkdirat}, "rmdir": {1, sysRmdir}, "unlinkat": {3, sysUnlinkat}, "renameat": {4, sysRenameat},
		"renameatx_np": {5, sysRenameatx}, "faccessat": {4, sysFaccessat}, "truncate": {2, sysTruncate}, "fcntl": {3, sysFcntl}, "dup": {1, sysDup}, "dup2": {2, sysDup2},
		"chdir": {1, sysChdir}, "fchdir": {1, sysFchdir}, "chmod": {2, sysChmod}, "identitysvc": {2, sysIdentitysvc}, "necp_session_open": {1, sysNecpSessionOpen}, "necp_session_action": {6, sysNecpSessionAction}, "socket": {3, sysSocket}, "chflags": {2, sysChflags}, "fchflags": {2, sysFchflags}, "fchmod": {2, sysFchmod}, "fchmodat": {4, sysFchmodat}, "fchmod_extended": {5, sysFchmodExtended}, "chmod_extended": {5, sysChmodExtended}, "symlink": {2, sysSymlink}, "symlinkat": {3, sysSymlinkat}, "link": {2, sysLink}, "linkat": {5, sysLinkat}, "stat64_extended": {4, sysStat64Extended}, "lstat64_extended": {4, sysLstat64Extended}, "fstat64_extended": {4, sysFstat64Extended}, "chown": {3, sysChown}, "fchownat": {5, sysFchownat},
		"lchown": {3, sysLchown}, "fchown": {3, sysFchown}, "utimes": {2, sysUtimes}, "futimes": {2, sysFutimes},
		"setattrlist": {5, sysSetattrlist}, "setattrlistat": {6, sysSetattrlistat}, "mkfifo": {2, sysMkfifo}, "getgroups": {2, sysGetgroups}, "fsetattrlist": {5, sysFsetattrlist}, "flock": {2, sysFlock},
		"getxattr": {6, sysGetxattr}, "fgetxattr": {6, sysFgetxattr}, "setxattr": {6, sysSetxattr}, "fsetxattr": {6, sysFsetxattr},
		"listxattr": {4, sysListxattr}, "flistxattr": {4, sysFlistxattr}, "removexattr": {3, sysRemovexattr}, "fremovexattr": {3, sysFremovexattr},
		"ioctl": {3, sysIoctl}, "pipe": {0, sysPipe}, "poll": {3, sysPoll}, "fsctl": {4, constant(-eNOTSUP)}, "statfs64": {2, sysStatfs64}, "fstatfs64": {2, sysFstatfs64},
		"getattrlist": {5, sysGetattrlist}, "getattrlistat": {6, sysGetattrlistat}, "fgetattrlist": {5, sysFgetattrlist},
		"getattrlistbulk": {5, sysGetattrlistbulk}, "fsgetpath": {4, sysFsgetpath}, "shm_open": {3, sysShmOpen}, "shm_unlink": {1, sysShmUnlink}, "msync": {3, sysMsync},
		"sem_open": {4, sysSemOpen}, "sem_close": {1, sysSemClose}, "sem_unlink": {1, sysSemUnlink}, "sem_wait": {1, sysSemWait}, "sem_trywait": {1, sysSemTrywait}, "sem_post": {1, sysSemPost},
		"getfsstat64": {3, sysGetfsstat64}, "pathconf": {2, sysPathconf}, "fpathconf": {2, sysPathconf}, "getdirentries64": {4, sysGetdirentries64},
		// memory
		"mmap": {6, sysMmap}, "munmap": {2, sysMunmap}, "mprotect": {3, sysMprotect}, "madvise": {3, sysMadvise},
		"mremap_encrypted": {0, constant(0)}, "shared_region_check_np": {1, sysSharedRegionCheck},
		"shared_region_map_and_slide_2_np": {4, sysSharedRegionMap}, "map_with_linking_np": {4, constant(-eNOTSUP)},
		// process
		"exit": {1, sysExit}, "getpid": {0, sysGetpid}, "getppid": {0, sysGetppid}, "posix_spawn": {5, sysPosixSpawn}, "wait4": {4, sysWait4}, "fork": {0, sysFork}, "__pthread_chdir": {1, sysPthreadChdir}, "__pthread_fchdir": {1, sysPthreadFchdir}, "execve": {3, sysExecve}, "getuid": {0, sysGetuid},
		"geteuid": {0, sysGetuid}, "getgid": {0, sysGetgid}, "getegid": {0, sysGetgid}, "gettid": {2, constant(-eSRCH)},
		"setuid": {1, sysSetuid}, "seteuid": {1, sysSetuid}, "setgid": {1, sysSetgid}, "setegid": {1, sysSetgid},
		"getrusage": {2, sysGetrusage}, "getlogin": {2, sysGetlogin}, "getdtablesize": {0, sysGetdtablesize},
		"issetugid": {0, constant(0)}, "sigaction": {3, sysSigaction}, "sigprocmask": {3, sysSigprocmask},
		"__pthread_sigmask": {3, sysSigprocmask}, "__pthread_kill": {2, sysPthreadKill}, "kill": {3, sysKill},
		"sigaltstack": {2, sysSigaltstack}, "setitimer": {3, sysSetitimer}, "getitimer": {2, sysGetitimer}, "__disable_threadsignal": {1, constant(0)}, "thread_selfid": {0, sysThreadSelfid},
		"bsdthread_register": {6, sysBsdthreadRegister}, "bsdthread_create": {5, sysBsdthreadCreate},
		"bsdthread_terminate": {4, sysBsdthreadTerminate}, "__semwait_signal": {6, sysSemwaitSignal},
		"sigsuspend": {1, sysSigsuspend}, "__sigwait": {2, sysSigwait}, "kevent_qos": {8, sysKeventQos}, "kqueue_workloop_ctl": {4, sysKqueueWorkloopCtl}, "kevent": {6, sysKevent}, "kevent64": {7, sysKevent64}, "kevent_id": {8, sysKeventID}, "psynch_mutexwait": {5, sysPsynchMutexwait}, "psynch_mutexdrop": {5, sysPsynchMutexdrop}, "psynch_cvwait": {8, sysPsynchCvwait},
		"psynch_cvsignal": {8, sysPsynchCvsignal}, "psynch_cvbroad": {7, sysPsynchCvbroad}, "psynch_cvclrprepost": {7, constant(0)},
		"psynch_rw_rdlock": {5, sysPsynchRWRdlock}, "psynch_rw_wrlock": {5, sysPsynchRWWrlock}, "psynch_rw_unlock": {5, sysPsynchRWUnlock}, "bsdthread_ctl": {4, constant(0)}, "workq_open": {0, constant(0)},
		"workq_kernreturn": {4, sysWorkqKernreturn}, "kdebug_trace64": {0, constant(0)}, "kdebug_trace": {0, constant(0)},
		"kdebug_trace_string": {0, constant(0)}, "kdebug_typefilter": {2, constant(-ePERM)}, "getentropy": {2, sysGetentropy},
		"ptrace": {1, sysPtrace}, "select": {5, sysSelect}, "pselect": {5, sysPselect}, "kqueue": {0, sysKqueue}, "guarded_kqueue_np": {2, sysKqueue}, "csops": {4, sysCsops}, "csops_audittoken": {4, sysCsops}, "csrctl": {3, constant(-ePERM)}, "getrlimit": {2, sysGetrlimit},
		"setrlimit": {2, sysSetrlimit}, "gettimeofday": {3, sysGettimeofday}, "__mac_syscall": {3, sysMacSyscall},
		"abort_with_payload": {6, sysAbortWithPayload}, "terminate_with_payload": {6, sysTerminateWithPayload},
		"os_fault_with_payload": {6, sysOSFaultWithPayload}, "ulock_wait": {4, sysUlockWait}, "ulock_wait2": {5, sysUlockWait2},
		"ulock_wake": {3, sysUlockWake}, "proc_rlimit_control": {3, sysProcRlimitControl}, "proc_info_extended_id": {8, sysProcInfoExtendedID}, "initgroups": {3, sysInitgroups}, "memorystatus_control": {5, sysMemorystatusControl}, "proc_info": {6, sysProcInfo}, "getpgrp": {0, sysGetpgrp}, "getsid": {1, sysGetsid}, "task_read_for_pid": {3, sysTaskReadForPID}, "reboot": {2, sysReboot}, "setsid": {0, sysSetsid}, "setpgid": {2, sysSetpgid}, "clonefileat": {5, sysClonefileat}, "fclonefileat": {4, sysFclonefileat}, "getpgid": {1, sysGetpgid}, "sigreturn": {3, sysSigreturn}, "__pthread_canceled": {1, sysPthreadCanceled}, "sigpending": {1, sysSigpending}, "gethostuuid": {2, sysGethostuuid}, "sysctl": {6, sysSysctl}, "sysctlbyname": {6, sysSysctlbyname},
	}
	for name, v := range trivial {
		bsdHandlers[name] = handler{4, constant(v)}
	}
}

// trivial are more calls answered with a constant (negative: errno) and no side effects.
// None answers success: a call that would have done something is implemented, or has no handler
// and stops the run. objc_bp_assist_cfg_np fails as on a
// core without the hardware that assists Objective-C breakpoints, which the emulated one is.
var trivial = map[string]int64{
	"work_interval_ctl": -eNOTSUP, "crossarch_trap": -eNOTSUP, "objc_bp_assist_cfg_np": -eNOTSUP,
	"necp_open":     -eNOSYS,
	"getaudit_addr": -eNOSYS, "coalition_info": -eNOSYS, "persona": -eNOSYS,
}

// call runs a handler; touching unmapped guest memory yields onFault instead of ending the run.
func (p *Process) call(h handler, args []uint64, onFault int64) (result int64) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(fault); !ok {
				panic(r)
			}
			result = onFault
		}
	}()
	return h.fn(p, args)
}

// descriptorArgs are the arguments of each call that are descriptors, as bits (1 for the first).
// A descriptor is an int, and the kernel reads an int's low half only: a program may leave
// anything in the rest of the register, and the shell does.
var descriptorArgs = func() map[string]uint8 {
	m := map[string]uint8{"dup2": 1 | 2, "renameat": 1 | 4, "clonefileat": 1 | 4, "fclonefileat": 1 | 2, "renameatx_np": 1 | 4, "linkat": 1 | 4, "symlinkat": 2, "mmap": 16}
	for _, name := range strings.Fields(`fstat64 fstatat64 openat guarded_close_np guarded_write_np guarded_pwrite_np close read pread write writev readv
		sendto recvfrom shutdown getsockopt setsockopt getsockname getpeername bind listen connect accept sendmsg recvmsg pwrite lseek ftruncate fsync readlinkat mkdirat unlinkat faccessat
		fcntl dup fchdir __pthread_fchdir fchmod fchmodat fchmod_extended fstat64_extended fchownat fchown futimes setattrlistat fsetattrlist
		flock fgetxattr fsetxattr flistxattr fremovexattr ioctl fstatfs64 getattrlistat fgetattrlist getattrlistbulk fpathconf getdirentries64
		kevent kevent64 kevent_qos`) {
		m[name] = 1
	}
	return m
}()

func (p *Process) bsd(n int64) {
	name, ok := bsdNames[n]
	if !ok {
		name = strconv.FormatInt(n, 10)
	}
	args := p.args()
	base := strings.ReplaceAll(name, "_nocancel", "")
	h, ok := bsdHandlers[base]
	if !ok && p.unimplemented(fmt.Sprintf("syscall %d %s", n, name), args) {
		p.setX(0, eNOSYS)
		p.carry(true)
		return
	}
	for i, ints := 0, descriptorArgs[base]; ints != 0; i, ints = i+1, ints>>1 {
		if ints&1 != 0 {
			args[i] = uint64(int64(int32(args[i])))
		}
	}
	p.current.arg0, p.current.sleeping = args[0], name == "__semwait_signal"
	r := p.call(h, args, -eFAULT)
	p.setX(0, uint64(max(r, -r)))
	p.carry(r < 0)
	p.deliver()
	result := fmt.Sprintf("%#x", uint64(r))
	if r < 0 {
		result = fmt.Sprintf("errno %d", -r)
	}
	p.note("%s(%s) = %s", name, hexArgs(args[:h.args]), result)
}

// ---- memory ----

func sysMmap(p *Process, a []uint64) int64 {
	addr, length, flags, fdn, off := a[0], a[1], a[3], a[4], a[5]
	deny := denied(uint32(a[2]))
	fixed := flags&0x10 != 0
	if fixed && addr&tbiMask+length > vaLimit {
		return -eNOMEM
	}
	if flags&0x1000 != 0 { // MAP_ANON
		// What is given for a descriptor is read as how the memory is to be had, if it is not
		// -1: a tag to know it by, and nothing else here; a bit that names nothing is EINVAL,
		// as the source has it and as measured of 99.
		if how := uint32(fdn); int32(how) != -1 {
			const tag, purgeable, chunk, superpage = 0xFF000000, 0x2, 0x4, 0x70000
			switch {
			case how&^(tag|purgeable|chunk|superpage) != 0:
				return -eINVAL
			case how&^tag != 0:
				p.unimplemented("mmap of memory that can be emptied, or of large pages", a)
				return -eNOSYS
			}
		}
		if flags&mapShared != 0 && length > 0 {
			// Memory of no file that is shared: a copy of the process has the same memory and
			// not a copy of it, as measured (tests/kernel/vmregion.c).
			shared := p.m.newShared(align(length, pageSize))
			if shared == nil {
				return -eNOMEM
			}
			return p.mapShared(shared, addr, align(length, pageSize), 0, fixed, deny)
		}
		start, ok := p.alloc(length, allocation{addr: addr, fixed: fixed, name: "mmap", deny: deny})
		if !ok {
			return -eNOMEM
		}
		return int64(start)
	}
	f := p.fds[fdn&0xFFFFFFFF]
	if f == nil {
		return -eBADF
	}
	// What is no file cannot be mapped (EINVAL; of a device ENODEV), and nor can a file opened
	// to write only (EACCES), nor one of the system's be mapped to be shared and written: as
	// measured.
	switch {
	case (f.kind == dirFD || f.kind == hostFD && f.isDirectory()) && flags&mapShared != 0: // of a directory under the home, as measured
		return -eNODEV
	case f.kind == dirFD, f.kind == pipeFD, f.kind == socketFD, f.kind == ttyFD, f.kind == hostFD && f.isDirectory():
		return -eINVAL
	case f.kind == hostFD && f.input() == inputNothing:
		return -eNODEV
	case f.kind == hostFD && f.accessMode() == 1:
		return -eACCES
	case (f.kind == cacheFD || f.kind == dataFD && f.shared == nil) && flags&mapShared != 0 && deny&2 == 0:
		return -eACCES
	}
	if f.kind == semFD || f.kind == dataFD && f.shared != nil && flags&mapShared == 0 { // a semaphore, and shared memory mapped for oneself: as measured
		return -eINVAL
	}
	if f.shared != nil && flags&mapShared != 0 {
		if f.kind == dataFD && f.flags&3 == 0 && deny&2 == 0 { // opened to read only and mapped to write: as measured
			return -ePERM
		}
		r := p.mapShared(f.shared, addr, length, off, fixed, deny)
		if r > 0 && f.kind == dataFD && f.flags&3 == 0 {
			p.findRegion(uint64(r)).readOnly = true
		}
		return r
	}
	if f.kind == hostFD && flags&mapShared != 0 {
		// A file mapped to be shared (see mappedfiles.go). One opened to read only is not
		// mapped to be written (EACCES), and is never made writable after: as measured.
		readOnly := f.accessMode() == 0
		if readOnly && deny&2 == 0 {
			return -eACCES
		}
		mf, errno := p.mappedFor(f)
		if errno != 0 {
			return errno
		}
		p.m.beforeRead(f.host)
		p.m.afterWrite(f.host)
		r := p.mapFile(mf, addr, length, off, fixed, deny)
		if r > 0 && readOnly {
			p.findRegion(uint64(r)).readOnly = true
		}
		return r
	}
	name := "?"
	if f.path != "" {
		name = f.path
	}
	what := allocation{addr: addr, fixed: fixed, kind: fileRegion, name: "mmap:" + name, deny: deny}
	if f.kind == cacheFD { // fetched by range, so only the pages the guest touches
		what.file, what.off = f.cache, off
	} else {
		var errno int64
		if what.data, errno = p.pread(f, off, length); errno != 0 {
			return errno
		}
	}
	start, ok := p.alloc(length, what)
	if !ok {
		return -eNOMEM
	}
	p.findRegion(start).into = off
	return int64(start)
}

// sysMprotect changes what a range of pages may be accessed for. An access they do not allow
// ends the run, as the fault ends a process whose signal nothing handles.
func sysMprotect(p *Process, a []uint64) int64 {
	addr := a[0] & tbiMask
	if addr%pageSize != 0 {
		return -eINVAL
	}
	end := addr + align(a[1], pageSize)
	if a[2]&2 != 0 && slices.ContainsFunc(p.overlaps(addr, end), func(r *region) bool { return r.readOnly }) {
		return -eACCES // shared memory that was opened to read only is not made writable, as measured
	}
	if !p.protect(addr, end, uint32(a[2]&protAll)) {
		return -eNOMEM
	}
	return 0
}

func sysMunmap(p *Process, a []uint64) int64 {
	if a[0]%pageSize != 0 || a[1] == 0 { // as measured
		return -eINVAL
	}
	p.unmap(a[0]&^(pageSize-1), align(a[0]+a[1], pageSize))
	p.m.deferred = append(p.m.deferred, p.m.settleMapped) // once the engine no longer maps it
	return 0
}

func sysSharedRegionCheck(p *Process, a []uint64) int64 {
	if !p.slideSet {
		return -eNOMEM
	}
	p.write(a[0], le64(p.cache.Base+p.slide))
	return 0
}

// sysSharedRegionMap is dyld mapping the cache into the (empty) shared region. Pages stay lazy;
// the kernel's page-in rebasing happens when a page is first touched.
func sysSharedRegionMap(p *Process, a []uint64) int64 {
	nfiles, files, nmappings, mappings := a[0], a[1], a[2], a[3]
	if nfiles > 1024 || nmappings > 4096 {
		return -eINVAL
	}
	var fds []uint64
	for i := uint64(0); i < nfiles; i++ {
		e := p.read(files+i*12, 12)
		for n := le.Uint32(e[4:]); n > 0 && len(fds) < 4096; n-- {
			fds = append(fds, uint64(le.Uint32(e)))
		}
	}
	for i := uint64(0); i < nmappings; i++ {
		e := p.read(mappings+i*48, 48)
		addr, size, fileOff, prot := le.Uint64(e), le.Uint64(e[8:]), le.Uint64(e[16:]), le.Uint32(e[44:])
		var f *fd
		if i < uint64(len(fds)) {
			f = p.fds[fds[i]]
		}
		var backing bool
		var address uint64
		if f != nil && f.kind == cacheFD {
			for _, m := range p.cache.Mappings {
				if m.File == f.cache && m.Offset == fileOff && m.Size == size {
					backing, address = true, m.Address
					break
				}
			}
		}
		kind := "zero-fill"
		if backing {
			kind = "cache"
		}
		p.note("  map %#x size %#x off %#x prot %#x %s", addr, size, fileOff, prot, kind)
		p.unmap(addr, addr+align(size, pageSize))
		if !backing {
			// Not file backed: dyld hands over a buffer (its dynamic config) to copy in.
			r := &region{start: addr, end: addr + align(size, pageSize), name: "shared-region", deny: denied(prot)}
			if f == nil && p.findRegion(fileOff) != nil {
				if r.data = p.read(fileOff, size); len(r.data) > 0 {
					r.kind = fileRegion
				}
			}
			p.addRegion(r)
			continue
		}
		slide := addr - address
		if p.slideSet && p.slide != slide {
			stopf("inconsistent shared region slide")
		}
		p.slide, p.slideSet = slide, true
		p.addRegion(&region{start: addr, end: addr + size, kind: cacheRegion, name: "cache", deny: denied(prot)})
	}
	return 0
}

// ---- process ----

func sysExit(p *Process, a []uint64) int64 {
	code := int(a[0] & 0xFF)
	if f := p.leaderDrains(); f != nil {
		t := p.current
		t.leaving, t.pipeOp = &code, &pipeOp{f: f, closing: true, flags: 1}
		p.waitTerminal(f)
		return 0
	}
	if f := p.leavesTerminal(); f != nil {
		// What it wrote to a terminal that nobody else has open waits to be read before it
		// is gone, and the process with it (see terminals.go).
		t := p.current
		t.leaving, t.pipeOp = &code, &pipeOp{f: f, closing: true}
		p.wait(waitPipe, f.tty.id, f.tty.until, result{})
		return 0
	}
	p.exitCode, p.exited = code, true
	return 0
}

func sysGetpid(p *Process, a []uint64) int64 { return int64(p.pid) }
func sysGetuid(p *Process, a []uint64) int64 { return int64(p.uid) }
func sysGetgid(p *Process, a []uint64) int64 { return int64(p.gid) }

// sysGetgroups is getgroups(room, &groups): the groups a process is in, its own first. A process
// here is in its own and no other: none has been put in more, which is done at a login.
func sysGetgroups(p *Process, a []uint64) int64 {
	room := int32(a[0])
	if room < 0 {
		return -eINVAL
	}
	if room > 0 {
		p.write(a[1], le32(p.gid))
	}
	return 1
}

// sysSetuid is setuid and seteuid, and sysSetgid the same of the group. A process has one user
// and one group here, real, effective and saved alike, so becoming itself changes nothing, and
// becoming another is not permitted to anyone but root. Root becoming another, which would
// leave it with two, is not implemented.
func sysSetuid(p *Process, a []uint64) int64 { return p.become(a, p.uid, "user") }
func sysSetgid(p *Process, a []uint64) int64 { return p.become(a, p.gid, "group") }

func (p *Process) become(a []uint64, own uint32, what string) int64 {
	switch id := uint32(a[0]); {
	case id == own:
		return 0
	case p.uid != 0:
		return -ePERM
	}
	p.unimplemented("root becoming another "+what, a)
	return -ePERM
}

// maxLogName is MAXLOGNAME: how long the kernel keeps a login name, with the zero that ends it.
const maxLogName = 255

// sysGetlogin is getlogin(buffer, size): the name the process's session was logged in under,
// copied with the zeros after it for as much of the kernel's own buffer as there is room for.
// A program run by hand on a Mac is in its user's session. What launchd's own session is called,
// which a daemon and everything on an iPhone are in, is not known here.
func sysGetlogin(p *Process, a []uint64) int64 {
	if p.uid == 0 || p.cache.Platform == dyldcache.IOS {
		p.unimplemented("getlogin of a daemon, or on iOS", a)
		return -eNOENT
	}
	name := make([]byte, maxLogName)
	copy(name, p.m.user.name)
	p.write(a[0], name[:min(a[1], maxLogName)])
	return 0
}

// sysGetdtablesize is how many descriptors the process may have: its soft limit, or the system's
// limit for a process if that is lower. That one (kern.maxfilesperproc) is not known here, and is
// no lower than the most descriptors the kernel was ever built to give a process.
func sysGetdtablesize(p *Process, a []uint64) int64 {
	soft := p.rlimit(rlimitNofile)[0]
	if soft > maxFD {
		p.unimplemented("getdtablesize with a limit above the system's own", a)
		return -eINVAL
	}
	return int64(soft)
}

// hostUUID is the machine's own identifier: the same on every run, as everything else about it is.
var hostUUID = [16]byte{0x1F, 0x5A, 0xE3, 0x70, 0x9C, 0x42, 0x4D, 0x1B, 0x8E, 0x6D, 0x49, 0x50, 0x53, 0x57, 0x52, 0x4E}

// bootSession is what a kernel makes up each time it boots to tell that boot from any other
// (kern.bootsessionuuid): here the one boot there is, the same on every run.
var bootSession = func() string {
	u := sha256.Sum256([]byte("ipswrun boot session"))
	u[6], u[8] = u[6]&0x0F|0x40, u[8]&0x3F|0x80 // a random UUID, as a kernel's is
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", u[:4], u[4:6], u[6:8], u[8:10], u[10:16]))
}()

func sysGethostuuid(p *Process, a []uint64) int64 {
	p.write(a[0], hostUUID[:])
	return 0
}

// Signals are never delivered: the calls that would send one (kill, pthread_kill, a CPU fault, a
// timer) end the run instead. What a process says about signals is kept and read back as a
// kernel does, which is all a process can see of it until one arrives.

const (
	sigKill    = 9
	sigStop    = 17
	numSignals = 32
	ssDisable  = 4     // SS_DISABLE: no alternate signal stack
	minSigStk  = 32768 // MINSIGSTKSZ
	// saUser is SA_USERSPACE_MASK: the flags a process reads back. libc also passes SA_USERTRAMP,
	// which the kernel keeps to itself.
	saUser = 0x7f
)

// sigAction is a signal's disposition, as struct sigaction says it: handler, mask, flags.
type sigAction struct {
	handler uint64
	tramp   uint64 // libc's trampoline, which calls the handler and returns to the kernel
	mask    uint32
	flags   uint32
}

// sysSigaction sets and reads a signal's disposition. The kernel's argument is struct
// __sigaction, which has libc's trampoline after the handler; what it hands back is the plain
// struct sigaction.
func sysSigaction(p *Process, a []uint64) int64 {
	sig, act, old := a[0], a[1], a[2]
	if sig == 0 || sig >= numSignals || act != 0 && (sig == sigKill || sig == sigStop) {
		return -eINVAL
	}
	if old != 0 {
		was := p.sigactions[sig]
		p.write(old, append(le64(was.handler), le32(was.mask, was.flags)...))
	}
	if act != 0 {
		b := p.read(act, 24)
		p.sigactions[sig] = sigAction{handler: le.Uint64(b), tramp: le.Uint64(b[8:]), mask: le.Uint32(b[16:]), flags: le.Uint32(b[20:]) & saUser}
	}
	return 0
}

// sysSigprocmask sets and reads the calling thread's mask of blocked signals: sigprocmask and
// pthread_sigmask alike, since Darwin keeps the mask per thread.
func sysSigprocmask(p *Process, a []uint64) int64 {
	how, set, old := a[0], a[1], a[2]
	t := p.current
	was := t.sigmask
	if set != 0 {
		mask := p.u32(set) &^ (1<<(sigKill-1) | 1<<(sigStop-1))
		switch how {
		case 1: // SIG_BLOCK
			t.sigmask |= mask
		case 2: // SIG_UNBLOCK
			t.sigmask &^= mask
		case 3: // SIG_SETMASK
			t.sigmask = mask
		default:
			return -eINVAL
		}
	}
	if old != 0 {
		p.write(old, le32(was))
	}
	return 0
}

// sysSigaltstack sets and reads the calling thread's alternate signal stack (stack_t).
func sysSigaltstack(p *Process, a []uint64) int64 {
	set, old := a[0], a[1]
	t := p.current
	if old != 0 {
		flags := uint32(ssDisable)
		if t.altstack[1] != 0 {
			flags = 0
		}
		if t.onstack {
			flags = ssOnStack
		}
		p.write(old, append(le64(t.altstack[0], t.altstack[1]), le32(flags, 0)...))
	}
	if set != 0 {
		b := p.read(set, 24)
		switch sp, size, flags := le.Uint64(b), le.Uint64(b[8:]), le.Uint32(b[16:]); {
		case flags&ssDisable != 0:
			t.altstack = [2]uint64{}
		case flags != 0:
			return -eINVAL
		case size < minSigStk:
			return -eNOMEM
		default:
			t.altstack = [2]uint64{sp, size}
		}
	}
	return 0
}

func sysThreadSelfid(p *Process, a []uint64) int64 { return 0x4242 + int64(p.current.id) }

// sysGetentropy is reproducible, but never repeats: corecrypto's health checks reject identical
// blocks. Darwin caps a single call at 256 bytes and answers more with EIO.
func sysGetentropy(p *Process, a []uint64) int64 {
	if a[1] > 256 {
		return -eIO
	}
	var out []byte
	for uint64(len(out)) < a[1] {
		p.entropy++
		sum := sha256.Sum256(fmt.Appendf(nil, "ipswrun entropy %d", p.entropy))
		out = append(out, sum[:]...)
	}
	p.write(a[0], out[:a[1]])
	return 0
}

// sysCsops is what the kernel knows of a process's code signature: its own, or that of another
// process, which is how a daemon decides what a client may ask of it. Nothing here checks a
// signature. What it says is taken at its word, as of code the kernel has found valid.
func sysCsops(p *Process, a []uint64) int64 {
	op, addr, size := a[1], a[2], a[3]
	p.note("  csops pid %d op %d size %d", a[0], op, size)
	about := p
	if a[0] != 0 {
		about = p.m.process(uint32(a[0]))
	}
	if about == nil {
		return -eSRCH
	}
	// blob is how a string or a property list is returned: under a header of a magic number and
	// the whole length, both big-endian. A caller with too little room gets the header, to come
	// back with enough.
	blob := func(magic uint32, data []byte) int64 {
		header := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, magic), uint32(8+len(data)))
		if size < 8 {
			return -eRANGE
		}
		if size < uint64(8+len(data)) {
			p.write(addr, header)
			return -eRANGE
		}
		p.write(addr, append(header, data...))
		return 0
	}
	switch op {
	case csOpsStatus:
		p.write(addr, le32(0x20000005|2))
		return 0
	case csOpsEntitlements:
		if about.entitlements == "" {
			return 0
		}
		return blob(0xFADE7171, []byte(about.entitlements))
	case csOpsEntitlementsDER:
		if about.entitlementsDER == nil {
			return -eNOENT
		}
		return blob(0xFADE7172, about.entitlementsDER)
	case csOpsIdentity:
		if about.identity == "" {
			return -eNOENT
		}
		return blob(0, str(about.identity))
	}
	return -eINVAL // no hash or team to report
}

const (
	csOpsStatus          = 0
	csOpsEntitlements    = 7  // as a property list
	csOpsIdentity        = 11 // the name the code was signed under
	csOpsEntitlementsDER = 16 // as DER, which is what Security reads
)

// selectNothing is select on descriptors nothing ever happens to: none is ready, once the time
// the caller would wait has passed. It never passes for a caller that set none.
func (p *Process) selectNothing(a []uint64, nanoseconds uint64) int64 {
	if r, ok := p.selectPipes(a, nanoseconds); ok {
		return r
	}
	for _, set := range a[1:4] {
		if set != 0 {
			p.write(set, make([]byte, align(a[0], 32)/8))
		}
	}
	if a[4] == 0 {
		return sysSigsuspend(p, a)
	}
	if deadline := p.after(relative(p.u64(a[4]), p.u64(a[4]+8)*nanoseconds)); !deadline.IsZero() {
		p.wait(waitSleep, 0, deadline, result{})
	}
	return 0
}

// The timeout of select is seconds and microseconds, that of pselect seconds and nanoseconds.
func sysSelect(p *Process, a []uint64) int64  { return p.selectNothing(a, 1000) }
func sysPselect(p *Process, a []uint64) int64 { return p.selectNothing(a, 1) }

const ptDenyAttach = 31

// sysPtrace lets a guest refuse a debugger (PT_DENY_ATTACH), which Apple's daemons do before
// anything else: there is none. No other request has a process to act on.
func sysPtrace(p *Process, a []uint64) int64 {
	if a[0] == ptDenyAttach {
		return 0
	}
	return -eINVAL
}

// sysIopolicysys sets and reads back a process's or thread's I/O policies (iopolicy_set and
// _get). A disk policy only changes how soon I/O is done, which nothing here can tell. The
// policies of what a file system shows (access times, modification times, permissions) would
// change what the guest sees, and stop the run when set.
func sysIopolicysys(p *Process, a []uint64) int64 {
	const get, set = 1, 2 // IOPOL_CMD_GET and _SET
	b := p.read(a[1], 12) // struct _iopol_param_t: scope, type, policy
	scope, kind, policy := le.Uint32(b), le.Uint32(b[4:]), le.Uint32(b[8:])
	key := [2]uint32{scope, kind}
	if scope == 1 { // IOPOL_SCOPE_THREAD: the thread's own
		key[0] = 1<<16 | uint32(p.current.port)
	}
	p.note("  iopolicysys cmd %d scope %d type %d policy %d", a[0], scope, kind, policy)
	switch {
	case scope > 2:
		return -eINVAL
	case a[0] == set && (kind == 2 || kind == 7 || kind == 8): // ATIME_UPDATES, IGNORE_PERMISSIONS, SKIP_MTIME_UPDATE
		p.unimplemented("iopolicysys of a policy that changes what files show", a)
		return -eNOSYS
	case a[0] == set:
		if p.iopolicies == nil {
			p.iopolicies = map[[2]uint32]uint32{}
		}
		p.iopolicies[key] = policy
	case a[0] == get:
		p.write(a[1]+8, le32(p.iopolicies[key])) // IOPOL_DEFAULT until set
	default:
		return -eINVAL
	}
	return 0
}

// sysMemorystatusGetLevel is the percentage of memory free, as kern.memorystatus_level says.
func sysMemorystatusGetLevel(p *Process, a []uint64) int64 {
	level, _ := p.sysctlValue("kern.memorystatus_level")
	p.write(a[0], le32(uint32(level.(uint64))))
	return 0
}

// Priorities are a process's nice value, by PRIO_PROCESS, PRIO_PGRP or PRIO_USER of its own pid,
// group (its pid) or user, as getpriority and setpriority keep them. A user may raise its nice
// value but not lower it again. Darwin's own kinds (PRIO_DARWIN_THREAD and the like) stop.
func (p *Process) priorityOf(a []uint64) int64 {
	which, who := a[0], a[1]
	switch {
	case which > 2:
		p.unimplemented("getpriority or setpriority of a Darwin kind", a)
		return -eNOSYS
	case who != 0 && who != uint64(p.pid) && (which != 2 || who != uint64(p.uid)):
		return -eSRCH
	}
	return 0
}

func sysGetpriority(p *Process, a []uint64) int64 {
	if errno := p.priorityOf(a); errno != 0 {
		return errno
	}
	if p.nice < 0 { // a negative result here is an errno: only root can have one
		p.unimplemented("getpriority of a negative nice value", a)
		return -eNOSYS
	}
	return p.nice
}

func sysSetpriority(p *Process, a []uint64) int64 {
	if errno := p.priorityOf(a); errno != 0 {
		return errno
	}
	nice := min(max(int64(int32(a[2])), -20), 20)
	if nice < p.nice && p.uid != 0 {
		return -eACCES
	}
	p.nice = nice
	return 0
}

const (
	rlimInfinity = 1<<63 - 1
	rlimitStack  = 3
	rlimitNofile = 8
	rlimitCount  = 9      // RLIM_NLIMITS
	rlimitPOSIX  = 0x1000 // _RLIMIT_POSIX_FLAG: refuse, rather than clamp, a soft limit too high
)

// rlimit is a resource's {soft, hard} limit: what setrlimit made it, or what a process launchd
// starts on a Mac or an iPhone has. RLIMIT_NPROC's depends on the device's memory and is not
// known here; it is left unlimited, as the emulator starts no processes.
func (p *Process) rlimit(resource uint64) [2]uint64 {
	if l, ok := p.rlimits[resource]; ok {
		return l
	}
	switch resource {
	case 4: // RLIMIT_CORE: no core files
		return [2]uint64{0, rlimInfinity}
	case rlimitStack: // 8 MB and 64 MB, less a guard page
		return [2]uint64{8<<20 - pageSize, 64<<20 - pageSize}
	case rlimitNofile:
		return [2]uint64{256, rlimInfinity}
	}
	return [2]uint64{rlimInfinity, rlimInfinity}
}

func sysGetrlimit(p *Process, a []uint64) int64 {
	resource := a[0] &^ rlimitPOSIX
	if resource >= rlimitCount {
		return -eINVAL
	}
	l := p.rlimit(resource)
	p.write(a[1], le64(l[0], l[1]))
	return 0
}

// sysSetrlimit changes a limit as the kernel lets a process: a soft limit no higher than the
// hard one, and a hard one raised only by root. The descriptors' limit is kept to (see
// fdLimit); the stack's applies to a process yet to start, which none is here.
func sysSetrlimit(p *Process, a []uint64) int64 {
	resource := a[0] &^ rlimitPOSIX
	if resource >= rlimitCount {
		return -eINVAL
	}
	b := p.read(a[1], 16)
	soft, hard := le.Uint64(b), le.Uint64(b[8:])
	if soft > hard {
		return -eINVAL
	}
	if hard > p.rlimit(resource)[1] && p.uid != 0 {
		return -ePERM
	}
	if p.rlimits == nil {
		p.rlimits = map[uint64][2]uint64{}
	}
	p.rlimits[resource] = [2]uint64{soft, hard}
	return 0
}

func sysGettimeofday(p *Process, a []uint64) int64 {
	now := p.m.clock.now()
	if a[0] != 0 {
		p.write(a[0], le64(uint64(now.Unix()), uint64(now.Nanosecond()/1000)))
	}
	if a[2] != 0 {
		p.write(a[2], le64(p.m.clock.ticks()))
	}
	return 0
}

// sysMacSyscall answers the __mac_syscall calls guests make as a Mac's kernel answers them for a
// process that is not sandboxed, as tests/kernel/macsyscall.c measures: AMFI's dyld policy, sandbox
// checks, which allow everything, the sandbox's queries of a process, which find nothing and
// expect no container, and quarantine, which it takes. Applying a sandbox profile succeeds and
// confines nothing, by choice (see limits.md). Anything not measured stops.
func sysMacSyscall(p *Process, a []uint64) int64 {
	name := p.cstr(a[0]) + " " + strconv.Itoa(int(uint32(a[1]))) // the policy and its call
	p.note("  mac_syscall %s", name)
	switch name {
	case "AMFI 90": // amfi_check_dyld_policy_self: {flags in, &flags out}
		p.write(p.u64(a[2]+8), le64(0x1df))
		return 0
	case "Sandbox 0": // applying a compiled profile: succeeds, confines nothing, by choice
		return 0
	case "Sandbox 2":
		return p.sandboxCheck(a)
	case "Sandbox 4":
		return p.sandboxQuery(a)
	case "Sandbox 12":
		return p.sandboxPassthrough(a)
	case "Sandbox 1": // putting on a profile the kernel has by name, {its name, ...}: as one that was compiled, it confines nothing
		name := p.cstr(p.u64(a[2]))
		p.note("  the profile %q", name)
		if !p.kernelHasProfile(name, a) {
			p.unimplemented("putting on a sandbox profile by a name the kernel has none for", a)
			return -eNOENT
		}
		return 0
	case "Sandbox 22": // a profile the kernel has by name; of any other ENOENT, and libsandbox compiles it from its file
		if p.kernelHasProfile(p.cstr(p.u64(a[2])), a) {
			return 0
		}
		return -eNOENT
	case "Sandbox 261":
		return p.sandboxStorageClass(a, true)
	case "Sandbox 264":
		return p.sandboxStorageClass(a, false)
	case "Sandbox 67": // sandbox_get_container_expected: {&expected, &second}
		p.write(p.u64(a[2]), le64(0))
		p.write(p.u64(a[2]+8), le64(0))
		return 0
	case "Quarantine 87": // _qtn_proc_apply_to_pid: taken, nothing written
		return 0
	case "Quarantine 82":
		return p.quarantineOf(a, false)
	case "Quarantine 80":
		return p.quarantineOf(a, true)
	}
	if a[2] != 0 && p.trace { // what it was given, for whoever measures it on a Mac
		p.note("  mac_syscall %s is given %s", name, hexArgs(func() []uint64 {
			b, w := p.read(a[2], 64), make([]uint64, 8)
			for i := range w {
				w[i] = le.Uint64(b[8*i:])
			}
			return w
		}()))
	}
	p.unimplemented("__mac_syscall "+name, a)
	return -eNOSYS
}

// quarantineOf is Quarantine 82, {a descriptor, &room, a buffer}: what a file is in quarantine
// by, as copyfile asks of a file it copies. That is the file's attribute com.apple.quarantine
// after "q/", with room set to how long it is; a file that has none is not in quarantine
// (ENOATTR), and with too little room nothing is written (ERANGE). Of what is no file it is
// EINVAL.
//
// Quarantine 80 asks the same of a path, {a path, &room, a buffer}, as tar asks of what it
// archives: of the file itself, not of what a symbolic link leads to.
func (p *Process) quarantineOf(a []uint64, byPath bool) int64 {
	args := p.read(a[2], 24)
	room, buffer := le.Uint64(args[8:]), le.Uint64(args[16:])
	var (
		id    hostFile
		errno int64
	)
	if byPath {
		if le.Uint64(args) == 0 {
			return -eFAULT
		}
		if _, id, errno = p.changeable(p.cstr(le.Uint64(args)), false); errno == -eNOENT {
			return errno
		}
	} else {
		f := p.fds[le.Uint64(args)]
		switch {
		case f == nil:
			return -eBADF
		case f.streams():
			return -eINVAL
		}
		id, errno = p.xattrFile(0, int64(le.Uint64(args)), 0)
	}
	value, ok := p.m.xattrs[id]["com.apple.quarantine"]
	if errno != 0 || !ok { // nothing that is not the guest's to change has attributes
		return -eNOATTR
	}
	label := append([]byte("q/"), value...)
	if p.u64(room) < uint64(len(label)) {
		return -eRANGE
	}
	p.write(buffer, label)
	p.write(room, le64(uint64(len(label))))
	return 0
}

// sandboxCheck is Sandbox 2, {&result, pid, operation, filter type, argument, flags}, of any
// process, even one that is not there: allowed, with a result of 0, for the filter types the
// guests use (none, 5, 6, 0x1c, 0x41). A path (type 1) that is not there has a result of 2,
// which sandbox_check reports as denied even where there is no sandbox. A descriptor (type 0xf0)
// is EBADF, and the result left as it was, if it is not open. Filter types not measured stop.
func (p *Process) sandboxCheck(a []uint64) int64 {
	w := p.read(a[2], 48)
	pid, kind, arg, flags := le.Uint64(w[8:]), le.Uint64(w[24:]), le.Uint64(w[32:]), le.Uint64(w[40:])
	result := uint64(0)
	switch {
	case kind == 0xf0 && p.fds[arg&0xFFFFFFFF] == nil:
		return -eBADF
	case kind == 1:
		if p.doAccess(p.cstr(arg)) != 0 {
			result = 2
		}
	case kind == 0 && le.Uint64(w[16:]) != 0:
		// An operation by its name, of anything: no process here is confined, and a Mac's kernel
		// allows one that is not confined every operation it has (measured of the four named
		// here, with and without the flag not to report), and says of a name it has no
		// operation for that it is no argument (EINVAL). Its list of names is the sandbox
		// extension's and in no source, so any other name stops.
		switch name := p.cstr(le.Uint64(w[16:])); {
		case slices.Contains([]string{"file-read-data", "mach-lookup", "network-outbound", "system-socket"}, name):
		case name == "no-such-operation":
			return -eINVAL
		default:
			p.unimplemented("sandbox check of the operation "+name+", which was not measured to be one", a)
			return -eNOSYS
		}
	case kind == 0, kind == 5, kind == 6, kind == 0x1c, kind == 0x41, kind == 0xf0:
	default:
		p.unimplemented(fmt.Sprintf("sandbox check of pid %d, filter type %#x, flags %#x", pid, kind, flags), a)
		return -eNOSYS
	}
	p.write(le.Uint64(w), le64(result))
	return 0
}

// sandboxPassthrough is Sandbox 12, sandbox_passthrough_access(descriptor, flags), as
// CFPreferences calls it for a plist it opened: allowed for a regular file with the flags 0x2 and
// 0x100 or neither, EACCES for one with 0x1 and for a directory, EBADF if the descriptor is not
// open. Other descriptors and flags stop.
func (p *Process) sandboxPassthrough(a []uint64) int64 {
	w := p.read(a[2], 16)
	n, flags := le.Uint64(w), le.Uint64(w[8:])
	f := p.fds[n&0xFFFFFFFF]
	if f == nil {
		return -eBADF
	}
	var kind uint16 // as fstat shows it, of the descriptors measured
	if f.kind == cacheFD || f.kind == dirFD || f.kind == hostFD {
		if info, errno := p.describeFD(f); errno == 0 {
			kind = info.mode & syscall.S_IFMT
		}
	}
	regular, dir := kind == syscall.S_IFREG, kind == syscall.S_IFDIR
	switch {
	case dir && flags == 0x102, regular && flags == 1:
		return -eACCES
	case regular && flags&^0x102 == 0:
		return 0
	}
	p.unimplemented(fmt.Sprintf("sandbox passthrough access of descriptor %d, flags %#x", n, flags), a)
	return -eNOSYS
}

// sandboxQuery is Sandbox 4, {pid, a version, buffer, size, how, ...}: a process's container,
// which is not supported where there is no sandbox (ENOTSUP), as measured on macOS 27.2.1. Told
// to (the first bit of how), the process must be the one of that version, as one known by its
// audit token is asked of, and is otherwise none (ESRCH); not told to, the version is not
// looked at. Of launchd a user is refused (EPERM), and of no process it is ESRCH. The buffer
// is not written. Of the kernel, pid 0, and of a process that has ended, it stops.
func (p *Process) sandboxQuery(a []uint64) int64 {
	w := p.read(a[2], 40)
	pid, version, exact := le.Uint64(w), le.Uint64(w[8:]), le.Uint64(w[32:])&1 != 0
	var is lineage
	about := p.m.process(uint32(pid))
	switch {
	case pid == 0 || pid>>32 != 0 || about == nil && pid != launchdPID && p.m.ended(uint32(pid)) != nil:
		p.unimplemented(fmt.Sprintf("sandbox query of pid %d", pid), a)
		return -eNOSYS
	case about != nil:
		is = about.lineage
	case pid == launchdPID:
		is = launchdLineage
	default:
		return -eSRCH
	}
	switch {
	case exact && version != uint64(is.version):
		return -eSRCH
	case about == nil && p.uid != 0: // launchd
		return -ePERM
	case about == nil:
		p.unimplemented("sandbox query of launchd, by root", a)
		return -eNOSYS
	}
	return -eNOTSUP
}

// fixed is a string in a field of n bytes, cut short if it has to be to end in a zero.
func fixed(s string, n int) []byte {
	out := make([]byte, n)
	copy(out[:n-1], s)
	return out
}

// sysReboot is reboot(how, message). Nothing here restarts the machine, so it stops the run as
// unimplemented. A daemon that gives up asks the kernel to panic (RB_PANIC) and says why, which is
// said with the stop: a message that cannot be read is no reason not to stop.
func sysReboot(p *Process, a []uint64) int64 {
	const panics = 0x800 // RB_PANIC
	what := "reboot"
	if a[0]&panics != 0 {
		func() {
			defer func() {
				if _, bad := recover().(fault); bad {
					what += ", asked to panic with a message that cannot be read"
				}
			}()
			if why := p.reason(a[1]); why != "" {
				what += fmt.Sprintf(", asked to panic: %q", why)
			}
		}()
	}
	p.unimplemented(what, a)
	return -eNOSYS
}

func (p *Process) reason(addr uint64) string {
	if addr == 0 {
		return ""
	}
	return p.cstr(addr)
}

func sysAbortWithPayload(p *Process, a []uint64) int64 {
	stopf("abort_with_payload ns=%d code=%#x: %s", a[0], a[1], p.reason(a[4]))
	return 0
}

func sysTerminateWithPayload(p *Process, a []uint64) int64 {
	stopf("terminate_with_payload: %s", p.reason(a[5]))
	return 0
}

func sysOSFaultWithPayload(p *Process, a []uint64) int64 {
	p.note("  os_fault: %s", p.reason(a[4]))
	return 0
}

func sysProcInfo(p *Process, a []uint64) int64 {
	callnum, flavor, buf, size := a[0], a[2], a[4], a[5]
	p.note("  proc_info call %d flavor %d size %d", callnum, flavor, size)
	if callnum == 15 { // PROC_INFO_CALL_SET_DYLD_IMAGES
		return 0
	}
	// Everything else is asked about a process, which may be another: a daemon asks about its
	// client. One that has ended is not there to ask of, waited for or not. launchd is, and being
	// root's says less of itself to a user: what it says in short, and where its program is.
	// That (call 2, PROC_INFO_CALL_PIDINFO) and what a process sets of itself (5) and of its
	// work in hand (8, see procctl.go) are answered: any other call stops and names itself,
	// whatever its second argument is, which is not always a pid.
	switch callnum {
	case 1:
		return p.listPIDs(uint32(a[1]), uint32(a[2]), buf, size, a)
	case 2: // answered below
	case 5:
		return p.procSetControl(uint32(a[1]), flavor, buf, size, a)
	case 8:
		return p.procDirtyControl(uint32(a[1]), flavor, a[3], a)
	default:
		p.unimplemented(fmt.Sprintf("proc_info call %d, flavor %d", callnum, flavor), a)
		return -eNOSYS
	}
	pid := uint32(a[1])
	if pid == 0 && p.mustBe != nil {
		p.unimplemented("proc_info_extended_id of the kernel, pid 0", a)
		return -eNOSYS
	}
	if pid == launchdPID {
		if p.mustBe.another(launchdLineage) {
			return -eSRCH
		}
		return p.launchdInfo(flavor, buf, size, a)
	}
	if pid == 0 && flavor == pidUniqueInfo && size >= 56 { // the kernel, of which this much is said: nothing
		p.write(buf, make([]byte, 56))
		return 56
	}
	about := p.m.process(pid)
	if about == nil || p.mustBe.another(about.lineage) {
		return -eSRCH
	}
	if (flavor == pidUniqueInfo || flavor == pidBSDInfoUnique) && about.uuid == [16]byte{} {
		p.unimplemented("proc_pidinfo's unique record of a program with no UUID", a)
		return -eNOSYS
	}
	if flavor == pidListFDs {
		return p.listFDs(about, buf, size, a)
	}
	{ // a flavour with too little room for it falls to the stop below, but for those measured
		switch r := about.record(); {
		case flavor == pidBSDInfo && size >= 136:
			p.write(buf, r.bsdInfo())
			return 136
		case flavor == pidShortBSDInfo && size >= 64:
			p.write(buf, r.shortInfo(r.infoFlags()))
			return 64
		case flavor == pidPathInfo:
			return p.pathInfo(about.path, buf, size)
		case flavor == pidUniqueInfo && size >= 56:
			p.write(buf, about.uniqueInfo())
			return 56
		case flavor == pidBSDInfoUnique && size < 192:
			return -eNOMEM
		case flavor == pidBSDInfoUnique:
			p.write(buf, append(r.bsdInfo(), about.uniqueInfo()...))
			return 192
		case flavor == pidArchInfo && size >= 8: // the CPU's type and its kind
			p.write(buf, le32(cpuARM64, cpuSubtypeARM64E))
			return 8
		case flavor == pidRegionPath:
			return p.regionPath(about, a[3], buf, size)
		}
	}
	// The process is there, and what was asked of it is not answered here.
	p.unimplemented(fmt.Sprintf("proc_pidinfo flavor %d, of a buffer of %d bytes", flavor, size), a)
	return -eNOSYS
}

// The flavours of proc_pidinfo that are answered.
const (
	pidListFDs       = 1  // PROC_PIDLISTFDS: its descriptors (see procs.go)
	pidBSDInfo       = 3  // PROC_PIDTBSDINFO: struct proc_bsdinfo
	pidPathInfo      = 11 // PROC_PIDPATHINFO: where its program is
	pidShortBSDInfo  = 13 // PROC_PIDT_SHORTBSDINFO: struct proc_bsdshortinfo
	pidUniqueInfo    = 17 // PROC_PIDUNIQIDENTIFIERINFO
	pidBSDInfoUnique = 18 // PROC_PIDBSDINFOWITHUNIQID: the full record, then the unique one
	pidArchInfo      = 19 // PROC_PIDARCHINFO
	pidRegionPath    = 31 // PROC_PIDREGIONPATH: {address, length, path}

	procInfoLP64 = 0x10       // PROC_FLAG_LP64
	procInfoExec = 0x4000     // PROC_FLAG_EXEC: it was started as, or has become, a program
	noTerminal   = 0xFFFFFFFF // of a process's terminal: none
	eOVERFLOW    = 84
)

// bsdInfo is a struct proc_bsdinfo: flags, state, exit status, pid, parent; the user and group
// three times over; the name twice; how many files, the group, no terminal, how nice; and when
// it started.
func (r procRecord) bsdInfo() []byte {
	info := le32(r.infoFlags(), uint32(r.state), 0, r.pid, r.parent, r.uid, r.gid, r.uid, r.gid, r.uid, r.gid, 0)
	info = append(append(info, fixed(r.name, 16)...), fixed(r.name, 32)...)
	info = append(info, le32(uint32(r.files), r.group, 0, noTerminal, 0, uint32(int32(r.nice)))...)
	return append(info, le64(uint64(r.began.Unix()), uint64(r.began.Nanosecond()/1000))...)
}

// infoFlags are a process's flags as proc_pidinfo has them.
func (r procRecord) infoFlags() uint32 {
	if r.execed {
		return procInfoLP64 | procInfoExec
	}
	return procInfoLP64
}

// shortInfo is a struct proc_bsdshortinfo: pid, parent, group, state, the name, flags, and the
// user and group three times over.
func (r procRecord) shortInfo(flags uint32) []byte {
	info := append(le32(r.pid, r.parent, r.group, uint32(r.state)), fixed(r.name, 16)...)
	return append(info, le32(flags, r.uid, r.gid, r.uid, r.gid, r.uid, r.gid, 0)...)
}

const launchdPath = "/sbin/launchd"

// launchdInfo is proc_pidinfo of launchd, as a Mac's answers a user (tests/kernel/procinfo.c): the
// short record, the unique one and the path of its program; the full record, with or without
// the unique one, and its architecture are EPERM. Root is given those too, as measured in a
// virtual machine (macOS 27.2, 26B123): the full record says it leads its session and has the
// flags below, no parent and no terminal. How many files it has open is not known here, launchd
// being no process of the machine's, and is said as none (a Mac's had 128). The other flavours
// were not measured.
func (p *Process) launchdInfo(flavor, buf, size uint64, a []uint64) int64 {
	// What a Mac's launchd has set beside being a 64-bit program that was started as one: how
	// the system treats it, in bits the kernel's public headers do not name.
	const flags, leader = 0xd0000000, 0x20
	record := p.m.launchdRecord()
	full := func() []byte {
		info := record.bsdInfo()
		le.PutUint32(info, record.infoFlags()|flags|leader)
		return info
	}
	unique := func() ([]byte, bool) {
		// The first of the machine's processes, with the UUID of the release's own launchd.
		if p.m.launchdUUID == nil {
			program, errno := p.fileToRun(launchdPath)
			if errno != 0 {
				p.unimplemented("proc_pidinfo of launchd's UUID, with no launchd in the release to read it from", a)
				return nil, false
			}
			uuid, err := uuidOf(program)
			if err != nil {
				p.unimplemented("proc_pidinfo of launchd's UUID, which its file does not give: "+err.Error(), a)
				return nil, false
			}
			p.m.launchdUUID = &uuid
		}
		of := launchdLineage
		of.uuid = *p.m.launchdUUID
		return of.uniqueInfo(), true
	}
	switch {
	case flavor == pidShortBSDInfo && size >= 64:
		p.write(buf, record.shortInfo(record.infoFlags()|flags))
		return 64
	case flavor == pidPathInfo:
		return p.pathInfo(launchdPath, buf, size)
	case flavor == pidUniqueInfo && size >= 56:
		info, ok := unique()
		if !ok {
			return -eNOSYS
		}
		p.write(buf, info)
		return 56
	case (flavor == pidBSDInfo || flavor == pidBSDInfoUnique || flavor == pidArchInfo || flavor == pidListFDs) && p.uid != 0:
		return -ePERM
	case flavor == pidBSDInfo && size >= 136:
		p.write(buf, full())
		return 136
	case flavor == pidBSDInfoUnique && size >= 192:
		info, ok := unique()
		if !ok {
			return -eNOSYS
		}
		p.write(buf, append(full(), info...))
		return 192
	case flavor == pidArchInfo && size >= 8:
		p.write(buf, le32(cpuARM64, cpuSubtypeARM64E))
		return 8
	}
	p.unimplemented(fmt.Sprintf("proc_pidinfo of launchd, flavor %d", flavor), a)
	return -eNOSYS
}

// pathInfo is PROC_PIDPATHINFO: where a process's program is, into room for it and its closing
// zero (ENOMEM) and for no more than 4096 bytes (EOVERFLOW), as a Mac's kernel was measured to
// say; 0, whatever the path's length.
func (p *Process) pathInfo(path string, buf, size uint64) int64 {
	switch {
	case size > 4096:
		return -eOVERFLOW
	case size < uint64(len(path))+1:
		return -eNOMEM
	}
	p.write(buf, str(path))
	return 0
}

// regionPath is proc_pidinfo's PROC_PIDREGIONPATH, which an older dyld asks of where it is itself:
// the region of a process's memory at an address, or the first above it, where it begins, how
// long it is, and the file it is of. One that is of no file (the stack, memory of its own, the
// shared cache) is EINVAL, and too little room ENOMEM, as measured on macOS 27.2.1.
func (p *Process) regionPath(about *Process, addr, buf, size uint64) int64 {
	const length = 1040
	if size < length {
		return -eNOMEM
	}
	at := about.regionFrom(addr)
	if at == nil {
		return -eINVAL
	}
	image, rest, _ := strings.Cut(at.name, ":") // what it was mapped as says whose it is
	var path string
	switch {
	case at.kind == cacheRegion, at.name == "stack", at.name == "mmap": // of no file, as measured: the shared cache, the stack, memory of its own
		return -eINVAL
	case at.kind == fileRegion && image == "dyld":
		path = guestDyld
	case at.kind == fileRegion && image == "guest": // the process's own program
		path = about.path
	case at.kind == fileRegion && image == "mmap" && strings.HasPrefix(rest, "/"):
		path = rest
	default:
		p.unimplemented("proc_pidinfo of the file a region of memory is of, for "+at.name, nil)
		return -eNOSYS
	}
	p.write(buf, append(le64(at.start, at.end-at.start), fixed(path, 1024)...))
	return length
}

// ---- sysctl ----

var sysctlMIBs = map[[2]int32]string{
	{1, 1}: "kern.ostype", {1, 2}: "kern.osrelease", {1, 4}: "kern.version", {1, 8}: "kern.argmax", {1, 10}: "kern.hostname", {1, 11}: "kern.hostid",
	{1, 65}: "kern.osversion", {1, 21}: "kern.boottime", {1, 59}: "kern.usrstack64", {1, 66}: "kern.safeboot", {1, 24}: "kern.ngroups",
	{6, 1}: "hw.machine", {6, 2}: "hw.model", {6, 3}: "hw.ncpu", {6, 7}: "hw.pagesize", {6, 24}: "hw.memsize",
	{6, 25}: "hw.activecpu", {6, 5}: "hw.physmem", {6, 4}: "hw.byteorder",
}

func sysSysctl(p *Process, a []uint64) int64 {
	name, namelen, oldp, oldlenp, newp, newlen := a[0], a[1], a[2], a[3], a[4], a[5]
	if namelen > 64 {
		return -eINVAL
	}
	raw := p.read(name, 4*namelen)
	mib := make([]int32, namelen)
	for i := range mib {
		mib[i] = int32(le.Uint32(raw[i*4:]))
	}
	if len(mib) >= 2 && mib[0] == 1 && mib[1] == 14 { // CTL_KERN, KERN_PROC: the processes there are
		return p.sysctlProcs(mib[2:], oldp, oldlenp, a)
	}
	if len(mib) >= 2 && mib[0] == 1 && mib[1] == 49 { // KERN_PROCARGS2: what a process was started with
		return p.sysctlProcArgs(mib[2:], oldp, oldlenp, a)
	}
	if len(mib) >= 2 && mib[0] == 1 && mib[1] == 38 { // KERN_PROCARGS, the same without the count
		p.unimplemented("sysctl kern.procargs, the arguments a process was started with", a)
		return -eNOSYS
	}
	if len(mib) >= 2 && mib[0] == 0 && mib[1] == 3 { // name2oid
		s := strings.TrimRight(string(p.read(newp, newlen)), "\x00")
		if _, ok := p.sysctlValue(s); !ok {
			p.note("  sysctl name2oid ENOENT %s", s)
			return -eNOENT
		}
		index := slices.Index(p.oids, s)
		if index < 0 {
			index = len(p.oids)
			p.oids = append(p.oids, s)
		}
		p.write(oldp, le32(0x7FFF, uint32(index)))
		p.write(oldlenp, le64(8))
		return 0
	}
	asked := mib
	// What the rest names is called (1) or is (4), not its value: the sysctl tool asks both.
	about := int32(-1)
	if len(mib) >= 2 && mib[0] == 0 && (mib[1] == 1 || mib[1] == 4) {
		about, mib = mib[1], mib[2:]
	}
	var s string
	switch {
	case len(mib) >= 2 && mib[0] == 0x7FFF && int(mib[1]) >= 0 && int(mib[1]) < len(p.oids):
		s = p.oids[mib[1]]
	case about != 1 && len(mib) >= 2 && sysctlMIBs[[2]int32{mib[0], mib[1]}] != "":
		// By its numbers a name is asked only of one that was given by name: the numbers in
		// sysctlMIBs are the old ones, some of which a Mac now calls something else.
		s = sysctlMIBs[[2]int32{mib[0], mib[1]}]
	default:
		p.note("  sysctl mib ENOENT %v", asked)
		return -eNOENT
	}
	switch about {
	case 1:
		var pieces [][]byte
		for i, part := range strings.Split(s, ".") {
			if i > 0 {
				pieces = append(pieces, []byte("."))
			}
			pieces = append(pieces, []byte(part))
		}
		return p.sysctlPieces(oldp, oldlenp, append(pieces, []byte{0})...)
	case 4:
		f, known := sysctlFormats[s]
		if _, ok := p.sysctlValue(s); !ok || !known {
			p.note("  sysctl format ENOENT %s", s)
			return -eNOENT
		}
		return p.sysctlPieces(oldp, oldlenp, le32(f.kind), str(f.format))
	}
	return p.sysctlOut(s, oldp, oldlenp)
}

// sysctlPieces writes an answer the kernel makes piece by piece: each piece whole or not at all,
// and ENOMEM at the first that does not fit, with the length of those before it. With no buffer
// it is the whole length.
func (p *Process) sysctlPieces(oldp, oldlenp uint64, pieces ...[]byte) int64 {
	if oldlenp == 0 {
		return 0
	}
	room, n := p.u64(oldlenp), uint64(0)
	for _, piece := range pieces {
		if oldp != 0 {
			if n+uint64(len(piece)) > room {
				p.write(oldlenp, le64(n))
				return -eNOMEM
			}
			p.write(oldp+n, piece)
		}
		n += uint64(len(piece))
	}
	p.write(oldlenp, le64(n))
	return 0
}

// sysctlFormats is what a Mac's kernel says each of these is (sysctl {0, 4, its numbers}, which
// the sysctl tool asks before it prints one): its kind, whose low bits are the type of its
// value, and how to show it. Measured on macOS 27.2.1 for the names answered here; a name's
// type is what programs are built against, and is taken to be every release's.
var sysctlFormats = map[string]struct {
	kind   uint32
	format string
}{
	"hw.activecpu":                     {0x81e00002, "I"},
	"hw.byteorder":                     {0x81e00002, "I"},
	"hw.cachelinesize":                 {0x81e00004, "Q"},
	"hw.cpu64bit_capable":              {0x81e00002, "I"},
	"hw.cpufamily":                     {0x81e00002, "I"},
	"hw.cpusubtype":                    {0x81e00002, "I"},
	"hw.cputype":                       {0x81e00002, "I"},
	"hw.l1dcachesize":                  {0x81e00004, "Q"},
	"hw.l1icachesize":                  {0x81e00004, "Q"},
	"hw.l2cachesize":                   {0x81e00004, "Q"},
	"hw.logicalcpu":                    {0x81e00002, "I"},
	"hw.logicalcpu_max":                {0x81e00002, "I"},
	"hw.machine":                       {0x84e00003, "A"},
	"hw.memsize":                       {0x81e00004, "Q"},
	"hw.model":                         {0x84e00003, "A"},
	"hw.ncpu":                          {0x81e00002, "I"},
	"hw.nperflevels":                   {0x81e00002, "I"},
	"hw.optional.arm.AdvSIMD":          {0x81e00002, "I"},
	"hw.optional.arm.FEAT_CRC32":       {0x81e00002, "I"},
	"hw.optional.arm.FEAT_LSE":         {0x81e00002, "I"},
	"hw.optional.arm64":                {0x81e00002, "I"},
	"hw.optional.armv8_1_atomics":      {0x81e00002, "I"},
	"hw.optional.armv8_crc32":          {0x81e00002, "I"},
	"hw.optional.floatingpoint":        {0x81e00002, "I"},
	"hw.optional.neon":                 {0x81e00002, "I"},
	"hw.packages":                      {0x81e00002, "I"},
	"hw.pagesize":                      {0x81e00004, "Q"},
	"hw.pagesize32":                    {0x81e00004, "Q"},
	"hw.perflevel0.logicalcpu":         {0x81e00002, "I"},
	"hw.perflevel0.logicalcpu_max":     {0x81e00002, "I"},
	"hw.perflevel0.physicalcpu":        {0x81e00002, "I"},
	"hw.physicalcpu":                   {0x81e00002, "I"},
	"hw.physicalcpu_max":               {0x81e00002, "I"},
	"hw.physmem":                       {0x84e00002, "IU"},
	"hw.targettype":                    {0x81e00003, "A"},
	"kern.argmax":                      {0x81e00002, "I"},
	"kern.hostid":                      {0xc1e00002, "I"},
	"kern.wq_max_threads":              {0xc0e00002, "I"},
	"kern.bootargs":                    {0x81e00003, "A"},
	"kern.bootsessionuuid":             {0x80e00003, "A"},
	"kern.boottime":                    {0x81e00005, "S,timeval"},
	"kern.development":                 {0x84600002, "I"},
	"kern.hostname":                    {0xc0e00003, "A"},
	"kern.hv_vmm_present":              {0x11e00002, "I"},
	"kern.memorystatus_level":          {0x80e00002, "I"},
	"kern.ngroups":                     {0x81e00002, "I"},
	"kern.osproductversion":            {0xc1e00003, "A"},
	"kern.osrelease":                   {0x81e00003, "A"},
	"kern.ostype":                      {0x81e00003, "A"},
	"kern.osvariant_status":            {0xc4e00004, "Q"},
	"kern.osversion":                   {0xc1e00003, "A"},
	"kern.safeboot":                    {0x80e00002, "I"},
	"kern.secure_kernel":               {0x80e00002, "I"},
	"kern.usrstack64":                  {0x80e00004, "Q"},
	"kern.version":                     {0x81e00003, "A"},
	"kern.willshutdown":                {0xc1e00002, "I"},
	"security.mac.lockdown_mode_state": {0x80e00002, "I"},
	"sysctl.proc_translated":           {0x80e00002, "I"},
	"vm.pagesize":                      {0x80e00002, "I"},
	"vm.vm_page_free_target":           {0x80e00002, "I"},
}

func sysSysctlbyname(p *Process, a []uint64) int64 {
	return p.sysctlOut(strings.TrimRight(string(p.read(a[0], a[1])), "\x00"), a[2], a[3])
}

// kern.osvariant_status is what libsystem_darwin's os_variant knows of the system without looking:
// for each question it asks (is there internal content, can a debugger attach, ...), two bits, 2
// for no and 3 for yes, or 0 for one it is to find the answer to itself. The Mac's is what an
// Apple silicon Mac running a release build says. iOS stops ("os_variant had unexpected status")
// at a question the kernel left open, so it is told no for each the Mac leaves open.
const (
	osVariantMac = 0x70010000F388828A
	osVariantIOS = 0x70010000FBAAAAAA
)

// cpuFamilyFirestorm is CPUFAMILY_ARM_FIRESTORM_ICESTORM, of the A14 and of every M1.
const cpuFamilyFirestorm = 0x1b588bb3

// cpuFamilies is the CPUFAMILY_ARM_* of mach/machine.h of the chip in each iPhone and iPad an iOS
// release of 14 or later runs on, and in the Macs with an M1, by the product type's generation
// (iPhone14) or, where one generation has two chips, by the product type itself. A product it
// does not know has no hw.cpufamily, rather than another chip's.
var cpuFamilies = map[string]uint64{
	"Macmini9":     cpuFamilyFirestorm, // M1
	"MacBookAir10": cpuFamilyFirestorm,
	"MacBookPro17": cpuFamilyFirestorm,
	"iMac21":       cpuFamilyFirestorm,
	"MacBookPro18": cpuFamilyFirestorm, // M1 Pro and M1 Max
	"Mac13":        cpuFamilyFirestorm, // M1 Max and M1 Ultra (Mac Studio)
	"iPhone11":     0x07d34b9f,         // A12 (Vortex and Tempest)
	"iPhone12":     0x462504d2,         // A13 (Lightning and Thunder)
	"iPhone13":     cpuFamilyFirestorm, // A14 (Firestorm and Icestorm)
	"iPhone14":     0xda33d83d,         // A15 (Avalanche and Blizzard)
	"iPhone15":     0x8765edea,         // A16 (Everest and Sawtooth)
	"iPhone16":     0x2876f5b5,         // A17 Pro (Coll)
	"iPhone17":     0x204526d0,         // A18 (Tupai)
	"iPhone17,1":   0x75d4acb9,         // A18 Pro (Tahiti)
	"iPhone17,2":   0x75d4acb9,
	"iPad8":        0x07d34b9f,         // A12X and A12Z
	"iPad11":       0x07d34b9f,         // A12
	"iPad12":       0x462504d2,         // A13
	"iPad13":       cpuFamilyFirestorm, // A14 and M1
	"iPad14":       0xda33d83d,         // A15 and M2
	"iPad15":       0xfa33415e,         // M3 (Ibiza)
	"iPad15,7":     0x8765edea,         // A16
	"iPad15,8":     0x8765edea,
	"iPad16":       0x6f5129ac, // M4 (Donan)
	"iPad16,1":     0x2876f5b5, // A17 Pro
	"iPad16,2":     0x2876f5b5,
}

// machExclaves is the trap for what runs beside the kernel on the chips that keep such a world
// apart (exclaves, from the A18 and the M4). A Mac without one, an M3's, was measured to answer
// every request the same way, whatever it is given: it is not supported. A chip that has them,
// or one not known here, is not answered for.
func machExclaves(p *Process, a []uint64) int64 {
	model := "Macmini9,1"
	if v, ok := p.os["model"]; ok {
		model = v
	}
	switch chipFamily(model) {
	case 0, 0x204526d0, 0x75d4acb9, 0x6f5129ac: // not known, or the A18, the A18 Pro and the M4
		p.unimplemented("exclaves_ctl_trap on a chip that may have exclaves", a)
	}
	return kernNotSupported
}

// chipFamily is the hw.cpufamily of the device of a product type (iPhone11,2), or 0 for one
// cpuFamilies does not know.
func chipFamily(product string) uint64 {
	if family, ok := cpuFamilies[product]; ok {
		return family
	}
	generation, _, _ := strings.Cut(product, ",")
	if family, ok := cpuFamilies[generation]; ok {
		return family
	}
	return 0
}

// sysctlValue is a uint64 or a string.
func (p *Process) sysctlValue(name string) (any, bool) {
	if p.sysctls == nil {
		os := func(key, fallback string) string {
			if v, ok := p.os[key]; ok {
				return v
			}
			return fallback
		}
		n := func(v uint64) any { return v }
		p.sysctls = map[string]any{
			"hw.pagesize": n(16384), "hw.pagesize32": n(16384), "vm.pagesize": n(16384), "hw.ncpu": n(1), "hw.activecpu": n(1),
			"hw.physicalcpu": n(1), "hw.physicalcpu_max": n(1), "hw.logicalcpu": n(1), "hw.logicalcpu_max": n(1),
			"hw.memsize": n(guestMemory), "hw.physmem": n(guestMemory), "hw.byteorder": n(1234), "hw.cputype": n(cpuARM64),
			"hw.cpusubtype": n(cpuSubtypeARM64E), "hw.cpu64bit_capable": n(1), "hw.cachelinesize": n(128), "hw.packages": n(1),
			"hw.nperflevels": n(1), "hw.perflevel0.logicalcpu": n(1), "hw.perflevel0.physicalcpu": n(1),
			"hw.perflevel0.logicalcpu_max": n(1), "hw.machine": "arm64", "hw.model": os("model", "Macmini9,1"),
			"kern.ostype": "Darwin", "kern.bootsessionuuid": bootSession, "kern.hostname": "emu.local", "kern.argmax": n(1 << 20), "kern.hostid": n(0), "kern.wq_max_threads": n(512),
			"kern.usrstack64": n(stackTop), "kern.safeboot": n(0), "kern.ngroups": n(16),
			"kern.secure_kernel": n(0), "kern.bootargs": "", "kern.osvariant_status": n(osVariantMac), "kern.willshutdown": n(0),
			"kern.hv_vmm_present": n(0), "sysctl.proc_translated": n(0), "kern.osrelease": os("release", "22.1.0"),
			"kern.osversion": os("build", "22A400"), "kern.osproductversion": os("version", "13.0.1"),
			"hw.optional.arm64": n(1), "hw.optional.floatingpoint": n(1),
			"hw.optional.neon": n(1), "hw.optional.armv8_1_atomics": n(1), "hw.optional.armv8_crc32": n(1),
			"hw.optional.arm.FEAT_LSE": n(1), "hw.optional.arm.FEAT_CRC32": n(1), "hw.optional.AdvSIMD": n(1),
			"hw.optional.arm.AdvSIMD": n(1), "security.mac.lockdown_mode_state": n(0), "kern.development": n(0),
			"vm.vm_page_free_target": n(4000), "kern.memorystatus_level": n(50), // half free, as host_statistics says
		}
		// The board, as the restore image's build manifest names it (J274AP), less its suffix.
		board := os("board", "")
		if board != "" {
			p.sysctls["hw.targettype"] = strings.TrimSuffix(board, "AP")
		}
		if p.cache.Platform == dyldcache.IOS {
			// An iPhone names its product where a Mac says arm64, and its board where a Mac
			// names its product.
			p.sysctls["hw.machine"], p.sysctls["hw.model"] = os("model", "iPhone"), board
			p.sysctls["kern.osvariant_status"] = n(osVariantIOS)
		}
		family := chipFamily(os("model", "Macmini9,1"))
		if family != 0 {
			p.sysctls["hw.cpufamily"] = n(family)
		}
		if family == cpuFamilyFirestorm && p.cache.Platform != dyldcache.IOS {
			// What every M1 Mac reports: the efficiency cores' caches.
			p.sysctls["hw.l1icachesize"], p.sysctls["hw.l1dcachesize"], p.sysctls["hw.l2cachesize"] = n(128<<10), n(64<<10), n(4<<20)
		}
	}
	if v, ok := p.sysctls[name]; ok {
		return v, true
	}
	if name == "kern.boottime" {
		// When the machine's own counter was at nothing, as a struct timeval: the guests' time
		// since boot (mach_absolute_time) counts from then. A Mac's kernel says a time some
		// seconds later, those it took to come up, which no machine here spends.
		boot := p.m.clock.boot
		return append(le64(uint64(boot.Unix())), le64(uint64(boot.Nanosecond()/1000))...), true
	}
	if strings.HasPrefix(name, "hw.optional.") {
		return uint64(0), true
	}
	return nil, false
}

func (p *Process) sysctlOut(name string, oldp, oldlenp uint64) int64 {
	v, ok := p.sysctlValue(name)
	if !ok {
		p.note("  sysctl ENOENT %s", name)
		return -eNOENT
	}
	p.note("  sysctl %s", name)
	var data []byte
	switch v := v.(type) {
	case string:
		data = str(v)
	case []byte:
		data = v
	case uint64:
		want := uint64(8)
		if oldlenp != 0 {
			want = p.u64(oldlenp)
		}
		data = le64(v)
		// An integer is four bytes however much room there is, where what it is is known.
		if want == 4 || (want != 8 && v < 1<<31) || sysctlFormats[name].kind&0xF == 2 {
			data = data[:4]
		}
	}
	if oldp != 0 && oldlenp != 0 {
		p.write(oldp, data[:min(p.u64(oldlenp), uint64(len(data)))])
	}
	if oldlenp != 0 {
		p.write(oldlenp, le64(uint64(len(data))))
	}
	return 0
}

// sysMadvise is madvise(address, length, advice): advice the emulator has no use for, taken of
// memory that is mapped and of the kinds a Mac's kernel knows (the source's list, 0 to 11); any
// other advice, and memory that is not mapped, is EINVAL, as measured of one of each.
func sysMadvise(p *Process, a []uint64) int64 {
	if a[2] > 11 || p.findRegion(a[0]&tbiMask) == nil {
		return -eINVAL
	}
	if start, end := a[0]&tbiMask&^(pageSize-1), align(a[0]&tbiMask+a[1], pageSize); a[2] <= 2 && end > start { // to be read as usual, at random or in order: the region is said to be so
		p.split(start)
		p.split(end)
		for _, r := range p.overlaps(start, end) {
			r.behavior = uint32(a[2])
		}
	}
	return 0
}
