// Package emulator is a minimal user-mode Darwin "kernel" for arm64e on Unicorn.
//
// It starts a macOS build's own /usr/lib/dyld the way XNU would, lets it map that build's dyld
// shared cache, and pages cache contents in on first touch. System calls and Mach traps are
// implemented just far enough for libSystem, CoreFoundation, Foundation, the Objective-C and
// Swift runtimes, Security and SQLite to run single-threaded with no daemons around; anything
// unimplemented stops the run with a message rather than guessing.
package emulator

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/KishanBagaria/ipswrun/dyldcache"
	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/types"
)

const (
	pageSize   = dyldcache.PageSize
	blockSize  = 16 * pageSize
	commPage   = 0xFFFFFC000
	commPageRO = 0xFFFFF4000
	pageTables = 0x0F0000000000
	vaLimit    = 1 << 44 // the emulated core's physical address size; identity mapped
	tbiMask    = 1<<56 - 1
	stackTop   = 0x170000000
	stackSize  = 8 << 20
	exeBase    = 0x100000000
	dyldBase   = 0x120000000
	heapBase   = 0x20000000000 // anonymous mappings grow up from 2 TB, clear of anything the guest places itself
	deviceID   = 0x1000004

	// GuestHome is the emulated user's home directory; it is backed by Config.Home on the host.
	GuestHome = "/Users/emu"
	// GuestExe is the path the guest executable has inside the guest.
	GuestExe    = "/emu/guest"
	guestDyld   = "/usr/lib/dyld"
	cryptex     = "/System/Volumes/Preboot/Cryptexes/OS"
	cryptexLink = "/System/Cryptexes/OS"
	exeInode    = 0x5105F2A
	dyldInode   = 0x5105F2B
	cacheInode  = 0x5105F30

	// The one process, user and machine the guest sees, however it asks.
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
	zeroRegion  regionKind = iota // anonymous memory
	fileRegion                    // a private copy of data, or of file from off, read on first touch
	cacheRegion                   // dyld shared cache pages, rebased on first touch
)

// region is a reserved range of guest address space; its pages reach Unicorn on first touch.
type region struct {
	start, end uint64
	kind       regionKind
	data       []byte
	file       dyldcache.File
	off        uint64
	name       string
}

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
}

// Config describes one guest process.
type Config struct {
	Unicorn    string           // path of the verified libunicorn (see EnsureUnicorn)
	Cache      *dyldcache.Cache // the build's dyld shared cache
	CacheDir   string           // guest directory the cache files appear in
	Home       string           // host directory behind GuestHome
	Mounts     []Mount          // more host directories the guest may change (see CheckMounts)
	Executable string           // host path of the guest executable
	Dyld       []byte           // the build's /usr/lib/dyld
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
	uc      *engine
	cache   *dyldcache.Cache
	dyld    []byte
	mounts  []Mount // the home among them; longest guest path first
	exe     []byte  // the guest executable
	exePath string  // and where it is on the host
	system  func(path string) (SystemFile, error)
	os      map[string]string
	trace   bool
	survey  bool

	regions []*region // sorted by start
	mapped  map[uint64]struct{}
	brk     uint64

	fds      map[uint64]*fd
	nextFD   uint64
	nextPort uint32

	exited    bool
	exitCode  int
	fault     string
	faultKind StopKind // of the stop that set fault, carried out of the hook it happened in
	resume    bool
	deadline  time.Time

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
	contexts    map[uint32]uint64 // of ports made with one
	ignored     map[int]int       // kevent filters nothing emulates, and how many changes each was sent
	threadStart uint64            // libpthread's entry points, and what it said of a pthread
	wqThread    uint64
	pthreadSize uint64
	tsdOffset   uint64
	portOffset  uint64

	log     []string
	unknown map[unknownMIG]int // how often each was sent
	missing []*missingCall     // in the order first made
	oids    []string
	sysctls map[string]any
	dit     uint64
	entropy int
	stdout  strings.Builder
	stderr  strings.Builder

	cacheDir   string
	cachePaths map[string]dyldcache.File
	inodes     map[string]uint64
	seen       map[fileID]string // every file the guest has looked at, for fsgetpath
	slide      uint64            // of the shared region, once dyld maps the cache into it
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
		stopKind(StopUnimplemented, "unimplemented %s(%s) from %s", name, hexArgs(args[:6]), p.where(lr))
	}
	i := slices.IndexFunc(p.missing, func(m *missingCall) bool { return m.name == name })
	if i < 0 {
		i = len(p.missing)
		p.missing = append(p.missing, &missingCall{name: name, lr: lr})
	}
	p.missing[i].count++
	p.note("%s(%s) is not implemented", name, hexArgs(args[:6]))
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

