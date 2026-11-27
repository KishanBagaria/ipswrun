// Package emulator is a minimal user-mode Darwin "kernel" for arm64e on Unicorn.
//
// It starts a macOS or iOS build's own /usr/lib/dyld the way XNU would, lets it map that build's
// dyld shared cache, and pages cache contents in on first touch. What differs between the two
// (where anonymous memory goes, what the system says of itself) follows the platform the cache
// was built for. System calls and Mach traps are
// implemented just far enough for libSystem, CoreFoundation, Foundation, the Objective-C and
// Swift runtimes, Security, SQLite, libdispatch and XPC to run, as one process or as several that
// send each other Mach messages (see Machine); anything unimplemented stops the run with a
// message rather than guessing.
package emulator

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/KishanBagaria/ipswrun/dyldcache"
	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/types"
	"time"
)

const (
	pageSize  = dyldcache.PageSize
	blockSize = 16 * pageSize
	commPage  = 0xFFFFFC000
	// commPageDiagnostic is where in the commpage the word is that says what programs log for
	// diagnosticd (_COMM_PAGE_ATM_DIAGNOSTIC_CONFIG).
	commPageDiagnostic = 0x104
	commPageRO         = 0xFFFFF4000
	pageTables         = 0x0F0000000000
	vaLimit            = 1 << 44 // the emulated core's physical address size; identity mapped
	tbiMask            = 1<<56 - 1
	stackTop           = 0x170000000
	stackSize          = 8 << 20
	exeBase            = 0x100000000
	dyldBase           = 0x120000000
	// Anonymous mappings grow up from here, clear of anything the guest places itself: from 2 TB
	// on macOS, and on iOS from 16 GB, above the largest shared region, as iOS's Objective-C
	// runtime keeps a class's data pointer in 39 bits.
	heapBase    = 0x20000000000
	heapBaseIOS = 0x400000000
	deviceID    = 0x1000004

	// GuestHome is the emulated user's home directory on macOS, and GuestHomeIOS on iOS, as
	// HomeOf says; it is backed by Config.Home on the host.
	GuestHome    = "/Users/emu"
	GuestHomeIOS = "/var/mobile"
	// GuestExe is the path a guest executable of the host's has inside the guest. One of the
	// build's own is at its path there.
	GuestExe    = "/emu/guest"
	guestDyld   = "/usr/lib/dyld"
	cryptex     = "/System/Volumes/Preboot/Cryptexes/OS"
	cryptexLink = "/System/Cryptexes/OS"
	exeInode    = 0x5105F2A
	dyldInode   = 0x5105F2B
	cacheInode  = 0x5105F30

	// The machine a guest sees, however it asks, the pid of the first process, and the user every
	// process that is not root's runs as.
	guestPID         = 4242
	guestUID         = 501
	guestGID         = 20
	guestMemory      = 8 << 30
	cpuARM64         = 0x100000C
	cpuSubtypeARM64E = 2
)

var le = binary.LittleEndian

func align(n, a uint64) uint64 { return (n + a - 1) &^ (a - 1) }

// str is a C string.
func str(s string) []byte { return append([]byte(s), 0) }

func le32(values ...uint32) []byte {
	out := make([]byte, 0, 4*len(values))
	for _, v := range values {
		out = le.AppendUint32(out, v)
	}
	return out
}

func le16(values ...uint16) []byte {
	out := make([]byte, 0, 2*len(values))
	for _, v := range values {
		out = le.AppendUint16(out, v)
	}
	return out
}

func le64(values ...uint64) []byte {
	out := make([]byte, 0, 8*len(values))
	for _, v := range values {
		out = le.AppendUint64(out, v)
	}
	return out
}

// thin returns the arm64 slice of a (possibly fat) Mach-O with the first of the given CPU subtypes
// that it has.
func thin(data []byte, subtypes ...types.CPUSubtype) ([]byte, error) {
	fat, err := macho.NewFatFile(bytes.NewReader(data))
	if errors.Is(err, macho.ErrNotFat) {
		return data, nil
	}
	if err != nil {
		return nil, err
	}
	size := uint64(len(data))
	for _, subtype := range subtypes {
		for _, a := range fat.Arches {
			if a.CPU == cpuARM64 && a.SubCPU&types.CpuSubtypeMask == subtype && a.Offset64 <= size && a.Size64 <= size-a.Offset64 {
				return data[a.Offset64 : a.Offset64+a.Size64], nil
			}
		}
	}
	return nil, errors.New("no matching arm64 slice in fat Mach-O")
}

type regionKind uint8

const (
	zeroRegion   regionKind = iota // anonymous memory
	fileRegion                     // a private copy of data, or of file from off, read on first touch
	cacheRegion                    // dyld shared cache pages, rebased on first touch
	sharedRegion                   // shared from off is memory of the host's, which other mappings of it share (see shared.go)
)

// region is a reserved range of guest address space; its pages reach Unicorn on first touch.
type region struct {
	start, end uint64
	kind       regionKind
	data       []byte
	file       dyldcache.File
	shared     *sharedMemory
	off        uint64
	name       string
	deny       uint32 // what of protAll its pages may not be accessed for (mprotect)
}

func (r *region) prot() uint32 { return protAll &^ r.deny }

// denied is what a protection does not allow, as a region keeps it.
func denied(prot uint32) uint32 { return protAll &^ (prot & protAll) }

// stop is the panic value that ends a run: the guest did something the emulator cannot carry on
// from. fault is the panic value for a system call touching unmapped guest memory, which the
// call reports to the guest as EFAULT.
type (
	stop struct {
		message string
		kind    StopKind
	}
	fault struct{ message string }
)

func stopf(format string, args ...any) { panic(stop{message: fmt.Sprintf(format, args...)}) }

// stopKind is stopf for a stop whose remedy differs from the rest.
func stopKind(kind StopKind, format string, args ...any) {
	panic(stop{fmt.Sprintf(format, args...), kind})
}

// StopKind tells apart the stops a caller can do something about.
type StopKind int

const (
	StopOther         StopKind = iota
	StopUnimplemented          // a system call or Mach trap the emulator has no handler for
	StopTimeout                // the run's deadline passed while the guest was still executing
	StopNoCache                // the restore image gave no dyld shared cache; Cause says why
	StopIdle                   // every thread waits for a Mach message nothing in the process will send
)

// StopError reports why a guest could not run to completion.
type StopError struct {
	Message string
	Kind    StopKind
	Cause   error // what led to it, when something else did
}

func (e *StopError) Error() string { return e.Message }
func (e *StopError) Unwrap() error { return e.Cause }

// SystemFile is what Config.System knows about a guest path.
type SystemFile struct {
	File dyldcache.File // a regular file, read as the guest reads or touches it
	Dir  bool
	// Real, of a directory reached through symbolic links, is its path without them.
	Real string
	// ReadDir lists the directory's entries when Dir is true and the volume backs it.
	ReadDir func() ([]string, error)
	// Link, of a directory the path's last part only links to, is that symbolic link's text: what
	// lstat and readlink see.
	Link string
	// Inode is what the volume's own record of it says of who owns it and what they may do, if
	// the volume backs it; LinkInode is the same of the symbolic link, where Link is set.
	Inode, LinkInode *Inode
	// Leads, of a symbolic link to what is not on the volume or may not be read there, is the
	// guest path it leads to; Link and LinkInode are then set, and neither Dir nor File.
	Leads string
}

