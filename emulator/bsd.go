package emulator

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
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
		"open": {3, sysOpen}, "openat": {4, sysOpenat}, "guarded_open_np": {5, sysGuardedOpen}, "guarded_close_np": {2, sysGuardedClose},
		"guarded_write_np": {4, sysGuardedWrite}, "guarded_pwrite_np": {5, sysGuardedPwrite}, "close": {1, sysClose},
		"read": {3, sysRead}, "pread": {4, sysPread}, "write": {3, sysWrite}, "pwrite": {4, sysPwrite}, "lseek": {3, sysLseek},
		"ftruncate": {2, sysFtruncate}, "fsync": {1, constant(0)}, "access": {2, sysAccess}, "unlink": {1, sysUnlink},
		"mkdir": {2, sysMkdir}, "rename": {2, sysRename}, "readlink": {3, sysReadlink}, "readlinkat": {4, sysReadlinkat},
		"mkdirat": {3, sysMkdirat}, "rmdir": {1, sysRmdir}, "unlinkat": {3, sysUnlinkat}, "renameat": {4, sysRenameat},
		"renameatx_np": {5, sysRenameatx}, "faccessat": {4, sysFaccessat}, "truncate": {2, sysTruncate}, "fcntl": {3, sysFcntl}, "dup": {1, sysDup},
		"ioctl": {3, constant(-eNOTTY)}, "fsctl": {4, constant(-eNOTSUP)}, "statfs64": {2, sysStatfs64}, "fstatfs64": {2, sysFstatfs64},
		"getattrlist": {5, sysGetattrlist}, "getattrlistat": {6, sysGetattrlistat}, "fgetattrlist": {5, sysFgetattrlist},
		"getattrlistbulk": {5, sysGetattrlistbulk}, "fsgetpath": {4, sysFsgetpath}, "shm_open": {3, sysShmOpen}, "shm_unlink": {1, sysShmUnlink},
		"getfsstat64": {3, constant(0)}, "pathconf": {2, sysPathconf}, "fpathconf": {2, sysPathconf}, "getdirentries64": {4, sysGetdirentries64},
		// memory
		"mmap": {6, sysMmap}, "munmap": {2, sysMunmap}, "mprotect": {3, constant(0)}, "madvise": {3, constant(0)},
		"mremap_encrypted": {0, constant(0)}, "shared_region_check_np": {1, sysSharedRegionCheck},
		"shared_region_map_and_slide_2_np": {4, sysSharedRegionMap}, "map_with_linking_np": {4, constant(-eNOTSUP)},
		// process
		"exit": {1, sysExit}, "getpid": {0, sysGetpid}, "getppid": {0, constant(1)}, "getuid": {0, sysGetuid},
		"geteuid": {0, sysGetuid}, "getgid": {0, sysGetgid}, "getegid": {0, sysGetgid}, "gettid": {2, constant(-eSRCH)},
		"issetugid": {0, constant(0)}, "sigaction": {3, sysSigaction}, "sigprocmask": {3, sysSigprocmask},
		"__pthread_sigmask": {3, sysSigprocmask}, "__pthread_kill": {2, sysPthreadKill}, "kill": {3, sysKill},
		"sigaltstack": {2, constant(0)}, "__disable_threadsignal": {1, constant(0)}, "thread_selfid": {0, sysThreadSelfid},
		"bsdthread_register": {6, sysBsdthreadRegister}, "bsdthread_create": {5, sysBsdthreadCreate},
		"bsdthread_terminate": {4, sysBsdthreadTerminate}, "__semwait_signal": {6, sysSemwaitSignal},
		"sigsuspend": {1, sysSigsuspend}, "__sigwait": {2, sysSigsuspend}, "kevent_qos": {8, sysKeventQos}, "kevent_id": {8, sysKeventID}, "psynch_mutexwait": {5, sysPsynchMutexwait}, "psynch_mutexdrop": {5, sysPsynchMutexdrop}, "psynch_cvwait": {8, sysPsynchCvwait},
		"psynch_cvsignal": {8, sysPsynchCvsignal}, "psynch_cvbroad": {7, sysPsynchCvbroad}, "psynch_cvclrprepost": {7, constant(0)},
		"psynch_rw_rdlock": {5, sysPsynchRWRdlock}, "psynch_rw_wrlock": {5, sysPsynchRWWrlock}, "psynch_rw_unlock": {5, sysPsynchRWUnlock}, "bsdthread_ctl": {4, constant(0)}, "workq_open": {0, constant(0)},
		"workq_kernreturn": {4, sysWorkqKernreturn}, "kdebug_trace64": {0, constant(0)}, "kdebug_trace": {0, constant(0)},
		"kdebug_trace_string": {0, constant(0)}, "kdebug_typefilter": {2, constant(-ePERM)}, "getentropy": {2, sysGetentropy},
		"ptrace": {1, sysPtrace}, "select": {5, sysSelect}, "pselect": {5, sysPselect}, "kqueue": {0, sysKqueue}, "guarded_kqueue_np": {2, sysKqueue}, "csops": {4, sysCsops}, "csops_audittoken": {4, sysCsops}, "csrctl": {3, constant(-ePERM)}, "getrlimit": {2, sysGetrlimit},
		"setrlimit": {2, constant(0)}, "gettimeofday": {3, sysGettimeofday}, "__mac_syscall": {3, sysMacSyscall},
		"abort_with_payload": {6, sysAbortWithPayload}, "terminate_with_payload": {6, sysTerminateWithPayload},
		"os_fault_with_payload": {6, sysOSFaultWithPayload}, "ulock_wait": {4, sysUlockWait}, "ulock_wait2": {5, sysUlockWait2},
		"ulock_wake": {3, sysUlockWake}, "proc_info": {6, sysProcInfo}, "getpgrp": {0, sysGetpid}, "gethostuuid": {2, sysGethostuuid}, "sysctl": {6, sysSysctl}, "sysctlbyname": {6, sysSysctlbyname},
	}
	for name, v := range trivial {
		bsdHandlers[name] = handler{4, constant(v)}
	}
}