// New creates a process with a stack, the commpage and the MMU set up, ready for Start.
func New(cfg Config) (p *Process, err error) {
	if err := loadUnicorn(cfg.Unicorn); err != nil {
		return nil, err
	}
	dyld, err := thin(cfg.Dyld, cpuSubtypeARM64E)
	if err != nil {
		return nil, fmt.Errorf("dyld: %w", err)
	}
	exe, err := os.ReadFile(cfg.Executable)
	if err != nil {
		return nil, err
	}
	if err := CheckMounts(cfg.Mounts); err != nil {
		return nil, err
	}
	mounts := append([]Mount{{GuestHome, cfg.Home}}, cfg.Mounts...)
	for i, m := range mounts {
		host, err := filepath.EvalSymlinks(m.Host)
		if err != nil {
			return nil, err
		}
		mounts[i] = Mount{canonical(m.Guest), host}
	}
	slices.SortStableFunc(mounts, func(a, b Mount) int { return len(b.Guest) - len(a.Guest) })
	p = &Process{mounts: mounts, cache: cfg.Cache, dyld: dyld, exe: exe, exePath: cfg.Executable, system: cfg.System, os: cfg.OS, trace: cfg.Trace, survey: cfg.Survey,
		mapped: map[uint64]struct{}{}, brk: heapBase, nextFD: 3, nextPort: 0x1003, unknown: map[unknownMIG]int{}, semaphores: map[uint32]int{}, handoffs: map[uint64]int{}, kqueues: map[uint64]*kqueue{}, rwlocks: map[uint64]*rwlock{}, contexts: map[uint32]uint64{}, ignored: map[int]int{},
		fds:      map[uint64]*fd{0: {kind: hostFD, host: 0}, 1: {kind: hostFD, host: 1}, 2: {kind: hostFD, host: 2}},
		cacheDir: cfg.CacheDir, cachePaths: map[string]dyldcache.File{}, inodes: map[string]uint64{GuestExe: exeInode, guestDyld: dyldInode}, seen: map[fileID]string{}}
	for _, v := range volumes { // their roots, for fsgetpath
		p.seen[fileID{v.dev, rootInode}] = v.mount
	}
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
	return p, nil
}

// Close releases the engine and any host files the guest left open.
func (p *Process) Close() {
	for _, t := range p.threads {
		if t.ctx != 0 {
			p.uc.freeContext(t.ctx)
			t.ctx = 0
		}
	}
	for n, f := range p.fds {
		if f.kind == hostFD && f.host > 2 {
			closeHost(f.host)
		}
		delete(p.fds, n)
	}
	p.uc.close()
}

// ExitCode is the guest's exit status; ok is false if it never exited.
func (p *Process) ExitCode() (code int, ok bool) { return p.exitCode, p.exited }

// Stdout is everything the guest wrote to file descriptor 1.
func (p *Process) Stdout() string { return p.stdout.String() }

// Stderr is everything the guest wrote to file descriptor 2.
func (p *Process) Stderr() string { return p.stderr.String() }

// MappedPages is the number of guest pages that were actually touched.
func (p *Process) MappedPages() int { return len(p.mapped) }

// ---- memory ----

func (p *Process) addRegion(r *region) *region {
	i := sort.Search(len(p.regions), func(i int) bool { return p.regions[i].start >= r.start })
	p.regions = slices.Insert(p.regions, i, r)
	return r
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
	}
	for a := start; a < end; {
		if !p.isMapped(a) {
			a += pageSize
			continue
		}
		b := a
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
	off       uint64
	name      string
	keepOther bool // a fixed request fails instead of replacing what is there
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
	p.addRegion(&region{start: start, end: start + size, kind: a.kind, data: a.data, file: a.file, off: a.off, name: a.name})
	return start, true
}