// Inode is the owner and the permission bits (with the sticky and set-id bits) a volume of the
// release has for a file or directory. They are said to whoever asks, and nobody is yet refused
// by them: what a guest may do is the host's to say.
type Inode struct {
	Mode     uint16
	UID, GID uint32
}

// Config describes the machine the guests run on.
type Config struct {
	Unicorn  string           // path of the verified libunicorn (see EnsureUnicorn)
	Cache    *dyldcache.Cache // the build's dyld shared cache
	CacheDir string           // guest directory the cache files appear in
	Home     string           // host directory behind the guest's home (see HomeOf)
	Mounts   []Mount          // more host directories the guests may change (see CheckMounts)
	Dyld     []byte           // the build's /usr/lib/dyld
	Drivers  []Driver         // the kernel's services, which guests reach through IOKit: none unless given
	// Stdout and Stderr, if not nil, are given what the first guest writes to its standard output
	// and error as it writes it, and none of it is kept (Process.Stdout). What the other guests
	// write is always kept.
	Stdout, Stderr io.Writer
	// Launch, if not nil, finds the build's own provider of a Mach service that is looked up and
	// that no guest provides, for launchd to start: nil if the build has none. system says the
	// one who asks is root, and sees only the daemons.
	Launch func(service string, system bool) (*Guest, error)
	// Program reads a program of the build for a guest to start, whatever of the system volume
	// guests may read: nil for a path with no file, and true for a directory.
	Program func(path string) (image []byte, dir bool, err error)
	// DeviceTree reads the device's flattened device tree, which is what the I/O Registry holds;
	// with none, the registry is not there to ask. It is read when a guest first asks.
	DeviceTree func() ([]byte, error)
	// KernelReading is what read makes of the release's kernel and the extensions it starts
	// with, unpacked, which is where the sandbox profiles the kernel has by name are. It may be
	// kept between runs under the name, which says what the reading is and how it is made. It
	// is asked for when a guest first asks for a profile the release has no file for; with
	// none, the kernel has no profile by name.
	KernelReading func(name string, read func(kernelcache []byte) ([]byte, error)) ([]byte, error)
	// System serves read-only files from the build's system volume; a lookup failure stops the run.
	System func(path string) (SystemFile, error)
	OS     map[string]string // "version", "build" and "release" reported by sysctl
	Trace  bool              // print every system call to stderr
	// Survey answers a system call or Mach trap nothing implements with an error (ENOSYS,
	// KERN_INVALID_ARGUMENT) instead of stopping the run, so that one run finds them all.
	Survey bool
}

// Process is one emulated process.
type Process struct {
	m       *Machine
	pid     uint32
	uid     uint32 // the user it runs as, and its group
	gid     uint32
	name    string // of its executable, which is what the process is called
	label   string // what it is called in traces and diagnostics: its name, with its pid if another has the name
	uc      *engine
	cache   *dyldcache.Cache
	dyld    []byte
	mounts  []mounted // the home among them; longest guest path first
	cwd     string    // canonical current working directory
	exe     []byte    // the guest executable
	path    string    // where the guest finds it: only this process does, and no other finds it there
	exePath string    // and where it is on the host, or what it is called if it is not a file there
	// What the executable's code signature says of it, if it has one: the name it was signed
	// under, and the entitlements it was signed with, as a property list and as DER.
	identity, entitlements string
	entitlementsDER        []byte
	system                 func(path string) (SystemFile, error)
	os                     map[string]string
	trace                  bool
	survey                 bool

	regions []*region // sorted by start
	mapped  map[uint64]struct{}
	brk     uint64

	fds      map[uint64]*fd
	nextPort uint32

	exited     bool
	early      bool  // it exited while the guest the run is for was still running
	taskName   *port // its task as those who may only name it have it (see machTaskNameForPid)
	detached   bool  // it had launchd move it to the background's session (see routineSession)
	demanded   bool  // launchd started it when a service of its was first looked up (see onDemand)
	exitCode   int
	endSignal  int             // the signal that ended it, if one did (see signals.go)
	pending    []pendingSignal // the signals no thread has taken yet
	guestFault *pendingSignal  // the fault the thread that has the CPU just made, on its way out of the hook
	crash      string          // of a process a fault ended: what it was, and where
	itimer     intervalTimer   // the timer that sends it SIGALRM, if it set one
	fault      string
	faultKind  StopKind // of the stop that set fault, carried out of the hook it happened in
	resume     bool

	// Threads: see threads.go.
	threads     []*thread
	current     *thread
	nextThread  int
	yield       bool // the running thread offered its turn
	waits       uint64
	semaphores  map[uint32]int
	handoffs    map[uint64]int // mutexes unlocked for a waiter that had not arrived
	kqueues     map[uint64]*kqueue
	rwlocks     map[uint64]*rwlock
	names       map[uint32]*right // the port rights it holds, by the name it has for each (see ports.go)
	named       map[*port]uint32  // the name it has for a port it can receive from or send to
	eventData   uint64            // where a kevent says a message it did not deliver is
	ignored     map[int]int       // kevent filters nothing emulates, and how many changes each was sent
	sigactions  [numSignals]sigAction
	rlimits     map[uint64][2]uint64 // what setrlimit changed, by resource: {soft, hard}
	cmask       uint32               // umask, once changed (cmaskSet)
	cmaskSet    bool
	nice        int64                // setpriority's, of the process
	iopolicies  map[[2]uint32]uint32 // iopolicysys's, by {scope, type}
	threadStart uint64               // libpthread's entry points, and what it said of a pthread
	wqThread    uint64
	pthreadSize uint64
	tsdOffset   uint64
	portOffset  uint64

	log          []string
	unknown      map[unknownMIG]int // how often each was sent
	missing      []*missingCall     // in the order first made
	oids         []string
	sysctls      map[string]any
	dit          uint64
	entropy      int
	stdout       strings.Builder
	stderr       strings.Builder
	usage        usage            // what the process has used of the machine (see usage.go)
	charged      [2]time.Duration // how much of that, as the user and in the kernel, has been counted as a thread's (see charge)
	childrenUsed used             // what the children it has waited for used, and theirs

	// Of a process another started (see spawn.go): which, and whether that one has been told
	// how it ended. ppid is 1, launchd, for a guest of the run's own. stdio is the process whose
	// standard output and error it writes to: itself, or the one it was started under.
	parent  *Process
	pgroup  uint32    // the group it is in, if not one of its own
	session uint32    // the session it is in, if not one of its own
	execed  bool      // it was started as, or has become, another program than its parent
	began   time.Time // when it was made, by the machine's clock
	// The port its own task may be read by (task_read_for_pid of itself), while it has a right
	// to it.
	taskRead   *port
	mustBe     *sameProcess  // of a proc_info that asks of a process by more than its pid (see procctl.go)
	policy     taskPolicy    // what it asked of how it is run (see taskpolicy.go)
	target     *thread       // of a request to a thread's port: the thread
	asked      message       // the request to a port of the kernel's that is being answered
	registered [3]carried    // the ports it keeps for whoever asks (see kernelports.go)
	dirtying   dirtyTracking // what it told the kernel of having work in hand (see procctl.go)
	fdRoom     int           // how many places its table of descriptors has (see procs.go)
	lineage                  // what tells it from every other process there has been (see lineage.go)
	task       *port         // the task's own port, while the process has a right to it (see kernelports.go)
	// Where what it was started with begins, which is the top of its stack from the program's path
	// up, and how many arguments, as the kernel keeps them for whoever asks (kern.procargs2).
	started uint64
	argc    uint32
	ppid    uint32
	reaped  bool
	stdio   *Process
	cloexec map[uint64]bool // the descriptors a program it starts is not to have

	cacheDir   string
	cachePaths map[string]dyldcache.File
	inodes     map[string]uint64
	slide      uint64 // of the shared region, once dyld maps the cache into it
	slideSet   bool
	imageInfo  [2]uint64 // address and size of dyld's __all_image_info, for TASK_DYLD_INFO
}