// trivial are more calls answered with a constant (negative: errno) and no side effects.
var trivial = map[string]int64{
	"objc_bp_assist_cfg_np": 0, "iopolicysys": 0, "work_interval_ctl": -eNOTSUP, "memorystatus_control": 0, "proc_rlimit_control": 0,
	"thread_selfusage": 0, "crossarch_trap": -eNOTSUP, "kevent64": 0, "kevent": 0,
	"kqueue_workloop_ctl": 0, "necp_open": -eNOSYS, "socket": -eACCES, "umask": 0o22,
	"audit_session_self": 0, "getaudit_addr": -eNOSYS, "coalition_info": -eNOSYS, "persona": -eNOSYS,
	"debug_syscall_reject_config": 0, "debug_syscall_reject": 0, "setattrlist": 0, "fsetattrlist": 0, "chmod": 0, "fchmod": 0,
	"flock": 0, "setxattr": 0, "fsetxattr": 0, "getxattr": -eNOATTR, "fgetxattr": -eNOATTR, "listxattr": 0, "flistxattr": 0, "chdir": 0,
	"memorystatus_get_level": 0, "record_system_event": 0, "log_data": 0, "oslog_coproc_reg": 0, "oslog_coproc": 0, "setpriority": 0,
	"getpriority": 0, "setsid": 0, "setitimer": 0, "utimes": 0, "futimes": 0, "fchown": 0, "chown": 0, "sync": 0,
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

func (p *Process) bsd(n int64) {
	name, ok := bsdNames[n]
	if !ok {
		name = strconv.FormatInt(n, 10)
	}
	args := p.args()
	h, ok := bsdHandlers[strings.ReplaceAll(name, "_nocancel", "")]
	if !ok && p.unimplemented(fmt.Sprintf("syscall %d %s", n, name), args) {
		p.setX(0, eNOSYS)
		p.carry(true)
		return
	}
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
	fixed := flags&0x10 != 0
	if fixed && addr&tbiMask+length > vaLimit {
		return -eNOMEM
	}
	if flags&0x1000 != 0 || fdn >= 1<<31 { // MAP_ANON, or no descriptor
		start, ok := p.alloc(length, allocation{addr: addr, fixed: fixed, name: "mmap"})
		if !ok {
			return -eNOMEM
		}
		return int64(start)
	}
	f := p.fds[fdn&0xFFFFFFFF]
	if f == nil {
		return -eBADF
	}
	if f.kind == dirFD {
		return -eACCES
	}
	if f.shared != nil && flags&mapShared != 0 {
		return p.mapShared(f.shared, addr, length, off, fixed)
	}
	name := "?"
	if f.path != "" {
		name = f.path
	}
	what := allocation{addr: addr, fixed: fixed, kind: fileRegion, name: "mmap:" + name}
	if f.kind == cacheFD { // fetched by range, so only the pages the guest touches
		what.file, what.off = f.cache, off
	} else {
		var errno int64
		if what.data, errno = f.pread(off, length); errno != 0 {
			return errno
		}
	}
	start, ok := p.alloc(length, what)
	if !ok {
		return -eNOMEM
	}
	return int64(start)
}

func sysMunmap(p *Process, a []uint64) int64 {
	p.unmap(a[0]&^(pageSize-1), align(a[0]+a[1], pageSize))
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
			r := &region{start: addr, end: addr + align(size, pageSize), name: "shared-region"}
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
		p.addRegion(&region{start: addr, end: addr + size, kind: cacheRegion, name: "cache"})
	}
	return 0
}

