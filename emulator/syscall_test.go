package emulator

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KishanBagaria/ipswrun/dyldcache"
)

// Unicorn needs the main thread's stack (see cmd/ipswrun), and tests run on other goroutines:
// TestMain keeps the main thread and runs on it whatever onMain is given.
var mainThread = make(chan func())

func init() { runtime.LockOSThread() }

func TestMain(m *testing.M) {
	code := make(chan int)
	go func() { code <- m.Run() }()
	for {
		select {
		case f := <-mainThread:
			f()
		case c := <-code:
			os.Exit(c)
		}
	}
}

func onMain(f func()) {
	done := make(chan struct{})
	mainThread <- func() { defer close(done); f() }
	<-done
}

// ---- the tables ----

func TestHandlersAreReachable(t *testing.T) {
	// A handler is found by the name its number has, so one filed under a name no number has can
	// never run.
	bsd := map[string]bool{}
	for _, name := range bsdNames {
		bsd[strings.ReplaceAll(name, "_nocancel", "")] = true
	}
	mach := map[string]bool{}
	for _, name := range machNames {
		mach[name] = true
	}
	for _, table := range []struct {
		kind     string
		handlers map[string]handler
		names    map[string]bool
	}{{"system call", bsdHandlers, bsd}, {"mach trap", machHandlers, mach}} {
		for name, h := range table.handlers {
			if !table.names[name] {
				t.Errorf("%s handler %q has no number", table.kind, name)
			}
			if h.fn == nil || h.args < 0 || h.args > 8 { // the trace slices x0…x7 by args
				t.Errorf("%s handler %q: %d arguments, fn set: %v", table.kind, name, h.args, h.fn != nil)
			}
		}
	}
}

func TestNamesAreUnique(t *testing.T) {
	for kind, names := range map[string]map[int64]string{"system call": bsdNames, "mach trap": machNames} {
		numbers := map[string]int64{}
		for n, name := range names {
			if other, ok := numbers[name]; ok {
				t.Errorf("%s %q is both %d and %d", kind, name, other, n)
			}
			numbers[name] = n
		}
	}
}

func TestSystemCallNumbers(t *testing.T) {
	// From xnu's bsd/kern/syscalls.master and osfmk/kern/syscall_sw.c: the calls every guest makes.
	for n, name := range map[int64]string{1: "exit", 3: "read", 4: "write", 5: "open", 6: "close", 20: "getpid", 73: "munmap",
		92: "fcntl", 153: "pread", 197: "mmap", 199: "lseek", 202: "sysctl", 274: "sysctlbyname", 294: "shared_region_check_np",
		336: "proc_info", 338: "stat64", 339: "fstat64", 340: "lstat64", 366: "bsdthread_register", 372: "thread_selfid",
		381: "__mac_syscall", 396: "read_nocancel", 397: "write_nocancel", 398: "open_nocancel", 399: "close_nocancel",
		441: "guarded_open_np", 463: "openat", 500: "getentropy", 515: "ulock_wait", 516: "ulock_wake",
		520: "terminate_with_payload", 521: "abort_with_payload", 536: "shared_region_map_and_slide_2_np", 544: "ulock_wait2"} {
		if bsdNames[n] != name {
			t.Errorf("system call %d is %q, want %q", n, bsdNames[n], name)
		}
	}
	for n, name := range map[int64]string{10: "vm_allocate", 12: "vm_deallocate", 15: "vm_map", 26: "reply_port", 27: "thread_self",
		28: "task_self", 29: "host_self", 31: "mach_msg", 32: "mach_msg_overwrite", 36: "semaphore_wait", 38: "semaphore_timedwait",
		41: "port_guard", 42: "port_unguard", 46: "pid_for_task", 47: "mach_msg2",
		89: "timebase_info", 91: "mk_timer_create"} {
		if machNames[n] != name {
			t.Errorf("mach trap %d is %q, want %q", n, machNames[n], name)
		}
	}
}

func TestSystemCallNumbersMatchSDK(t *testing.T) {
	sdk, err := exec.Command("xcrun", "--show-sdk-path").Output()
	if err != nil {
		t.Skip("no SDK:", err)
	}
	header, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(sdk)), "usr/include/sys/syscall.h"))
	if err != nil {
		t.Skip(err)
	}
	defined := regexp.MustCompile(`(?m)^#define\s+SYS_(\w+)\s+(\d+)$`).FindAllStringSubmatch(string(header), -1)
	if len(defined) < 400 {
		t.Fatalf("only %d system calls found in the SDK's sys/syscall.h", len(defined))
	}
	for _, d := range defined {
		n, _ := strconv.ParseInt(d[2], 10, 64)
		if name, ok := bsdNames[n]; ok && name != d[1] && d[1] != "MAXSYSCALL" && d[1] != "invalid" {
			t.Errorf("system call %d is %q, and %q in the SDK", n, name, d[1])
		}
	}
}