// unknownMIG is a Mach message nothing handles, and the return address of its sender.
type unknownMIG struct {
	id, port uint32
	lr       uint64
}

// missingCall is a system call or Mach trap with no handler that a survey let the guest carry on
// from: how often it was made, and the return address of the first caller.
type missingCall struct {
	name  string // "syscall 2 fork", "mach trap 90 mach_wait_until"
	count int
	lr    uint64
}

// unimplemented is what a call with no handler comes to: the end of the run, or under a survey
// a count, and true for the caller to answer the guest with an error.
func (p *Process) unimplemented(name string, args []uint64) bool {
	lr := p.uc.reg(regLR)
	if !p.survey {
		stopKind(StopUnimplemented, "unimplemented %s(%s) from %s", name, hexArgs(args[:min(6, len(args))]), p.where(lr))
	}
	i := slices.IndexFunc(p.missing, func(m *missingCall) bool { return m.name == name })
	if i < 0 {
		i = len(p.missing)
		p.missing = append(p.missing, &missingCall{name: name, lr: lr})
	}
	p.missing[i].count++
	p.note("%s(%s) is not implemented", name, hexArgs(args[:min(6, len(args))]))
	return true
}

// Unimplemented lists what the guest asked for that the emulator does not implement and answered
// with an error: system calls and Mach traps (only a survey gets past those) and Mach messages.
// It is empty when there were none.
func (p *Process) Unimplemented() string {
	var out []string
	for _, m := range p.missing {
		out = append(out, fmt.Sprintf("  %4d× %s, first from %s", m.count, m.name, p.where(m.lr)))
	}
	var messages []string
	for u, count := range p.unknown {
		messages = append(messages, fmt.Sprintf("  %4d× mach message %d to port %#x from %s", count, u.id, u.port, p.where(u.lr)))
	}
	for filter, count := range p.ignored {
		messages = append(messages, fmt.Sprintf("  %4d× kevent filter %d: its events never happen", count, filter))
	}
	slices.Sort(messages)
	return strings.Join(append(out, messages...), "\n")
}

// guard turns the panics the emulator uses for control flow into an error.
func guard(err *error) {
	switch r := recover().(type) {
	case nil:
	case stop:
		*err = &StopError{Message: r.message, Kind: r.kind}
	case fault:
		*err = &StopError{Message: r.message}
	default:
		panic(r)
	}
}

// user is who every guest that is not root's runs as, and owns every file of the home and the
// mounts: on macOS the user emu, of the group staff, and on iOS mobile, as on an iPhone.
type user struct {
	name, home, shell string
	uid, gid          uint32
}

var (
	macUser = user{name: "emu", home: GuestHome, shell: "/bin/zsh", uid: guestUID, gid: guestGID}
	iosUser = user{name: "mobile", home: GuestHomeIOS, shell: "/bin/sh", uid: guestUID, gid: guestUID}
)

// userOf is the user of a platform's builds.
func userOf(platform dyldcache.Platform) user {
	if platform == dyldcache.IOS {
		return iosUser
	}
	return macUser
}

// HomeOf is the guest's home directory on a platform: GuestHome, or GuestHomeIOS.
func HomeOf(platform dyldcache.Platform) string { return userOf(platform).home }

// heapStart is where anonymous mappings start for a process of the platform's.
func heapStart(platform dyldcache.Platform) uint64 {
	if platform == dyldcache.IOS {
		return heapBaseIOS
	}
	return heapBase
}

// newProcess gives the machine a process with a stack, the commpage and the MMU set up, ready for
// load.
func (m *Machine) newProcess(executable string, exe []byte, path string) (p *Process, err error) {
	cfg := m.cfg
	m.started++
	p = &Process{m: m, fdRoom: fdRoomAtFirst, pid: guestPID + m.started - 1, uid: m.user.uid, gid: m.user.gid, name: filepath.Base(executable), label: filepath.Base(executable), path: path, mounts: m.mounts, cwd: "/", cache: cfg.Cache, dyld: m.dyld, exe: exe, exePath: executable,
		system: cfg.System, os: cfg.OS, trace: cfg.Trace, survey: cfg.Survey,
		mapped: map[uint64]struct{}{}, brk: heapStart(cfg.Cache.Platform), nextPort: 0x1003, unknown: map[unknownMIG]int{}, semaphores: map[uint32]int{}, handoffs: map[uint64]int{}, kqueues: map[uint64]*kqueue{}, rwlocks: map[uint64]*rwlock{},
		names: map[uint32]*right{}, named: map[*port]uint32{}, ignored: map[int]int{},
		fds:      map[uint64]*fd{0: {kind: stdioFD, host: 0}, 1: {kind: stdioFD, host: 1}, 2: {kind: stdioFD, host: 2}},
		cacheDir: cfg.CacheDir, cachePaths: map[string]dyldcache.File{}, inodes: map[string]uint64{path: exeInode, guestDyld: dyldInode}}
	p.ppid, p.stdio, p.cloexec, p.began = launchdPID, p, map[uint64]bool{}, m.clock.now()
	for suffix, f := range cfg.Cache.Files {
		p.cachePaths[cfg.CacheDir+"/"+CacheName+suffix] = f
	}
	for i, path := range slices.Sorted(maps.Keys(p.cachePaths)) {
		p.inodes[path] = cacheInode + uint64(i)
	}
	if p.uc, err = newEngine(p); err != nil {
		return nil, err
	}
	p.current = &thread{port: mainThreadPort, ctx: p.uc.newContext()}
	p.nameThread(p.current, mainThreadPort)
	p.threads = []*thread{p.current}
	created := p
	defer func() {
		if err != nil {
			created.Close()
			p = nil
		}
	}()
	defer guard(&err)
	p.uc.setReg(regCPACREL1, 3<<20)
	p.addRegion(&region{start: stackTop - stackSize, end: stackTop, name: "stack"})
	p.setupCommpage()
	if err := p.setupMMU(); err != nil {
		return nil, err
	}
	// Two processes of one program are told apart by their pids.
	for _, other := range m.procs {
		if other.label == p.label {
			p.label = fmt.Sprintf("%s.%d", p.label, p.pid)
		}
	}
	m.procs = append(m.procs, p)
	return p, nil
}

