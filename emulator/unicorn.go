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
	regV0        = 228 // the first of the 32 vector registers, 16 bytes each
	regFPCR      = 291
	regFPSR      = 292
	cpuModelMax  = 3
	unicornMajor = 2
	unicornMinor = 1
)

// Other constants of unicorn.h.
const (
	archARM64     = 2
	modeARM       = 0
	protAll       = 7
	ctlCPUModel   = 7 | 1<<26 // UC_CTL_CPU_MODEL with one argument
	ctlTLBFlush   = 11        // UC_CTL_TLB_FLUSH, with none
	ctlWrite      = 1 << 30
	ctlRead       = 2 << 30
	hookInterrupt = 1 << 0
	hookUnmapped  = 1<<4 | 1<<5 | 1<<6 // read, write, fetch
	hookProtected = 1<<7 | 1<<8 | 1<<9 // likewise, of memory mapped without the permission
	hookInvalid   = 1 << 14
)

// The part of libunicorn 2.1's C API the emulator uses, bound at run time so that the library can
// be fetched and verified first (see artifact.go). Every call but version and strerror returns a
// uc_err. uc_ctl is variadic, and how it is called depends on the host (see ucCtl); uc_hook_add
// is variadic too, but only past the arguments used here.
var api struct {
	version  func(major, minor *uint32) uint32
	strerror func(code int32) string
	open     func(arch, mode int32, uc *uintptr) int32
	close    func(uc uintptr) int32
	regRead  func(uc uintptr, id int32, value unsafe.Pointer) int32
	regWrite func(uc uintptr, id int32, value unsafe.Pointer) int32
	memMap   func(uc uintptr, address, size uint64, protection uint32) int32
	memPtr   func(uc uintptr, address, size uint64, protection uint32, memory unsafe.Pointer) int32
	memUnmap func(uc uintptr, address, size uint64) int32
	memProt  func(uc uintptr, address, size uint64, protection uint32) int32
	memRead  func(uc uintptr, address uint64, bytes unsafe.Pointer, size uint64) int32
	memWrite func(uc uintptr, address uint64, bytes unsafe.Pointer, size uint64) int32
	emuStart func(uc uintptr, begin, until, timeoutMicros, count uint64) int32
	emuStop  func(uc uintptr) int32
	hookAdd  func(uc uintptr, hook *uintptr, kind int32, callback, user uintptr, begin, end uint64) int32

	// A context is a copy of the CPU's registers: what a thread that is not running keeps.
	contextAlloc    func(uc uintptr, context *uintptr) int32
	contextSave     func(uc, context uintptr) int32
	contextRestore  func(uc, context uintptr) int32
	contextRegRead  func(context uintptr, id int32, value unsafe.Pointer) int32
	contextRegWrite func(context uintptr, id int32, value unsafe.Pointer) int32
	contextFree     func(context uintptr) int32

	// C entry points of the hook callbacks; user is a key of targets.
	onInterrupt, onUnmapped, onProtected, onInvalid uintptr
}