func TestConstantHandlers(t *testing.T) {
	// These answer without looking at the process or the arguments.
	for name, want := range map[string]int64{"issetugid": 0, "csrctl": -ePERM} {
		if got := bsdHandlers[name].fn(nil, nil); got != want {
			t.Errorf("%s() = %d, want %d", name, got, want)
		}
	}
	for name, want := range trivial {
		if got := bsdHandlers[name].fn(nil, nil); got != want {
			t.Errorf("%s() = %d, want %d", name, got, want)
		}
	}
	for name, want := range map[string]int64{"task_dyld_process_info_notify_get": kernFailure} {
		if got := machHandlers[name].fn(nil, nil); got != want {
			t.Errorf("mach %s() = %#x, want %#x", name, got, want)
		}
	}
}

// ---- dispatch, through the real engine ----

const (
	svc       = 0xD4001001 // svc #0x80
	codeAddr  = stackTop - stackSize
	dataAddr  = codeAddr + pageSize
	nowhere   = 0x7000000000 // in no region
	carryFlag = 1 << 29
)

// newProcess is a process with no program: enough to make system calls from. It skips the test
// unless ipswrun has already fetched its libunicorn (ipswrun --setup); tests do not download.
func newProcess(t *testing.T) *Process {
	base, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	unicorn := unicornPath(filepath.Join(base, "ipswrun"))
	if library, err := os.ReadFile(unicorn); err != nil || sha256Hex(library) != unicornBuilds[runtime.GOOS+"/"+runtime.GOARCH].librarySHA256 {
		t.Skip("no verified libunicorn in the user cache directory; run ipswrun --setup")
	}
	exe := filepath.Join(t.TempDir(), "guest")
	if err := os.WriteFile(exe, []byte("not run"), 0o666); err != nil {
		t.Fatal(err)
	}
	var p *Process
	onMain(func() { // New runs the engine to turn the MMU on
		var m *Machine
		if m, err = NewMachine(Config{Unicorn: unicorn, Cache: &dyldcache.Cache{}, Home: t.TempDir(), Dyld: le32(0xFEEDFACF, cpuARM64, 0, 0, 0, 0, 0, 0),
			OS: map[string]string{"build": "24G90", "version": "15.6.1"}}); err == nil {
			p, err = m.newProcess(exe, []byte("not run"), GuestExe)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { onMain(p.m.Close) })
	p.m.deadline = p.m.clock.now().Add(time.Minute)
	return p
}

// number is the number of the system call with a name.
func number(t *testing.T, name string) int64 {
	for n, candidate := range bsdNames {
		if candidate == name {
			return n
		}
	}
	t.Fatalf("no system call named %s", name)
	return 0
}

// trap executes one svc with x16 = number and the given argument registers. It returns x0, the
// carry flag (how a BSD call reports an errno) and what stopped the run, if anything did.
func (p *Process) trap(number int64, args ...uint64) (x0 uint64, carry bool, stopped string) {
	p.fault = ""
	p.write(codeAddr, le32(svc))
	for i := 0; i < 8; i++ {
		p.setX(i, 0)
	}
	for i, a := range args {
		p.setX(i, a)
	}
	p.uc.setReg(regX0+16, uint64(number))
	if err := p.uc.start(codeAddr, codeAddr+4, 10_000_000); err != nil && p.fault == "" {
		p.fault = err.Error()
	}
	return p.x(0), p.uc.reg(regNZCV)&carryFlag != 0, p.fault
}

// scheduleFrom is the machine's schedule, for the thread that stopped at pc; it returns where the next resumes.
func (p *Process) scheduleFrom(pc uint64) uint64 {
	p.current.pc, p.m.running = pc, p
	p.m.schedule()
	return p.current.pc
}

func TestDispatch(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		p.write(dataAddr, []byte("kern.osversion\x00"))
		p.write(dataAddr+0x100, le64(64)) // the size of the buffer at dataAddr+0x200

		for _, tc := range []struct {
			name  string
			args  []uint64
			x0    uint64
			carry bool
		}{
			{"getpid", nil, guestPID, false},
			{"umask", nil, 0o22, false},
			{"socket", nil, eACCES, true},                // a negative result is an errno: carry set, x0 positive
			{"ptrace", []uint64{ptDenyAttach}, 0, false}, // nothing would attach
			{"ptrace", []uint64{10, 1}, eINVAL, true},    // PT_ATTACH: nothing to attach to
			{"read", []uint64{99, dataAddr, 8}, eBADF, true},
			{"read_nocancel", []uint64{99, dataAddr, 8}, eBADF, true},   // shares read's handler
			{"open", []uint64{nowhere, 0, 0}, eFAULT, true},             // a path the guest has not mapped
			{"sysctlbyname", []uint64{nowhere, 15, 0, 0}, eFAULT, true}, // likewise
			{"sysctlbyname", []uint64{dataAddr, 15, dataAddr + 0x200, dataAddr + 0x100}, 0, false},
		} {
			x0, carry, stopped := p.trap(number(t, tc.name), tc.args...)
			if stopped != "" || x0 != tc.x0 || carry != tc.carry {
				t.Errorf("%s: x0 %d carry %v stopped %q; want x0 %d carry %v", tc.name, x0, carry, stopped, tc.x0, tc.carry)
			}
		}
		if got := p.cstr(dataAddr + 0x200); got != "24G90" || p.u64(dataAddr+0x100) != 6 {
			t.Errorf("sysctlbyname kern.osversion wrote %q, length %d", got, p.u64(dataAddr+0x100))
		}

		// Mach traps are the negative numbers, and return a kern_return_t or a value: no carry.
		if x0, carry, stopped := p.trap(-28); !p.isTask(uint32(x0)) || carry || stopped != "" {
			t.Errorf("task_self: x0 %#x carry %v stopped %q", x0, carry, stopped)
		}
		first, _, _ := p.trap(-26)
		if second, _, stopped := p.trap(-26); first == 0 || first == second || stopped != "" {
			t.Errorf("reply_port gave %#x then %#x (stopped %q), want two ports", first, second, stopped)
		}
		if x0, _, stopped := p.trap(-10, 0x203, nowhere, pageSize, 1); x0 != 1 || stopped != "" { // KERN_INVALID_ADDRESS
			t.Errorf("vm_allocate with an unmapped out pointer: x0 %d stopped %q", x0, stopped)
		}

		// Machine-dependent calls: set and get the thread pointer.
		if x0, _, stopped := p.trap(0x80000000, 0x1234000, 0, 0, 2); x0 != 0 || stopped != "" {
			t.Errorf("set thread pointer: x0 %#x stopped %q", x0, stopped)
		}
		if x0, _, stopped := p.trap(0x80000000, 0, 0, 0, 3); x0 != 0x1234000 || stopped != "" {
			t.Errorf("get thread pointer: x0 %#x stopped %q", x0, stopped)
		}

		// Nothing is guessed: a call with no handler stops the run and says which.
		for number, want := range map[int64]string{
			51:  "unimplemented syscall 51 acct(",
			999: "unimplemented syscall 999 999(",
			-90: "unimplemented mach trap 90 mach_wait_until(",
			-99: "unimplemented mach trap 99 99(",
		} {
			if _, _, stopped := p.trap(number, 1, 2); !strings.HasPrefix(stopped, want) || !strings.Contains(stopped, "0x1, 0x2, 0x0") {
				t.Errorf("trap %d stopped with %q, want %q and its arguments", number, stopped, want)
			}
			if p.faultKind != StopUnimplemented {
				t.Errorf("trap %d stopped as kind %d, not as unimplemented", number, p.faultKind)
			}
		}
		// A survey answers them with an error instead, and keeps count.
		p.survey = true
		for i := 0; i < 2; i++ {
			if x0, carry, stopped := p.trap(51); x0 != eNOSYS || !carry || stopped != "" {
				t.Errorf("acct under a survey: x0 %d carry %v stopped %q", x0, carry, stopped)
			}
		}
		if x0, _, stopped := p.trap(-90); x0 != kernInvalidArg || stopped != "" {
			t.Errorf("mach_wait_until under a survey: x0 %d stopped %q", x0, stopped)
		}
		p.unknown[unknownMIG{id: 3400, port: 0x707}] = 5
		list := strings.Split(p.Unimplemented(), "\n")
		for i, want := range []string{"2× syscall 51 acct, first from ", "1× mach trap 90 mach_wait_until, first from ", "5× mach message 3400 to port 0x707 from "} {
			if i >= len(list) || !strings.Contains(list[i], want) {
				t.Errorf("survey line %d: want %q in %q", i, want, list)
			}
		}
		p.survey = false

		if _, _, stopped := p.trap(0x80000000, 0, 0, 0, 7); stopped != "machdep call 7" {
			t.Errorf("machdep call 7 stopped with %q", stopped)
		}

		// exit ends the process with its status.
		if _, _, stopped := p.trap(1, 3); stopped != "" {
			t.Errorf("exit stopped with %q", stopped)
		}
		if code, ok := p.ExitCode(); !ok || code != 3 {
			t.Errorf("after exit(3): code %d, exited %v", code, ok)
		}
	})
}