// Close releases the engine and any host files the guest left open.
func (p *Process) Close() {
	if p.uc == nil {
		return
	}
	p.forgetRegistered()
	for _, t := range p.threads {
		if t.ctx != 0 {
			p.uc.freeContext(t.ctx)
			t.ctx = 0
		}
	}
	for n, f := range p.fds {
		if f.kind == hostFD {
			p.hostClose(f)
		}
		if f.shared != nil {
			f.shared.release()
		}
		delete(p.fds, n)
	}
	p.uc.close()
	p.uc = nil
}

// Label is what the process is called in traces and diagnostics: the name of its executable.
func (p *Process) Label() string { return p.label }

// ExitCode is the guest's exit status; ok is false if it never exited.
func (p *Process) ExitCode() (code int, ok bool) {
	if p.endSignal != 0 { // as a shell reports a program that a signal ended
		return 128 + p.endSignal, p.exited
	}
	return p.exitCode, p.exited
}

// LeftEarly reports whether a guest that was started beside the main one exited before it did:
// not when it was asked to, at the end, but while it may still have been needed.
func (p *Process) LeftEarly() bool { return p.early }

// OnDemand reports whether the process is a daemon or an agent of the release's that launchd
// started when a service of its was first looked up, and not one the run was given.
func (p *Process) OnDemand() bool { return p.demanded }

// Stdout is everything the guest wrote to file descriptor 1.
func (p *Process) Stdout() string { return p.output().stdout.String() }

// output is the process that holds what p wrote: itself, unless it is a program put in the
// place of the one that was started (execve), which goes on writing where that one did.
func (p *Process) output() *Process {
	if p.parent == nil {
		return p.stdio
	}
	return p
}

// Stderr is everything the guest wrote to file descriptor 2.
func (p *Process) Stderr() string { return p.output().stderr.String() }

// MappedPages is the number of guest pages that were actually touched.
func (p *Process) MappedPages() int { return len(p.mapped) }

// ---- memory ----

func (p *Process) addRegion(r *region) *region {
	i := sort.Search(len(p.regions), func(i int) bool { return p.regions[i].start >= r.start })
	p.regions = slices.Insert(p.regions, i, r)
	if r.shared != nil {
		r.shared.retain()
	}
	return r
}

// regionFrom is the region an address is in, or the first above it.
func (p *Process) regionFrom(addr uint64) *region {
	if i := sort.Search(len(p.regions), func(i int) bool { return p.regions[i].end > addr }); i < len(p.regions) {
		return p.regions[i]
	}
	return nil
}

func (p *Process) findRegion(addr uint64) *region {
	i := sort.Search(len(p.regions), func(i int) bool { return p.regions[i].start > addr }) - 1
	if i >= 0 && addr < p.regions[i].end {
		return p.regions[i]
	}
	return nil
}

func (p *Process) overlaps(start, end uint64) []*region {
	var out []*region
	for _, r := range p.regions {
		if r.start < end && start < r.end {
			out = append(out, r)
		}
	}
	return out
}

func (p *Process) isMapped(addr uint64) bool {
	_, ok := p.mapped[addr/pageSize]
	return ok
}

func (p *Process) unmap(start, end uint64) {
	for _, r := range p.overlaps(start, end) {
		i := slices.Index(p.regions, r)
		p.regions = slices.Delete(p.regions, i, i+1)
		if r.start < start {
			head := *r
			head.end = start
			p.addRegion(&head)
		}
		if r.end > end {
			tail := *r
			tail.start, tail.off = end, r.off+(end-r.start)
			p.addRegion(&tail)
		}
		if r.shared != nil {
			defer r.shared.release() // once the engine no longer maps it
		}
	}
	for a := start; a < end; {
		if !p.isMapped(a) {
			a += pageSize
			continue
		}
		b := a
		p.usage.peak = max(p.usage.peak, len(p.mapped)) // before it has fewer
		for ; b < end && p.isMapped(b); b += pageSize {
			delete(p.mapped, b/pageSize)
		}
		p.uc.memUnmap(a, b-a)
		a = b
	}
}

type allocation struct {
	addr      uint64
	fixed     bool
	mask      uint64
	kind      regionKind
	data      []byte
	file      dyldcache.File // instead of data: read from off as pages are touched
	shared    *sharedMemory  // instead of either: the mapping is this memory, from off
	off       uint64
	name      string
	keepOther bool   // a fixed request fails instead of replacing what is there
	deny      uint32 // see region
}

// alloc reserves guest address space; false when a fixed request cannot be honored.
func (p *Process) alloc(size uint64, a allocation) (uint64, bool) {
	size = align(size, pageSize)
	var start uint64
	if a.fixed {
		if a.addr&tbiMask+size > vaLimit {
			return 0, false
		}
		start = a.addr &^ (pageSize - 1)
		if a.keepOther && len(p.overlaps(start, start+size)) > 0 {
			return 0, false
		}
		p.unmap(start, start+size)
	} else {
		start = align(p.brk, max(a.mask+1, pageSize))
		p.brk = start + size + pageSize
	}
	p.addRegion(&region{start: start, end: start + size, kind: a.kind, data: a.data, file: a.file, shared: a.shared, off: a.off, name: a.name, deny: a.deny})
	return start, true
}

// split makes at, if it is inside a region, the start of one.
func (p *Process) split(at uint64) {
	if r := p.findRegion(at); r != nil && r.start < at {
		tail := *r
		tail.start, tail.off = at, r.off+(at-r.start)
		r.end = at
		p.addRegion(&tail)
	}
}

// protect gives the pages of [start, end) the protection prot (of protAll), as mprotect does;
// false if any of them is not mapped. Pages Unicorn has get it now, the others when touched.
func (p *Process) protect(start, end uint64, prot uint32) bool {
	for a := start; a < end; {
		r := p.findRegion(a)
		if r == nil {
			return false
		}
		a = r.end
	}
	p.split(start)
	p.split(end)
	for _, r := range p.overlaps(start, end) {
		r.deny = denied(prot)
	}
	for a := start; a < end; {
		if !p.isMapped(a) {
			a += pageSize
			continue
		}
		b := a
		for b < end && p.isMapped(b) {
			b += pageSize
		}
		p.uc.memProtect(a, b-a, prot)
		a = b
	}
	p.uc.forgetTranslations()
	return true
}

