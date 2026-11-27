package emulator

import (
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

const clockUptimeRaw = 8

// uc_ctl is variadic, and on Apple arm64 variadic arguments travel on the stack, never in
// registers: the six placeholders fill the remaining integer registers so that the value lands
// where the callee looks for it.
var ucCtl struct {
	set func(uc uintptr, control uint32, _, _, _, _, _, _ uintptr, value uintptr) int32
	get func(uc uintptr, control uint32, _, _, _, _, _, _ uintptr, value *int32) int32
}

func ctlSet(uc uintptr, control uint32, value uintptr) int32 {
	return ucCtl.set(uc, control, 0, 0, 0, 0, 0, 0, value)
}

func ctlGet(uc uintptr, control uint32, value *int32) int32 {
	return ucCtl.get(uc, control, 0, 0, 0, 0, 0, 0, value)
}

// From the host's libSystem.
var host struct {
	memcpy     func(to unsafe.Pointer, from, size uintptr) uintptr
	uptimeNsec func(clock int32) uint64
}

func hostSymbols() []symbol {
	return []symbol{{purego.RTLD_DEFAULT, "memcpy", &host.memcpy}, {purego.RTLD_DEFAULT, "clock_gettime_nsec_np", &host.uptimeNsec}}
}

// hostPage copies memory of this process: the host's own commpage, which the guest's libsystem
// expects at the same address.
func hostPage(addr uint64, size int) []byte {
	out := make([]byte, size)
	host.memcpy(unsafe.Pointer(&out[0]), uintptr(addr), uintptr(size))
	return out
}

// uptimeNanos is nanoseconds of uptime, not counting sleep (mach_absolute_time's clock).
func uptimeNanos() uint64 { return host.uptimeNsec(clockUptimeRaw) }

// The host is the guest's own kind of system: its errno numbers and lseek's whence are the guest's.
func guestErrno(errno syscall.Errno) int64 { return int64(errno) }

func hostWhence(whence int) int { return whence }

func mtimeSeconds(st *syscall.Stat_t) int64 { return st.Mtimespec.Sec }

// utimeOmit is UTIME_OMIT: a date utimensat leaves as it is.
const utimeOmit = -2

// hostTimes are a host file's times of access and change, in nanoseconds.
func hostTimes(st *syscall.Stat_t) (access, change int64) {
	return st.Atimespec.Nano(), st.Mtimespec.Nano()
}