func TestBugFixes(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		// getentropy succeeds up to 256 bytes and fails with EIO above 256.
		getentropy := number(t, "getentropy")
		if x0, carry, stopped := p.trap(getentropy, dataAddr, 256); stopped != "" || carry || x0 != 0 {
			t.Errorf("getentropy(256): x0=%d carry=%v stopped=%q", x0, carry, stopped)
		}
		if x0, carry, stopped := p.trap(getentropy, dataAddr, 257); stopped != "" || !carry || x0 != eIO {
			t.Errorf("getentropy(257): x0=%d carry=%v stopped=%q; want EIO", x0, carry, stopped)
		}

		// hw.model reflects Config.OS["model"] when set.
		p.os["model"] = "Mac16,1"
		p.sysctls = nil
		p.write(dataAddr, str("hw.model"))
		p.write(dataAddr+0x100, le64(64))
		if x0, carry, stopped := p.trap(number(t, "sysctlbyname"), dataAddr, 8, dataAddr+0x200, dataAddr+0x100); stopped != "" || carry || x0 != 0 {
			t.Fatalf("sysctlbyname hw.model: x0=%d carry=%v stopped=%q", x0, carry, stopped)
		}
		if got := p.cstr(dataAddr + 0x200); got != "Mac16,1" {
			t.Errorf("hw.model = %q, want Mac16,1", got)
		}

		// open_dprotected_np creates a file as open does, whatever the protection class.
		p.write(dataAddr, str(GuestHome+"/protected"))
		if fd, carry, stopped := p.trap(number(t, "open_dprotected_np"), dataAddr, 0x201, 4, 0, 0o600); stopped != "" || carry || fd < 3 {
			t.Errorf("open_dprotected_np: fd=%d carry=%v stopped=%q", fd, carry, stopped)
		} else {
			p.trap(number(t, "close"), fd)
		}

		// chdir, fchdir, relative paths, AT_FDCWD, and F_GETPATH (getcwd).
		p.write(dataAddr, str(GuestHome+"/sub"))
		if x0, carry, _ := p.trap(number(t, "mkdir"), dataAddr, 0o755); carry || x0 != 0 {
			t.Fatalf("mkdir %s/sub: x0=%d carry=%v", GuestHome, x0, carry)
		}
		if x0, carry, _ := p.trap(number(t, "chdir"), dataAddr); carry || x0 != 0 {
			t.Fatalf("chdir: x0=%d carry=%v", x0, carry)
		}
		// open relative file via open and openat(AT_FDCWD).
		p.write(dataAddr, str("rel.txt"))
		fd1, carry, _ := p.trap(number(t, "open"), dataAddr, 0x202, 0o644) // O_RDWR|O_CREAT
		if carry || fd1 < 3 {
			t.Fatalf("relative open: fd=%d carry=%v", fd1, carry)
		}
		p.trap(number(t, "close"), fd1)
		// chdir to a non-directory fails with ENOTDIR.
		if x0, carry, _ := p.trap(number(t, "chdir"), dataAddr); !carry || x0 != eNOTDIR {
			t.Errorf("chdir to file: x0=%d carry=%v, want ENOTDIR", x0, carry)
		}
		// openat with AT_FDCWD (-2) resolves relative to cwd, and bad dirfd returns EBADF.
		atFD, carry, _ := p.trap(number(t, "openat"), ^uint64(1), dataAddr, 0, 0) // int32(-2)
		if carry || atFD < 3 {
			t.Fatalf("openat(AT_FDCWD, rel.txt): fd=%d carry=%v", atFD, carry)
		}
		p.trap(number(t, "close"), atFD)
		if x0, carry, _ := p.trap(number(t, "openat"), 999, dataAddr, 0, 0); !carry || x0 != eBADF {
			t.Errorf("openat(999, rel.txt): x0=%d carry=%v, want EBADF", x0, carry)
		}
		// open(".") + fcntl(F_GETPATH) returns cwd (how Darwin getcwd works).
		p.write(dataAddr, str("."))
		dotFD, carry, _ := p.trap(number(t, "open"), dataAddr, 0, 0)
		if carry {
			t.Fatalf("open(.): %d", dotFD)
		}
		if x0, carry, _ := p.trap(number(t, "fcntl"), dotFD, 50, dataAddr+0x200); carry || x0 != 0 {
			t.Fatalf("fcntl F_GETPATH: x0=%d carry=%v", x0, carry)
		}
		if got := p.cstr(dataAddr + 0x200); got != GuestHome+"/sub" {
			t.Errorf("getcwd via F_GETPATH = %q, want %s/sub", got, GuestHome)
		}
		// fchdir back to home.
		p.write(dataAddr, str(GuestHome))
		homeFD, carry, _ := p.trap(number(t, "open"), dataAddr, 0, 0)
		if carry {
			t.Fatalf("open(home): %d", homeFD)
		}
		if x0, carry, _ := p.trap(number(t, "fchdir"), homeFD); carry || x0 != 0 || p.cwd != GuestHome {
			t.Errorf("fchdir: x0=%d carry=%v cwd=%q", x0, carry, p.cwd)
		}
		p.trap(number(t, "close"), dotFD)
		p.trap(number(t, "close"), homeFD)

		// dup(1), fcntl(F_DUPFD, min), and dup2 keep stdio writes captured instead of leaking to host.
		dupOut, carry, _ := p.trap(number(t, "dup"), 1)
		if carry || dupOut < 3 {
			t.Fatalf("dup(1): fd=%d carry=%v", dupOut, carry)
		}
		fdupOut, carry, _ := p.trap(number(t, "fcntl"), 1, 0, 7) // F_DUPFD >= 7
		if carry || fdupOut != 7 {
			t.Fatalf("fcntl(1, F_DUPFD, 7): fd=%d carry=%v, want 7", fdupOut, carry)
		}
		dup2Err, carry, _ := p.trap(number(t, "dup2"), 2, 10)
		if carry || dup2Err != 10 {
			t.Fatalf("dup2(2, 10): fd=%d carry=%v, want 10", dup2Err, carry)
		}
		p.write(dataAddr, []byte("via-dup "))
		p.trap(number(t, "write"), dupOut, dataAddr, 8)
		p.write(dataAddr, []byte("via-fdup"))
		p.trap(number(t, "write"), fdupOut, dataAddr, 8)
		p.write(dataAddr, []byte("err-dup2"))
		p.trap(number(t, "write"), dup2Err, dataAddr, 8)
		p.trap(number(t, "close"), dupOut)
		p.trap(number(t, "close"), fdupOut)
		p.trap(number(t, "close"), dup2Err)
		if p.Stdout() != "via-dup via-fdup" || p.Stderr() != "err-dup2" {
			t.Errorf("captured stdout=%q stderr=%q", p.Stdout(), p.Stderr())
		}
	})
}