// materialize gives Unicorn the 16-page block around addr, filled from the region's backing.
func (p *Process) materialize(addr uint64) bool {
	r := p.findRegion(addr)
	if r == nil {
		return false
	}
	b0 := r.start + (addr-r.start)/blockSize*blockSize
	b1 := min(b0+blockSize, r.end)
	// A fault, if it brings anything in: a major one if that is read from a file of the build.
	before, read := len(p.mapped), false
	defer func() {
		if len(p.mapped) > before {
			p.usage.fault(read, len(p.mapped))
		}
	}()
	for a := b0; a < b1; {
		if p.isMapped(a) {
			a += pageSize
			continue
		}
		b := a
		for ; b < b1 && !p.isMapped(b); b += pageSize {
			p.mapped[b/pageSize] = struct{}{}
		}
		if r.kind == sharedRegion {
			o := r.off + (a - r.start)
			p.uc.memMapHost(a, r.shared.data[o:o+(b-a)], r.prot())
			a = b
			continue
		}
		p.uc.memMap(a, b-a, r.prot())
		switch r.kind {
		case cacheRegion:
			read = true
			data, err := away(p.m.clock, func() ([]byte, error) { return p.cache.Page(a-p.slide, b-a, p.slide) })
			if err != nil {
				stopf("shared cache page %#x: %v", a-p.slide, err)
			}
			p.uc.memWrite(a, data)
		case fileRegion:
			if o := r.off + (a - r.start); r.file != nil {
				read = true
				data, err := away(p.m.clock, func() ([]byte, error) { return r.file.Pread(int64(o), int64(b-a)) })
				if err != nil {
					stopf("page in %s: %v", r.name, err)
				}
				p.uc.memWrite(a, data)
			} else if o < uint64(len(r.data)) {
				p.uc.memWrite(a, r.data[o:min(o+(b-a), uint64(len(r.data)))])
			}
		}
		a = b
	}
	return true
}

func (p *Process) unmapped(addr uint64, size int) (ok bool) {
	defer p.hookGuard(func() { ok = false })
	p.usage.enter(p.m.clock, true)
	defer p.usage.enter(p.m.clock, false)
	addr &= tbiMask
	ok = p.materialize(addr)
	if last := addr + uint64(size) - 1; ok && last/pageSize != addr/pageSize {
		ok = p.materialize(last)
	}
	if !ok {
		p.fault = fmt.Sprintf("unmapped access %#x size %d", addr, size)
		p.guestFault = &pendingSignal{sig: sigSegv, code: codeNotMapped, addr: addr, fault: true}
	}
	return ok
}

// protected is an access the page's protection does not allow: the end of the run, as a fault
// would be the end of the process with no handler for its signal.
func (p *Process) protected(kind int32, addr uint64, size int) bool {
	defer p.hookGuard(nil)
	what := map[int32]string{22: "write to", 23: "read of", 24: "instruction fetch from"}[kind]
	p.fault = fmt.Sprintf("%s protected memory %#x size %d (%s)", what, addr, size, p.where(addr))
	p.guestFault = &pendingSignal{sig: sigBus, code: codeNoAccess, addr: addr, fault: true}
	return false
}

func (p *Process) invalid() bool {
	defer p.hookGuard(nil)
	pc := p.uc.reg(regPC)
	p.fault = fmt.Sprintf("invalid instruction %08x at %s", p.u32(pc), p.where(pc))
	return false
}

// hookGuard keeps panics from unwinding through Unicorn: the run stops with the message instead.
func (p *Process) hookGuard(after func()) {
	r := recover()
	if r == nil {
		return
	}
	switch r := r.(type) {
	case stop:
		p.fault, p.faultKind = r.message, r.kind
	case fault:
		p.fault = r.message
	default:
		p.fault = fmt.Sprintf("emulator bug: %v\n%s", r, debug.Stack())
	}
	p.uc.stop()
	if after != nil {
		after()
	}
}

func (p *Process) ensure(addr, n uint64) {
	for a := addr &^ (pageSize - 1); a < addr+max(n, 1); a += pageSize {
		if !p.isMapped(a) && !p.materialize(a) {
			panic(fault{fmt.Sprintf("kernel access to unmapped guest memory %#x", a)})
		}
	}
}

func (p *Process) read(addr, n uint64) []byte {
	if n == 0 {
		return nil
	}
	addr &= tbiMask
	if n > vaLimit || addr+n > vaLimit {
		panic(fault{fmt.Sprintf("kernel access to guest memory %#x+%#x", addr, n)})
	}
	data := make([]byte, n)
	p.readInto(data, addr)
	return data
}

// readInto fills dst with the guest memory at addr.
func (p *Process) readInto(dst []byte, addr uint64) {
	addr &= tbiMask
	if n := uint64(len(dst)); n > vaLimit || addr+n > vaLimit {
		panic(fault{fmt.Sprintf("kernel access to guest memory %#x+%#x", addr, n)})
	}
	p.ensure(addr, uint64(len(dst)))
	if err := p.uc.memReadInto(addr, dst); err != nil {
		panic(fault{fmt.Sprintf("kernel read of guest memory %#x: %v", addr, err)})
	}
}

func (p *Process) write(addr uint64, data []byte) {
	if len(data) == 0 {
		return
	}
	addr &= tbiMask
	if addr+uint64(len(data)) > vaLimit {
		panic(fault{fmt.Sprintf("kernel access to guest memory %#x+%#x", addr, len(data))})
	}
	p.ensure(addr, uint64(len(data)))
	p.uc.memWrite(addr, data)
}

func (p *Process) cstr(addr uint64) string {
	const limit = 4096
	var out []byte
	for len(out) < limit {
		at := addr + uint64(len(out))
		chunk := p.read(at, min(256, pageSize-at&(pageSize-1)))
		if i := bytes.IndexByte(chunk, 0); i >= 0 {
			out = append(out, chunk[:i]...)
			break
		}
		out = append(out, chunk...)
	}
	return strings.ToValidUTF8(string(out), "�")
}

func (p *Process) u32(addr uint64) uint32 { return le.Uint32(p.read(addr, 4)) }
func (p *Process) u64(addr uint64) uint64 { return le.Uint64(p.read(addr, 8)) }