// ---- process ----

func sysExit(p *Process, a []uint64) int64 {
	p.exitCode, p.exited = int(a[0]&0xFF), true
	return 0
}

func sysGetpid(p *Process, a []uint64) int64 { return int64(p.pid) }
func sysGetuid(p *Process, a []uint64) int64 { return int64(p.uid) }
func sysGetgid(p *Process, a []uint64) int64 { return int64(p.gid) }

// hostUUID is the machine's own identifier: the same on every run, as everything else about it is.
var hostUUID = [16]byte{0x1F, 0x5A, 0xE3, 0x70, 0x9C, 0x42, 0x4D, 0x1B, 0x8E, 0x6D, 0x49, 0x50, 0x53, 0x57, 0x52, 0x4E}

func sysGethostuuid(p *Process, a []uint64) int64 {
	p.write(a[0], hostUUID[:])
	return 0
}

func sysSigaction(p *Process, a []uint64) int64 {
	if a[2] != 0 {
		p.write(a[2], make([]byte, 16))
	}
	return 0
}

func sysSigprocmask(p *Process, a []uint64) int64 {
	if a[2] != 0 {
		p.write(a[2], make([]byte, 4))
	}
	return 0
}

func sysPthreadKill(p *Process, a []uint64) int64 {
	stopf("guest raised signal %d", a[1])
	return 0
}

func sysKill(p *Process, a []uint64) int64 {
	stopf("guest kill signal %d", a[1])
	return 0
}

func sysThreadSelfid(p *Process, a []uint64) int64 { return 0x4242 + int64(p.current.id) }

// sysGetentropy is reproducible, but never repeats: corecrypto's health checks reject identical
// blocks.
func sysGetentropy(p *Process, a []uint64) int64 {
	var out []byte
	for uint64(len(out)) < a[1] {
		p.entropy++
		sum := sha256.Sum256(fmt.Appendf(nil, "ipswrun entropy %d", p.entropy))
		out = append(out, sum[:]...)
	}
	p.write(a[0], out[:a[1]])
	return 0
}