// An AMX instruction stops the run as unimplemented and says what it is, rather than as an
// unexplained CPU exception.
func TestAMXStops(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		p.fault, p.faultKind = "", StopOther
		p.write(codeAddr, le32(0x00201220)) // AMX set
		if err := p.uc.start(codeAddr, codeAddr+4, 1000); err != nil && p.fault == "" {
			p.fault = err.Error()
		}
		if !strings.HasPrefix(p.fault, "AMX instruction 00201220") || p.faultKind != StopUnimplemented {
			t.Errorf("an AMX instruction stopped with %q, kind %d", p.fault, p.faultKind)
		}
	})
	for instruction, want := range map[uint32]bool{0x00201000: true, 0x002013ff: true, 0x00201400: false, 0xd503201f: false} {
		if amx(instruction) != want {
			t.Errorf("amx(%08x) = %v", instruction, !want)
		}
	}
}

func TestRebootSaysWhy(t *testing.T) {
	// A daemon that gives up asks the kernel to panic and says why; the stop says it too, and a
	// message that cannot be read still stops the run.
	for _, c := range []struct {
		how, message uint64
		stop         string
	}{
		{0xc00, dataAddr, `reboot, asked to panic: "no node configuration"`},
		{0xc00, 0x10, "reboot, asked to panic with a message that cannot be read"},
		{0, 0, "reboot("},
	} {
		p := newProcess(t)
		onMain(func() {
			p.write(dataAddr, []byte("no node configuration\x00"))
			if _, _, stopped := p.trap(number(t, "reboot"), c.how, c.message); !strings.Contains(stopped, c.stop) {
				t.Errorf("reboot(%#x, %#x) stopped with %q, want %q", c.how, c.message, stopped, c.stop)
			}
		})
	}
}
