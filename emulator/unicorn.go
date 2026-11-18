package emulator

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// arm64 register ids (uc_arm64_reg) and the CPU model (uc_cpu_arm64).
const (
	regX29       = 1
	regLR        = 2
	regNZCV      = 3
	regSP        = 4
	regX0        = 199
	regPC        = 260
	regCPACREL1  = 261
	regTPIDRRO   = 263
	regCPReg     = 290
	cpuModelMax  = 3
	unicornMajor = 2
	unicornMinor = 1
)

// Other constants of unicorn.h.
const (
	archARM64       = 2
	modeARM         = 0
	protAll         = 7
	ctlCPUModel     = 7 | 1<<26 // UC_CTL_CPU_MODEL with one argument
	ctlWrite        = 1 << 30
	ctlRead         = 2 << 30
	queryTimeout    = 4
	hookInterrupt   = 1 << 0
	hookUnmapped    = 1<<4 | 1<<5 | 1<<6 // read, write, fetch
	hookInvalid     = 1 << 14
	clockUptimeRaw  = 8
	libSystemHandle = purego.RTLD_DEFAULT
)

// The part of libunicorn 2.1's C API the emulator uses, bound at run time so that the library can
// be fetched and verified first (see artifact.go). Every call but version and strerror returns a
// uc_err.
//
// uc_ctl is variadic, and on Apple arm64 variadic arguments travel on the stack, never in
// registers: the six placeholders fill the remaining integer registers so that the value lands
// where the callee looks for it. uc_hook_add is variadic too, but only past the arguments used here.
var api struct {
	version  func(major, minor *uint32) uint32
	strerror func(code int32) string
	open     func(arch, mode int32, uc *uintptr) int32
	close    func(uc uintptr) int32
	query    func(uc uintptr, kind int32, result *uint64) int32
	ctlSet   func(uc uintptr, control uint32, _, _, _, _, _, _ uintptr, value uintptr) int32
	ctlGet   func(uc uintptr, control uint32, _, _, _, _, _, _ uintptr, value *int32) int32
	regRead  func(uc uintptr, id int32, value unsafe.Pointer) int32
	regWrite func(uc uintptr, id int32, value unsafe.Pointer) int32
	memMap   func(uc uintptr, address, size uint64, protection uint32) int32
	memUnmap func(uc uintptr, address, size uint64) int32
	memRead  func(uc uintptr, address uint64, bytes unsafe.Pointer, size uint64) int32
	memWrite func(uc uintptr, address uint64, bytes unsafe.Pointer, size uint64) int32
	emuStart func(uc uintptr, begin, until, timeoutMicros, count uint64) int32
	emuStop  func(uc uintptr) int32
	hookAdd  func(uc uintptr, hook *uintptr, kind int32, callback, user uintptr, begin, end uint64) int32

	// From the host's libSystem.
	memcpy     func(to unsafe.Pointer, from, size uintptr) uintptr
	uptimeNsec func(clock int32) uint64

	// C entry points of the hook callbacks; user is a key of targets.
	onInterrupt, onUnmapped, onInvalid uintptr
}

var loadOnce struct {
	sync.Mutex
	path string
}

// loadUnicorn binds the C API to the library at path, once per process.
func loadUnicorn(path string) error {
	loadOnce.Lock()
	defer loadOnce.Unlock()
	if loadOnce.path != "" {
		if loadOnce.path != path {
			return fmt.Errorf("unicorn already loaded from %s", loadOnce.path)
		}
		return nil
	}
	library, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return fmt.Errorf("load unicorn: %w", err)
	}
	for _, symbol := range []struct {
		library  uintptr
		name     string
		function any
	}{
		{library, "uc_version", &api.version}, {library, "uc_strerror", &api.strerror},
		{library, "uc_open", &api.open}, {library, "uc_close", &api.close},
		{library, "uc_query", &api.query}, {library, "uc_ctl", &api.ctlSet}, {library, "uc_ctl", &api.ctlGet},
		{library, "uc_reg_read", &api.regRead}, {library, "uc_reg_write", &api.regWrite},
		{library, "uc_mem_map", &api.memMap}, {library, "uc_mem_unmap", &api.memUnmap},
		{library, "uc_mem_read", &api.memRead}, {library, "uc_mem_write", &api.memWrite},
		{library, "uc_emu_start", &api.emuStart}, {library, "uc_emu_stop", &api.emuStop},
		{library, "uc_hook_add", &api.hookAdd},
		{libSystemHandle, "memcpy", &api.memcpy}, {libSystemHandle, "clock_gettime_nsec_np", &api.uptimeNsec},
	} {
		address, err := purego.Dlsym(symbol.library, symbol.name)
		if err != nil {
			return fmt.Errorf("load unicorn: %s has no %s", path, symbol.name)
		}
		purego.RegisterFunc(symbol.function, address)
	}
	var major, minor uint32
	if api.version(&major, &minor); major != unicornMajor || minor != unicornMinor {
		return fmt.Errorf("load unicorn: %s is version %d.%d, not %d.%d", path, major, minor, unicornMajor, unicornMinor)
	}
	// Callbacks are never released, so they are made once and shared by every engine.
	api.onInterrupt = purego.NewCallback(func(_ uintptr, number uint32, user uintptr) {
		hooksFor(user).interrupt(number)
	})
	api.onUnmapped = purego.NewCallback(func(_ uintptr, _ int32, address uint64, size int32, _ int64, user uintptr) uintptr {
		return truth(hooksFor(user).unmapped(address, int(size)))
	})
	api.onInvalid = purego.NewCallback(func(_, user uintptr) uintptr {
		return truth(hooksFor(user).invalid())
	})
	loadOnce.path = path
	return nil
}