// materialize gives Unicorn the 16-page block around addr, filled from the region's backing.
func (p *Process) materialize(addr uint64) bool {
	r := p.findRegion(addr)
	if r == nil {
		return false
	}
	b0 := r.start + (addr-r.start)/blockSize*blockSize
	b1 := min(b0+blockSize, r.end)
	for a := b0; a < b1; {
		if p.isMapped(a) {
			a += pageSize
			continue
		}
		b := a
		for ; b < b1 && !p.isMapped(b); b += pageSize {
			p.mapped[b/pageSize] = struct{}{}
		}
		p.uc.memMap(a, b-a)
		switch r.kind {
		case cacheRegion:
			data, err := p.cache.Page(a-p.slide, b-a, p.slide)
			if err != nil {
				stopf("shared cache page %#x: %v", a-p.slide, err)
			}
			p.uc.memWrite(a, data)
		case fileRegion:
			if o := r.off + (a - r.start); r.file != nil {
				data, err := r.file.Pread(int64(o), int64(b-a))
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
	addr &= tbiMask
	ok = p.materialize(addr)
	if last := addr + uint64(size) - 1; ok && last/pageSize != addr/pageSize {
		ok = p.materialize(last)
	}
	if !ok {
		p.fault = fmt.Sprintf("unmapped access %#x size %d", addr, size)
	}
	return ok
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
	p.ensure(addr, n)
	data, err := p.uc.memRead(addr, int(n))
	if err != nil {
		panic(fault{fmt.Sprintf("kernel read of guest memory %#x: %v", addr, err)})
	}
	return data
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
	p.uc.memMap(l0, size)
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
	if _, err := p.uc.start(stub, stub+4*uint64(len(code)), 0); err != nil {
		return fmt.Errorf("MMU setup: %w", err)
	}
	for i := range values {
		p.setX(i, 0)
	}
	return nil
}

// The guest's clock is the host's uptime in the ticks of an Apple silicon Mac: 24 MHz, which
// mach_timebase_info reports as 125/3 ns a tick and newer system libraries take for granted. The
// emulated CPU's own counter runs at another rate, so the commpage tells libsystem not to read
// it and to ask the kernel instead (see interrupt).

// machTicks is the guest's mach_absolute_time now.
func machTicks() uint64 { return uptimeNanos() * 3 / 125 }

// tickDuration is how long a number of ticks lasts.
func tickDuration(ticks int64) time.Duration { return time.Duration(ticks * 125 / 3) }

// tickTime is when the guest's mach_absolute_time is, or was, ticks.
func tickTime(ticks uint64) time.Time {
	return time.Now().Add(tickDuration(int64(ticks - machTicks())))
}

func (p *Process) setupCommpage() {
	for _, base := range []uint64{commPage, commPageRO} {
		page := hostPage(base, pageSize)
		if base == commPage {
			clear(page[0x120:0x148]) // no timeofday fast path: forces the syscall
			// The clock is machTicks and nothing else: no offset (the host's own, or that of the
			// Mac a Linux host's page was taken from, would put mach_absolute_time anywhere,
			// before zero included), no time asleep, no approximate time.
			clear(page[0x88:0x90]) // TIMEBASE_OFFSET
			clear(page[0x98:0xB0]) // CONT_TIMEBASE, BOOTTIME_USEC, CONT_HW_TIMEBASE
			le.PutUint64(page[0xA0:], uint64(time.Now().UnixMicro())-uptimeNanos()/1000)
			clear(page[0xC0:0xD0]) // APPROX_TIME and whether it is supported
			page[0x90] = 0         // USER_TIMEBASE_NONE: mach_absolute_time is a trap, not the counter
			page[0x91] = 0         // nor does the counter keep mach_continuous_time
			page[0x10C] = 0        // no TPRO: dyld falls back to mprotect
			// Hide CPU features the emulated core lacks (DIT, SHA3, SHA512, SPECRES) and report one CPU.
			caps := le.Uint64(page[0x10:]) &^ (0x0200000000000000 | 0x100000000 | 0x80000000 | 0x0000100000000000)
			caps = caps&^0x00FF0000 | 0x00010000
			le.PutUint64(page[0x10:], caps)
			le.PutUint32(page[0x20:], uint32(caps))
			page[0x22], page[0x34], page[0x35], page[0x36] = 1, 1, 1, 1
		}
		p.uc.memMap(base, pageSize)
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
	switch path, offset, err := p.cache.Image(addr); {
	case err != nil:
		return fmt.Sprintf("%#x (cache)", addr)
	case path != "":
		return fmt.Sprintf("%#x (%s+%#x)", addr, path[strings.LastIndexByte(path, '/')+1:], offset)
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

// Start runs the guest with the given arguments and environment until it exits. Any other end
// (a fault, something unimplemented, the timeout) is a *StopError.
func (p *Process) Start(argv, env []string, timeout time.Duration) (err error) {
	defer guard(&err)
	// A guest built for this is arm64. What Apple ships has only arm64e, which runs the same
	// here: the keys are off, as dyld is told below.
	exe, err := thin(p.exe, 0, cpuSubtypeARM64E)
	if err != nil {
		return fmt.Errorf("guest executable: %w", err)
	}
	p.loadMachO(exe, "guest", 0)
	apple := []string{"executable_path=" + GuestExe, "", "", "", "ptr_munge=0x7a3c5e9b1d2f4a68",
		fmt.Sprintf("main_stack=%#x,%#x,%#x,%#x", uint64(stackTop), uint64(stackSize), uint64(stackTop), uint64(stackSize)),
		fmt.Sprintf("executable_file=%#x,%#x", uint64(fsTypeAPFS<<32|deviceID), uint64(exeInode)),
		fmt.Sprintf("dyld_file=%#x,%#x", uint64(fsTypeAPFS<<32|deviceID), uint64(dyldInode)),
		"executable_cdhash=" + strings.Repeat("11", 20), "executable_boothash=" + strings.Repeat("22", 20), "arm64e_abi=os", fmt.Sprintf("th_port=%#x", mainThreadPort),
		"ptrauth_disabled=1", "dyld_flags=0x0", "stack_guard=0x5a17c0de5a17c000", "malloc_entropy=0x1122334455667788,0x8877665544332211"}
	var text []byte
	var offsets [][]uint64
	for _, group := range [][]string{argv, env, apple} {
		var o []uint64
		for _, s := range group {
			o = append(o, uint64(len(text)))
			text = append(append(text, s...), 0)
		}
		offsets = append(offsets, o)
	}
	sbase := stackTop - align(uint64(len(text))+64, 16)
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
	p.deadline = time.Now().Add(timeout)
	dyld, slide, pc := p.loadMachO(p.dyld, "dyld", dyldBase)
	for _, segment := range []string{"__DATA", "__DATA_DIRTY"} {
		if s := dyld.Section(segment, "__all_image_info"); s != nil && s.Addr+slide != 0 {
			p.imageInfo = [2]uint64{s.Addr + slide, s.Size}
			break
		}
	}
	p.run(pc)
	return nil
}

func (p *Process) run(pc uint64) {
	// One uninterrupted emu_start per stretch: resuming after Unicorn's own timeout fires is not
	// reliable, so the timeout is only ever the overall deadline and hitting it is fatal, whatever
	// else happened in that stretch. Stops requested from inside a hook (exit, an emulated
	// instruction) are safe to resume from.
	for !p.exited {
		p.resume = false
		left := max(time.Until(p.deadline).Microseconds(), 1)
		timedOut, err := p.uc.start(pc, 0, uint64(left))
		if err != nil {
			reason := p.fault
			if reason == "" {
				reason = "no hook reported a cause"
			}
			pc = p.uc.reg(regPC)
			stopf("%v: %s at %s lr %s", err, reason, p.where(pc), p.where(p.uc.reg(regLR)))
		}
		pc = p.uc.reg(regPC)
		if timedOut {
			stopKind(StopTimeout, "timed out at %s lr %s", p.where(pc), p.where(p.uc.reg(regLR)))
		}
		if p.fault != "" {
			stopKind(p.faultKind, "%s", p.fault)
		}
		if !p.exited && !p.resume {
			stopf("emulation stopped without a cause at %s lr %s", p.where(pc), p.where(p.uc.reg(regLR)))
		}
		if !p.exited && p.reschedule() {
			pc = p.schedule(pc)
		}
	}
}

// ---- traps ----

func (p *Process) interrupt(number uint32) {
	defer p.hookGuard(nil)
	if number == 1 && p.undefined() {
		p.resume = true
		p.uc.stop()
		return
	}
	if number != 2 {
		pc := p.uc.reg(regPC)
		words := "?"
		func() {
			defer func() { recover() }()
			w := p.read(pc-8, 16)
			words = fmt.Sprintf("%08x %08x %08x %08x", le.Uint32(w), le.Uint32(w[4:]), le.Uint32(w[8:]), le.Uint32(w[12:]))
		}()
		p.fault = fmt.Sprintf("CPU exception %d at %s (code around pc: %s)", number, p.where(pc), words)
		p.uc.stop()
		return
	}
	switch n := int64(p.uc.reg(regX0 + 16)); {
	case uint32(n) == 0xFFFFFFFD, uint32(n) == 0xFFFFFFFC:
		// The fast traps -3 and -4, mach_absolute_time and mach_continuous_time: the same clock,
		// as no time is spent asleep. They are made too often to be worth a line in the trace.
		p.setX(0, machTicks())
	case n > 0 && n&0xFFFFFFFF == 0x80000000:
		p.machdep()
	case n < 0:
		p.machTrap(-n)
	default:
		p.bsd(n)
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
		s = fmt.Sprintf("[%d] %s", p.current.id, s)
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