// setupMMU identity-maps the low 16 TB with 1 GB blocks so the core applies top-byte-ignore like
// a real user process; pages still fault in lazily underneath.
func (p *Process) setupMMU() error {
	const l0, n = uint64(pageTables), uint64(vaLimit >> 39)
	size := align((n+2)*4096, pageSize)
	p.uc.memMap(l0, size, protAll)
	p.addRegion(&region{start: l0, end: l0 + size, name: "pagetables"})
	for a := l0; a < l0+size; a += pageSize {
		p.mapped[a/pageSize] = struct{}{}
	}
	top := make([]uint64, n)
	for i := range top {
		top[i] = (l0 + uint64(i+1)*4096) | 3
		blocks := make([]uint64, 512)
		for j := range blocks {
			blocks[j] = (uint64(i)*512+uint64(j))<<30 | 1<<10 | 3<<8 | 1
		}
		p.uc.memWrite(l0+uint64(i+1)*4096, le64(blocks...))
	}
	p.uc.memWrite(l0, le64(top...))
	// Non-secure EL1 with nothing trapping to EL2/EL3 (page walks and pacga need both), then the
	// translation registers, which guest code has to write for the core to pick the regime up.
	p.uc.setCPReg(1, 1, 3, 6, 0, 1|1<<10|3<<16) // SCR_EL3: NS, RW, API, APK
	p.uc.setCPReg(1, 1, 3, 4, 0, 1<<31|3<<40)   // HCR_EL2: RW, API, APK
	stub := l0 + size - 4096
	values := []uint64{
		0xFF, // MAIR_EL1
		l0,   // TTBR0_EL1
		16 | 1<<8 | 1<<10 | 3<<12 | 1<<23 | 4<<32 | 1<<37, // TCR_EL1: 48-bit VA, 44-bit PA, 4K, TBI0
		0xC50838 | 1, // SCTLR_EL1: MMU on, pointer-auth keys off
	}
	// msr mair_el1, x0; msr ttbr0_el1, x1; msr tcr_el1, x2; isb; msr sctlr_el1, x3; isb
	code := []uint32{0xD518A200, 0xD5182001, 0xD5182042, 0xD5033FDF, 0xD5181003, 0xD5033FDF}
	p.uc.memWrite(stub, le32(code...))
	for i, v := range values {
		p.setX(i, v)
	}
	if err := p.uc.start(stub, stub+4*uint64(len(code)), 0); err != nil {
		return fmt.Errorf("MMU setup: %w", err)
	}
	for i := range values {
		p.setX(i, 0)
	}
	return nil
}

// amx says whether an instruction is one of Apple's matrix coprocessor (AMX), which every Apple
// chip of a supported release has and Unicorn does not: 0x00201000, its operation in bits 5 to 9
// and its operand register or immediate in bits 0 to 4.
func amx(instruction uint32) bool { return instruction&^0x3FF == 0x00201000 }

func (p *Process) setupCommpage() {
	for _, base := range []uint64{commPage, commPageRO} {
		page := hostPage(base, pageSize)
		if base == commPage {
			clear(page[0x120:0x148]) // no timeofday fast path: forces the syscall
			// The clock is the machine's (see clock) and nothing else: no offset (the host's own,
			// or that of the Mac a Linux host's page was taken from, would put mach_absolute_time
			// anywhere, before zero included), no time asleep, no approximate time.
			clear(page[0x88:0x90]) // TIMEBASE_OFFSET
			clear(page[0x98:0xB0]) // CONT_TIMEBASE, BOOTTIME_USEC, CONT_HW_TIMEBASE
			le.PutUint64(page[0xA0:], uint64(p.m.clock.boot.UnixMicro()))
			clear(page[0xC0:0xD0])                   // APPROX_TIME and whether it is supported
			page[0x90] = 0                           // USER_TIMEBASE_NONE: mach_absolute_time is a trap, not the counter
			page[0x91] = 0                           // nor does the counter keep mach_continuous_time
			page[0x10C] = 0                          // no TPRO: dyld falls back to mprotect
			if flag := p.m.diagnostic; flag != nil { // as the machine's diagnosticd last set it
				le.PutUint32(page[commPageDiagnostic:], *flag)
			}
			// Hide CPU features the emulated core lacks (DIT, SHA3, SHA512, SPECRES) and report one CPU,
			// in one cluster: malloc sizes its per-cluster tables by _COMM_PAGE_CPU_CLUSTERS, and the
			// host's count with one CPU makes them overlap (cfprefsd faulted in xzone malloc on 26B131).
			caps := le.Uint64(page[0x10:]) &^ (0x0200000000000000 | 0x100000000 | 0x80000000 | 0x0000100000000000)
			caps = caps&^0x00FF0000 | 0x00010000
			le.PutUint64(page[0x10:], caps)
			le.PutUint32(page[0x20:], uint32(caps))
			page[0x22], page[0x2F], page[0x34], page[0x35], page[0x36] = 1, 1, 1, 1, 1
			// The chip is the guest's, as hw.cpufamily says, not the host's: libraries that pick
			// their kernels by chip (libBLAS) read it here.
			family, _ := p.sysctlValue("hw.cpufamily")
			n, _ := family.(uint64)              // none, CPUFAMILY_UNKNOWN, for a product of unknown chip
			le.PutUint32(page[0x80:], uint32(n)) // _COMM_PAGE_CPUFAMILY
		}
		p.uc.memMap(base, pageSize, protAll)
		p.uc.memWrite(base, page)
		p.mapped[base/pageSize] = struct{}{}
		p.addRegion(&region{start: base, end: base + pageSize, name: "commpage"})
	}
}

// ---- registers ----

func (p *Process) x(i int) uint64 { return p.uc.reg(regX0 + i) }

// args is x0…x7, the argument registers of a system call or trap.
func (p *Process) args() []uint64 {
	args := make([]uint64, 8)
	for i := range args {
		args[i] = p.x(i)
	}
	return args
}

func (p *Process) setX(i int, v uint64)     { p.uc.setReg(regX0+i, v) }
func (p *Process) xr(i uint32) uint64       { return p.uc.reg(xrID(i)) }
func (p *Process) setXR(i uint32, v uint64) { p.uc.setReg(xrID(i), v) }

// xrID is the register id of x0…x30 as numbered in an instruction encoding.
func xrID(i uint32) int {
	switch i {
	case 29:
		return regX29
	case 30:
		return regLR
	}
	return regX0 + int(i)
}

func (p *Process) carry(on bool) {
	v := p.uc.reg(regNZCV)
	if on {
		v |= 1 << 29
	} else {
		v &^= 1 << 29
	}
	p.uc.setReg(regNZCV, v)
}

// where describes a guest address for messages: region or cache image, and offset.
func (p *Process) where(addr uint64) string {
	r := p.findRegion(addr)
	if r == nil {
		return fmt.Sprintf("%#x (?)", addr)
	}
	if r.kind != cacheRegion {
		return fmt.Sprintf("%#x (%s+%#x)", addr, r.name, addr-r.start)
	}
	addr -= p.slide
	type image struct {
		path   string
		offset uint64
	}
	switch in, err := away(p.m.clock, func() (image, error) { // its name is read from the cache
		path, offset, err := p.cache.Image(addr)
		return image{path, offset}, err
	}); {
	case err != nil:
		return fmt.Sprintf("%#x (cache)", addr)
	case in.path != "":
		return fmt.Sprintf("%#x (%s+%#x)", addr, in.path[strings.LastIndexByte(in.path, '/')+1:], in.offset)
	}
	return fmt.Sprintf("%#x (cache data)", addr)
}