func truth(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

// hooks receives the engine's callbacks, on the goroutine that called start.
type hooks interface {
	interrupt(number uint32)
	unmapped(address uint64, size int) bool
	invalid() bool
}

// targets maps the user value Unicorn hands a callback to the hooks of its engine.
var targets struct {
	sync.Mutex
	last  uintptr
	hooks map[uintptr]hooks
}

func hooksFor(id uintptr) hooks {
	targets.Lock()
	defer targets.Unlock()
	return targets.hooks[id]
}

// engine is one Unicorn arm64 instance.
type engine struct {
	uc, id uintptr
}

type ucError int32

func (e ucError) Error() string { return api.strerror(int32(e)) }

func check(err int32) error {
	if err != 0 {
		return ucError(err)
	}
	return nil
}

// must is for calls whose failure means the emulator itself is wrong, not the guest.
func must(err int32, what string) {
	if err != 0 {
		panic(stop{fmt.Sprintf("unicorn %s: %v", what, ucError(err))})
	}
}

func newEngine(h hooks) (*engine, error) {
	e := &engine{}
	if err := check(api.open(archARM64, modeARM, &e.uc)); err != nil {
		return nil, fmt.Errorf("unicorn open: %w", err)
	}
	targets.Lock()
	if targets.hooks == nil {
		targets.hooks = map[uintptr]hooks{}
	}
	targets.last++
	e.id = targets.last
	targets.hooks[e.id] = h
	targets.Unlock()
	if err := e.setup(); err != nil {
		e.close()
		return nil, fmt.Errorf("unicorn setup: %w", err)
	}
	return e, nil
}

func (e *engine) setup() error {
	// The model has to be chosen before anything else touches the engine. Reading it back is what
	// shows the variadic call above really delivered it; a default core lacks what dyld needs.
	model := int32(-1)
	if err := errors.Join(check(api.ctlSet(e.uc, ctlCPUModel|ctlWrite, 0, 0, 0, 0, 0, 0, cpuModelMax)),
		check(api.ctlGet(e.uc, ctlCPUModel|ctlRead, 0, 0, 0, 0, 0, 0, &model))); err != nil {
		return err
	}
	if model != cpuModelMax {
		return fmt.Errorf("CPU model is %d, not %d", model, cpuModelMax)
	}
	var hook uintptr
	return errors.Join(check(api.hookAdd(e.uc, &hook, hookInterrupt, api.onInterrupt, e.id, 1, 0)),
		check(api.hookAdd(e.uc, &hook, hookUnmapped, api.onUnmapped, e.id, 1, 0)),
		check(api.hookAdd(e.uc, &hook, hookInvalid, api.onInvalid, e.id, 1, 0)))
}

func (e *engine) close() {
	if e.uc != 0 {
		api.close(e.uc)
		targets.Lock()
		delete(targets.hooks, e.id)
		targets.Unlock()
		e.uc = 0
	}
}

func (e *engine) reg(id int) uint64 {
	var v uint64
	must(api.regRead(e.uc, int32(id), unsafe.Pointer(&v)), "reg_read")
	return v
}

func (e *engine) setReg(id int, v uint64) {
	must(api.regWrite(e.uc, int32(id), unsafe.Pointer(&v)), "reg_write")
}

func (e *engine) setCPReg(crn, crm, op0, op1, op2 uint32, v uint64) {
	// uc_arm64_cp_reg
	reg := struct {
		crn, crm, op0, op1, op2 uint32
		_                       uint32
		val                     uint64
	}{crn: crn, crm: crm, op0: op0, op1: op1, op2: op2, val: v}
	must(api.regWrite(e.uc, regCPReg, unsafe.Pointer(&reg)), "reg_write")
}

func (e *engine) memMap(addr, size uint64) {
	must(api.memMap(e.uc, addr, size, protAll), fmt.Sprintf("mem_map %#x+%#x", addr, size))
}

func (e *engine) memUnmap(addr, size uint64) {
	must(api.memUnmap(e.uc, addr, size), fmt.Sprintf("mem_unmap %#x+%#x", addr, size))
}

func (e *engine) memRead(addr uint64, n int) ([]byte, error) {
	out := make([]byte, n)
	if n == 0 {
		return out, nil
	}
	return out, check(api.memRead(e.uc, addr, unsafe.Pointer(&out[0]), uint64(n)))
}

func (e *engine) memWrite(addr uint64, data []byte) {
	if len(data) > 0 {
		must(api.memWrite(e.uc, addr, unsafe.Pointer(&data[0]), uint64(len(data))), fmt.Sprintf("mem_write %#x+%#x", addr, len(data)))
	}
}

// start runs from begin until something stops the engine: until is reached, a hook calls stop,
// the CPU faults (the only case that is an error), or timeoutMicros pass. Unicorn itself says
// whether its timeout fired; after that the engine's state is not reliable and it must not be
// started again.
func (e *engine) start(begin, until, timeoutMicros uint64) (timedOut bool, err error) {
	if err := check(api.emuStart(e.uc, begin, until, timeoutMicros, 0)); err != nil {
		return false, err
	}
	var fired uint64
	must(api.query(e.uc, queryTimeout, &fired), "query")
	return fired != 0, nil
}

func (e *engine) stop() { api.emuStop(e.uc) }

// hostPage copies memory of this process: the host's own commpage, which the guest's libsystem
// expects at the same address.
func hostPage(addr uint64, size int) []byte {
	out := make([]byte, size)
	api.memcpy(unsafe.Pointer(&out[0]), uintptr(addr), uintptr(size))
	return out
}

// uptimeNanos is nanoseconds of uptime, not counting sleep (mach_absolute_time's clock).
func uptimeNanos() uint64 { return api.uptimeNsec(clockUptimeRaw) }