// symbol is a C function to bind: its library, its name and a pointer to the Go func variable.
type symbol struct {
	library  uintptr
	name     string
	function any
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
	for _, symbol := range append([]symbol{
		{library, "uc_version", &api.version}, {library, "uc_strerror", &api.strerror},
		{library, "uc_open", &api.open}, {library, "uc_close", &api.close},
		{library, "uc_ctl", &ucCtl.set}, {library, "uc_ctl", &ucCtl.get},
		{library, "uc_reg_read", &api.regRead}, {library, "uc_reg_write", &api.regWrite},
		{library, "uc_mem_map", &api.memMap}, {library, "uc_mem_unmap", &api.memUnmap},
		{library, "uc_mem_map_ptr", &api.memPtr}, {library, "uc_mem_protect", &api.memProt},
		{library, "uc_mem_read", &api.memRead}, {library, "uc_mem_write", &api.memWrite},
		{library, "uc_emu_start", &api.emuStart}, {library, "uc_emu_stop", &api.emuStop},
		{library, "uc_hook_add", &api.hookAdd},
		{library, "uc_context_alloc", &api.contextAlloc}, {library, "uc_context_save", &api.contextSave},
		{library, "uc_context_restore", &api.contextRestore}, {library, "uc_context_reg_read", &api.contextRegRead},
		{library, "uc_context_reg_write", &api.contextRegWrite}, {library, "uc_context_free", &api.contextFree},
	}, hostSymbols()...) {
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
	api.onProtected = purego.NewCallback(func(_ uintptr, kind int32, address uint64, size int32, _ int64, user uintptr) uintptr {
		return truth(hooksFor(user).protected(kind, address, int(size)))
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
	protected(kind int32, address uint64, size int) bool
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
// must stops the run on an engine error; what says of which call, formatted only then.
func must(err int32, what string, args ...any) {
	if err != 0 {
		panic(stop{message: fmt.Sprintf("unicorn %s: %v", fmt.Sprintf(what, args...), ucError(err))})
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
	if err := errors.Join(check(ctlSet(e.uc, ctlCPUModel|ctlWrite, cpuModelMax)),
		check(ctlGet(e.uc, ctlCPUModel|ctlRead, &model))); err != nil {
		return err
	}
	if model != cpuModelMax {
		return fmt.Errorf("CPU model is %d, not %d", model, cpuModelMax)
	}
	var hook uintptr
	return errors.Join(check(api.hookAdd(e.uc, &hook, hookInterrupt, api.onInterrupt, e.id, 1, 0)),
		check(api.hookAdd(e.uc, &hook, hookUnmapped, api.onUnmapped, e.id, 1, 0)),
		check(api.hookAdd(e.uc, &hook, hookProtected, api.onProtected, e.id, 1, 0)),
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

// regBytes reads a register that is wider than a word, and setRegBytes writes one.
func (e *engine) regBytes(id int, v []byte) {
	must(api.regRead(e.uc, int32(id), unsafe.Pointer(&v[0])), "reg_read")
}

func (e *engine) setRegBytes(id int, v []byte) {
	must(api.regWrite(e.uc, int32(id), unsafe.Pointer(&v[0])), "reg_write")
}

func (e *engine) newContext() uintptr {
	var context uintptr
	must(api.contextAlloc(e.uc, &context), "context_alloc")
	return context
}

func (e *engine) save(context uintptr)        { must(api.contextSave(e.uc, context), "context_save") }
func (e *engine) restore(context uintptr)     { must(api.contextRestore(e.uc, context), "context_restore") }
func (e *engine) freeContext(context uintptr) { api.contextFree(context) }

func (e *engine) contextReg(context uintptr, id int) uint64 {
	var v uint64
	must(api.contextRegRead(context, int32(id), unsafe.Pointer(&v)), "context_reg_read")
	return v
}

func (e *engine) setContextReg(context uintptr, id int, v uint64) {
	must(api.contextRegWrite(context, int32(id), unsafe.Pointer(&v)), "context_reg_write")
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

func (e *engine) memMap(addr, size uint64, prot uint32) {
	must(api.memMap(e.uc, addr, size, prot), "mem_map %#x+%#x", addr, size)
}

// memMapHost maps host memory, which every engine that maps it then shares. Its length is a
// whole number of pages.
func (e *engine) memMapHost(addr uint64, memory []byte, prot uint32) {
	must(api.memPtr(e.uc, addr, uint64(len(memory)), prot, unsafe.Pointer(&memory[0])), "mem_map_ptr %#x+%#x", addr, len(memory))
}

func (e *engine) memProtect(addr, size uint64, prot uint32) {
	must(api.memProt(e.uc, addr, size, prot), "mem_protect %#x+%#x", addr, size)
}

// forgetTranslations has the engine look every address up again: it remembers what an address
// may be touched for from the last time it was, and a page that may no longer be read would go
// on being read.
func (e *engine) forgetTranslations() {
	must(ctlSet(e.uc, ctlTLBFlush|ctlWrite, 0), "tlb_flush")
}

func (e *engine) memUnmap(addr, size uint64) {
	must(api.memUnmap(e.uc, addr, size), "mem_unmap %#x+%#x", addr, size)
}

func (e *engine) memRead(addr uint64, n int) ([]byte, error) {
	out := make([]byte, n)
	return out, e.memReadInto(addr, out)
}

func (e *engine) memReadInto(addr uint64, out []byte) error {
	if len(out) == 0 {
		return nil
	}
	return check(api.memRead(e.uc, addr, unsafe.Pointer(&out[0]), uint64(len(out))))
}

func (e *engine) memWrite(addr uint64, data []byte) {
	if len(data) > 0 {
		must(api.memWrite(e.uc, addr, unsafe.Pointer(&data[0]), uint64(len(data))), "mem_write %#x+%#x", addr, len(data))
	}
}

// start runs from begin until something stops the engine: until is reached, stop is called, the
// CPU faults (the only case that is an error), or timeoutMicros of the host's time pass, if that
// is not zero. After a stop that no hook asked for (the timeout's, or one from another goroutine)
// the engine's state is not reliable and it must not be started again.
func (e *engine) start(begin, until, timeoutMicros uint64) error {
	return check(api.emuStart(e.uc, begin, until, timeoutMicros, 0))
}

func (e *engine) stop() { api.emuStop(e.uc) }