// Diagnostics describes a failed run: recent system calls, guest output and a backtrace.
func (p *Process) Diagnostics() string {
	const lines = 20
	var out []string
	for _, l := range p.log[max(0, len(p.log)-lines):] {
		out = append(out, "  "+l)
	}
	if text := strings.TrimRight(p.stderr.String(), "\n"); text != "" {
		stderr := strings.Split(text, "\n")
		for _, l := range stderr[max(0, len(stderr)-8):] {
			out = append(out, "  guest stderr: "+l)
		}
	}
	if len(p.threads) > 1 || p.current.state != runnable {
		out = append(out, fmt.Sprintf("  threads (the backtrace is of thread %d):", p.current.id), p.threadList())
	}
	out = append(out, "  backtrace (cache addresses):")
	for _, a := range p.backtrace() {
		out = append(out, "    "+p.where(a))
	}
	var unknown []string
	for u := range p.unknown {
		unknown = append(unknown, fmt.Sprintf("  unhandled: mig %d port %#x from %s", u.id, u.port, p.where(u.lr)))
	}
	slices.Sort(unknown)
	out = append(out, unknown...)
	return strings.Join(out, "\n")
}

func (p *Process) backtrace() []uint64 {
	out := []uint64{p.uc.reg(regPC), p.uc.reg(regLR)}
	fp := p.uc.reg(regX29)
	for i := 0; i < 24 && fp != 0 && p.isMapped(fp) && p.isMapped(fp+15); i++ {
		frame, err := p.uc.memRead(fp, 16)
		if err != nil {
			break
		}
		fp = le.Uint64(frame)
		out = append(out, le.Uint64(frame[8:])&0xFFFFFFFFFFFF)
	}
	return out
}

// ---- loading ----

// loadMachO maps a Mach-O's segments the way the kernel does. base, if not zero, is where its
// lowest segment goes. It returns the slide and the LC_UNIXTHREAD entry point, if any.
func (p *Process) loadMachO(data []byte, name string, base uint64) (f *macho.File, slide, entry uint64) {
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		stopf("%s: %v", name, err)
	}
	segments := f.Segments()
	lowest := ^uint64(0)
	for _, s := range segments {
		if s.Name != "__PAGEZERO" {
			lowest = min(lowest, s.Addr)
		}
	}
	if base != 0 {
		slide = base - lowest
	}
	for _, s := range segments {
		if s.Name == "__PAGEZERO" || s.Memsz == 0 {
			continue
		}
		start := min(s.Offset, uint64(len(data)))
		end := min(start+s.Filesz, uint64(len(data)))
		p.alloc(s.Memsz, allocation{addr: s.Addr + slide, fixed: true, kind: fileRegion, data: data[start:end], name: name + ":" + s.Name})
	}
	for _, l := range f.Loads {
		if t, ok := l.(*macho.UnixThread); ok && len(t.Threads) > 0 && len(t.Threads[0].Data) >= 33*8 {
			if pc := le.Uint64(t.Threads[0].Data[32*8:]); pc != 0 { // arm_thread_state64
				entry = pc + slide
			}
		}
	}
	return f, slide, entry
}

// load maps the guest's executable and the build's dyld, and leaves the first thread where the
// kernel leaves a new process: at dyld's entry point, with its arguments and environment on the
// stack.
func (p *Process) load(argv, env []string) error {
	// A guest built for this is arm64. What Apple ships has only arm64e, which runs the same
	// here: the keys are off, as dyld is told below.
	exe, err := thin(p.exe, 0, cpuSubtypeARM64E)
	if err != nil {
		return fmt.Errorf("guest executable: %w", err)
	}
	cdhash := ""
	image, _, _ := p.loadMachO(exe, "guest", 0)
	if u := image.UUID(); u != nil {
		p.uuid = u.UUID
	}
	if image.CodeSignature() != nil {
		signed := image.CodeSignature()
		rank := -1
		for _, directory := range signed.CodeDirectories {
			p.identity = directory.ID
			// The kernel's hash of the executable is that of its code directory of the strongest
			// hash it knows, cut to 20 bytes. dyld checks it against the launch closures the
			// shared cache has for the system's own executables, and uses one only if it matches.
			if r := slices.Index([]uint8{1, 3, 2, 4}, uint8(directory.Header.HashType)); r > rank && len(directory.CDHash) >= 40 {
				if _, err := hex.DecodeString(directory.CDHash); err == nil {
					rank, cdhash = r, directory.CDHash[:40]
				}
			}
		}
		p.entitlements, p.entitlementsDER = signed.Entitlements, signed.EntitlementsDER
	}
	// The kernel's own strings, in a Mac's order where it has them: those the libraries erase
	// once read (the guard, the entropy, and the first thread's port) before others that stay.
	apple := []string{"executable_path=" + p.path, "", "", "", "stack_guard=0x5a17c0de5a17c000", "malloc_entropy=0x1122334455667788,0x8877665544332211", "ptr_munge=0x7a3c5e9b1d2f4a68",
		fmt.Sprintf("main_stack=%#x,%#x,%#x,%#x", uint64(stackTop), uint64(stackSize), uint64(stackTop), uint64(stackSize)),
		fmt.Sprintf("executable_file=%#x,%#x", uint64(fsTypeAPFS<<32|deviceID), uint64(exeInode)),
		fmt.Sprintf("dyld_file=%#x,%#x", uint64(fsTypeAPFS<<32|deviceID), uint64(dyldInode))}
	if cdhash != "" { // an executable with no code signature has none
		apple = append(apple, "executable_cdhash="+cdhash)
	}
	apple = append(apple, "executable_boothash="+strings.Repeat("22", 20), fmt.Sprintf("th_port=%#x", mainThreadPort), "arm64e_abi=os", "ptrauth_disabled=1", "dyld_flags=0x0")
	// The strings are at the top of the stack, as a Mac's kernel lays them (examples/stacktop.c):
	// the first of its own, which names the program, the arguments, the environment and the
	// rest of its own, each run of them starting a multiple of 8 above the first and the last
	// ending at the top. Below them are the pointers to them.
	var text []byte
	offsets := make([][]uint64, 3) // of the arguments, the environment and the kernel's strings
	add := func(to int, strings ...string) {
		for _, s := range strings {
			offsets[to] = append(offsets[to], uint64(len(text)))
			text = append(append(text, s...), 0)
		}
	}
	pad := func() { text = append(text, make([]byte, -len(text)&7)...) }
	add(2, apple[0])
	pad()
	add(0, argv...)
	add(1, env...)
	pad()
	add(2, apple[1:]...)
	pad()
	sbase := stackTop - uint64(len(text))
	p.started, p.argc = sbase+uint64(len(apple[0])-len(p.path)), uint32(len(argv)) // from the path in the first string up
	words := []uint64{exeBase, uint64(len(argv))}
	for _, o := range offsets {
		for _, i := range o {
			words = append(words, sbase+i)
		}
		words = append(words, 0)
	}
	sp := (sbase - uint64(len(words))*8) &^ 15
	p.write(sbase, text)
	p.write(sp, le64(words...))
	p.uc.setReg(regSP, sp)
	dyld, slide, pc := p.loadMachO(p.dyld, "dyld", dyldBase)
	for _, segment := range []string{"__DATA", "__DATA_DIRTY"} {
		if s := dyld.Section(segment, "__all_image_info"); s != nil && s.Addr+slide != 0 {
			p.imageInfo = [2]uint64{s.Addr + slide, s.Size}
			break
		}
	}
	p.current.pc = pc
	return nil
}