func sysCsops(p *Process, a []uint64) int64 {
	p.note("  csops op %d size %d", a[1], a[3])
	if a[1] == 0 {
		p.write(a[2], le32(0x20000005|2))
		return 0
	}
	return -eINVAL // no entitlements, identity or cdhash to report
}

// sysKqueue gives a descriptor that can be described and closed like any other. The events asked
// of it never happen: only libdispatch's own kqueues, which have no descriptor, are emulated.
func sysKqueue(p *Process, a []uint64) int64 {
	n := p.nextFD
	p.fds[n] = &fd{kind: dataFD}
	p.nextFD++
	return int64(n)
}

// selectNothing is select on descriptors nothing ever happens to: none is ready, once the time
// the caller would wait has passed. It never passes for a caller that set none.
func (p *Process) selectNothing(a []uint64, nanoseconds uint64) int64 {
	for _, set := range a[1:4] {
		if set != 0 {
			p.write(set, make([]byte, align(a[0], 32)/8))
		}
	}
	if a[4] == 0 {
		return sysSigsuspend(p, a)
	}
	if deadline := after(relative(p.u64(a[4]), p.u64(a[4]+8)*nanoseconds)); !deadline.IsZero() {
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

func sysGetrlimit(p *Process, a []uint64) int64 {
	limits := []uint64{0x7FFFFFFFFFFFFFFF, 0x7FFFFFFFFFFFFFFF}
	switch a[0] & 0xFF {
	case 3: // RLIMIT_STACK
		limits = []uint64{stackSize, 64 << 20}
	case 8: // RLIMIT_NOFILE
		limits = []uint64{256, 10240}
	}
	p.write(a[1], le64(limits...))
	return 0
}

func sysGettimeofday(p *Process, a []uint64) int64 {
	now := time.Now()
	if a[0] != 0 {
		p.write(a[0], le64(uint64(now.Unix()), uint64(now.Nanosecond()/1000)))
	}
	if a[2] != 0 {
		p.write(a[2], le64(machTicks()))
	}
	return 0
}

func sysMacSyscall(p *Process, a []uint64) int64 {
	policy := p.cstr(a[0])
	p.note("  mac_syscall %s %d", policy, a[1])
	switch {
	case policy == "AMFI" && a[1] == 90:
		p.write(p.u64(a[2]+8), le64(0x7F))
		return 0
	case policy == "Sandbox":
		return 0
	case policy == "Quarantine": // nothing is quarantined, and nothing a guest writes will be
		return 0
	}
	return -eNOSYS
}

// fixed is a string in a field of n bytes, cut short if it has to be to end in a zero.
func fixed(s string, n int) []byte {
	out := make([]byte, n)
	copy(out[:n-1], s)
	return out
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
	// Everything else is asked about a process, which may be another: a daemon asks about its client.
	about := p.m.process(uint32(a[1]))
	if about == nil {
		return -eSRCH
	}
	switch {
	case callnum == 2 && flavor == 3 && size >= 136: // PROC_PIDTBSDINFO
		// struct proc_bsdinfo: flags (64-bit), status (running), exit status, pid, parent; the
		// user and group three times over; the name twice; then nothing until when it started.
		info := le32(0x10, 2, 0, about.pid, launchdPID, about.uid, about.gid, about.uid, about.gid, about.uid, about.gid, 0)
		info = append(append(info, fixed(about.label, 16)...), fixed(about.label, 32)...)
		info = append(info, le32(uint32(len(about.fds)), about.pid, 0, 0, 0, 0)...)
		p.write(buf, append(info, le64(fixedMtime, 0)...))
		return 136
	case callnum == 2 && flavor == 11: // PROC_PIDPATHINFO
		p.write(buf, str(GuestExe))
		return 0
	case callnum == 2 && flavor == 17 && size >= 56: // PROC_PIDUNIQIDENTIFIERINFO
		p.write(buf, append(make([]byte, 16), le64(uint64(about.pid), 1, 0, 0, 0)...))
		return 56
	case callnum == 2 && flavor == 19: // PROC_PIDARCHINFO
		info := le32(cpuARM64, cpuSubtypeARM64E, 0)
		p.write(buf, info[:min(size, 12)])
		return int64(min(size, 12))
	}
	return -eSRCH
}

// ---- sysctl ----

var sysctlMIBs = map[[2]int32]string{
	{1, 1}: "kern.ostype", {1, 2}: "kern.osrelease", {1, 4}: "kern.version", {1, 8}: "kern.argmax", {1, 10}: "kern.hostname",
	{1, 65}: "kern.osversion", {1, 59}: "kern.usrstack64", {1, 66}: "kern.safeboot", {1, 24}: "kern.ngroups",
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
	var s string
	switch {
	case len(mib) >= 2 && mib[0] == 0x7FFF && int(mib[1]) >= 0 && int(mib[1]) < len(p.oids):
		s = p.oids[mib[1]]
	case len(mib) >= 2 && sysctlMIBs[[2]int32{mib[0], mib[1]}] != "":
		s = sysctlMIBs[[2]int32{mib[0], mib[1]}]
	default:
		p.note("  sysctl mib ENOENT %v", mib)
		return -eNOENT
	}
	return p.sysctlOut(s, oldp, oldlenp)
}

func sysSysctlbyname(p *Process, a []uint64) int64 {
	return p.sysctlOut(strings.TrimRight(string(p.read(a[0], a[1])), "\x00"), a[2], a[3])
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
			"hw.cpusubtype": n(cpuSubtypeARM64E), "hw.cpufamily": n(0x1b588bb3), "hw.cpu64bit_capable": n(1), "hw.cachelinesize": n(128),
			"hw.l1dcachesize": n(65536), "hw.l1icachesize": n(131072), "hw.l2cachesize": n(4 << 20), "hw.packages": n(1),
			"hw.nperflevels": n(1), "hw.perflevel0.logicalcpu": n(1), "hw.perflevel0.physicalcpu": n(1),
			"hw.perflevel0.logicalcpu_max": n(1), "hw.machine": "arm64", "hw.model": "Macmini9,1", "hw.targettype": "J274",
			"hw.cpubrand": "Apple M1", "kern.ostype": "Darwin", "kern.hostname": "emu.local", "kern.argmax": n(1 << 20),
			"kern.usrstack64": n(stackTop), "kern.safeboot": n(0), "kern.ngroups": n(16), "kern.maxfilesperproc": n(10240),
			"kern.secure_kernel": n(0), "kern.bootargs": "", "kern.osvariant_status": n(0x70010000F388828A), "kern.willshutdown": n(0),
			"kern.hv_vmm_present": n(0), "sysctl.proc_translated": n(0), "kern.osrelease": os("release", "22.1.0"),
			"kern.osversion": os("build", "22A400"), "kern.osproductversion": os("version", "13.0.1"),
			"kern.version": "Darwin Kernel Version (emulated)", "kern.osproductversioncompat": "10.16",
			"kern.iossupportversion": "16.1", "hw.optional.arm64": n(1), "hw.optional.floatingpoint": n(1),
			"hw.optional.neon": n(1), "hw.optional.armv8_1_atomics": n(1), "hw.optional.armv8_crc32": n(1),
			"hw.optional.arm.FEAT_LSE": n(1), "hw.optional.arm.FEAT_CRC32": n(1), "hw.optional.AdvSIMD": n(1),
			"hw.optional.arm.AdvSIMD": n(1), "security.mac.lockdown_mode_state": n(0), "kern.development": n(0),
			"vm.vm_page_free_target": n(4000), "kern.memorystatus_level": n(50),
		}
	}
	if v, ok := p.sysctls[name]; ok {
		return v, true
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
	case uint64:
		want := uint64(8)
		if oldlenp != 0 {
			want = p.u64(oldlenp)
		}
		data = le64(v)
		if want == 4 || (want != 8 && v < 1<<31) {
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
