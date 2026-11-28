package emulator

import (
	"os"
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
	futimens   func(fd int32, times *[2]syscall.Timespec) int32
}

func hostSymbols() []symbol {
	return []symbol{{purego.RTLD_DEFAULT, "memcpy", &host.memcpy}, {purego.RTLD_DEFAULT, "clock_gettime_nsec_np", &host.uptimeNsec}, {purego.RTLD_DEFAULT, "futimens", &host.futimens}}
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

// fileTimes are a host file's four times, in nanoseconds: when it was last read, written and
// changed, and when it was made.
func fileTimes(st *syscall.Stat_t) [4]int64 {
	return [4]int64{st.Atimespec.Nano(), st.Mtimespec.Nano(), st.Ctimespec.Nano(), st.Birthtimespec.Nano()}
}

// hostDirKinds is the kind (DT_*) the host's listing has by each name of an open directory,
// read from its start by the host's own getdirentries64, which Go has no call for: its
// libraries read a directory by opening it again by name, which one that may not be searched
// does not let them. Nothing if it cannot be read so.
func hostDirKinds(f *os.File) map[string]byte {
	if _, err := f.Seek(0, 0); err != nil {
		return nil
	}
	kinds := map[string]byte{}
	buf := make([]byte, 1<<16)
	for {
		var position int64
		n, _, errno := syscall.Syscall6(344, f.Fd(), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&position)), 0, 0)
		if errno != 0 || int(n) <= 0 || int(n) > len(buf) {
			return kinds
		}
		for b := buf[:n]; len(b) >= 21; { // a dirent: number, position, its length, its name's, kind, name
			length, named := int(le.Uint16(b[16:])), int(le.Uint16(b[18:]))
			if length < 21 || length > len(b) || 21+named > length {
				return kinds
			}
			kinds[string(b[21:21+named])] = b[20]
			b = b[length:]
		}
	}
}

// setFileTimes sets an open host file's times of access and change, in nanoseconds.
func setFileTimes(fd int, access, change int64) {
	host.futimens(int32(fd), &[2]syscall.Timespec{syscall.NsecToTimespec(access), syscall.NsecToTimespec(change)})
}