// step runs the thread that has the turn until it gives it up, the process exits, or the run
// has to stop. False is a run that is over: the guest it was for has exited, and this process
// used up the time the others had left.
func (p *Process) step() bool {
	// One uninterrupted emu_start per stretch: resuming after a stop that came from outside it is
	// not reliable, so the only such stop is for the overall deadline and hitting it is fatal,
	// whatever else happened in that stretch. Stops requested from inside a hook (exit, an emulated
	// instruction) are safe to resume from. The deadline is one of the machine's clock, which
	// stands still while the host fetches what a hook asked for, and Unicorn's own timeout counts
	// the host's: so the clock's alarm stops the engine instead, as Unicorn's timer would.
	p.resume = false
	limit, settling := p.m.deadline, p.m.procs[0].exited
	if settling && p.m.settling.Before(limit) {
		limit = p.m.settling
	}
	off := p.m.clock.alarm(limit, p.uc.stop)
	p.usage.start(p.m.clock)
	p.m.stepping = true
	err := p.uc.start(p.current.pc, 0, 0)
	p.m.stepping = false
	p.usage.stop(p.m.clock)
	timedOut := off()
	pc := p.uc.reg(regPC)
	if s := p.guestFault; s != nil && !timedOut {
		// A fault of the guest's own: a signal to the thread that made it, as a Mac's kernel
		// makes of one.
		p.guestFault, p.fault = nil, ""
		p.faulted(*s, pc)
		return true
	}
	p.guestFault = nil
	if err != nil {
		reason := p.fault
		if reason == "" {
			reason = "no hook reported a cause"
		}
		stopf("%v: %s at %s lr %s", err, reason, p.where(pc), p.where(p.uc.reg(regLR)))
	}
	if timedOut && settling {
		return false
	}
	if timedOut {
		stopKind(StopTimeout, "timed out at %s lr %s", p.where(pc), p.where(p.uc.reg(regLR)))
	}
	if p.fault != "" {
		stopKind(p.faultKind, "%s", p.fault)
	}
	if !p.exited && !p.resume {
		stopf("emulation stopped without a cause at %s lr %s", p.where(pc), p.where(p.uc.reg(regLR)))
	}
	p.current.pc = pc
	return true
}

// ---- traps ----

func (p *Process) interrupt(number uint32) {
	defer p.hookGuard(nil)
	p.usage.enter(p.m.clock, true)
	defer p.usage.enter(p.m.clock, false)
	if number == 1 && p.undefined() {
		p.resume = true
		p.uc.stop()
		return
	}
	if number != 2 {
		pc := p.uc.reg(regPC)
		words, instruction := "?", uint32(0)
		func() {
			defer func() { recover() }()
			instruction = p.u32(pc)
			w := p.read(pc-8, 16)
			words = fmt.Sprintf("%08x %08x %08x %08x", le.Uint32(w), le.Uint32(w[4:]), le.Uint32(w[8:]), le.Uint32(w[12:]))
		}()
		p.fault = fmt.Sprintf("CPU exception %d at %s (code around pc: %s)", number, p.where(pc), words)
		if breakpoint(instruction) {
			p.guestFault = &pendingSignal{sig: sigTrap, code: codeBreakpoint, addr: pc, fault: true}
		}
		if number == 1 && amx(instruction) {
			p.fault = fmt.Sprintf("AMX instruction %08x at %s: Accelerate's BLAS and LAPACK need Apple's matrix coprocessor, which is not emulated",
				instruction, p.where(pc))
			p.faultKind = StopUnimplemented
		}
		p.uc.stop()
		return
	}
	switch n := int64(p.uc.reg(regX0 + 16)); {
	case uint32(n) == 0xFFFFFFFD, uint32(n) == 0xFFFFFFFC:
		// The fast traps -3 and -4, mach_absolute_time and mach_continuous_time: the same clock,
		// as no time is spent asleep. They are made too often to be worth a line in the trace.
		p.setX(0, p.m.clock.ticks())
	case n > 0 && n&0xFFFFFFFF == 0x80000000:
		p.machdep()
	case n < 0:
		p.machTrap(-n)
	default:
		p.bsd(n)
	}
	if !p.exited && !p.reschedule() {
		p.takeSignals()
	}
	if p.exited || p.reschedule() {
		p.resume = true
		p.uc.stop()
	}
}

// undefined emulates instructions the core refuses. Keys are off (as in any arm64 process running
// arm64e system code) so pac*/aut* are no-ops, but pacga is never gated and the core traps it to a
// hypervisor that is not there: compute a stand-in generic MAC instead.
func (p *Process) undefined() bool {
	pc := p.uc.reg(regPC)
	insn := p.u32(pc)
	rt := insn & 31
	switch {
	case insn&0xFFE0FC00 == 0x9AC03000: // pacga xd, xn, xm|sp
		var n, m uint64
		if rn := (insn >> 5) & 31; rn != 31 {
			n = p.xr(rn)
		}
		if rm := (insn >> 16) & 31; rm == 31 {
			m = p.uc.reg(regSP)
		} else {
			m = p.xr(rm)
		}
		if rt != 31 {
			p.setXR(rt, (n*0x9E3779B97F4A7C15^m*0xC2B2AE3D27D4EB4F^n>>29)&0xFFFFFFFF00000000)
		}
	// PSTATE.DIT (data-independent timing), which some corecrypto builds use unconditionally.
	case insn&0xFFFFFFE0 == 0xD53B42A0: // mrs xt, dit
		if rt != 31 {
			p.setXR(rt, p.dit<<24)
		}
	case insn&0xFFFFFFE0 == 0xD51B42A0: // msr dit, xt
		p.dit = 0
		if rt != 31 {
			p.dit = p.xr(rt) >> 24 & 1
		}
	case insn&0xFFFFF0FF == 0xD503405F: // msr dit, #imm
		p.dit = uint64(insn >> 8 & 1)
	default:
		return false
	}
	p.uc.setReg(regPC, pc+4)
	return true
}

func (p *Process) machdep() {
	switch selector := p.x(3); selector {
	case 2: // set the thread-specific data base
		p.current.tsd = p.x(0)
		p.uc.setReg(regTPIDRRO, p.current.tsd)
		p.setX(0, 0)
	case 3:
		p.setX(0, p.current.tsd)
	default:
		stopf("machdep call %d", selector)
	}
}

func (p *Process) note(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if len(p.threads) > 1 {
		s = "[" + strconv.Itoa(p.current.id) + "] " + s
	}
	if p.m != nil && len(p.m.procs) > 1 {
		s = "[" + p.label + "] " + s
	}
	p.log = append(p.log, s)
	if len(p.log) > 400 {
		p.log = append(p.log[:0], p.log[200:]...)
	}
	if p.trace {
		fmt.Fprintln(os.Stderr, "  "+s)
	}
}

func hexArgs(args []uint64) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = fmt.Sprintf("%#x", a)
	}
	return strings.Join(parts, ", ")
}
