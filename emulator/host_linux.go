package emulator

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// uc_ctl is variadic; the System V calling conventions pass its arguments like any others.
var ucCtl struct {
	set func(uc uintptr, control uint32, value uintptr) int32
	get func(uc uintptr, control uint32, value *int32) int32
}

func ctlSet(uc uintptr, control uint32, value uintptr) int32 { return ucCtl.set(uc, control, value) }
func ctlGet(uc uintptr, control uint32, value *int32) int32  { return ucCtl.get(uc, control, value) }

func hostSymbols() []symbol { return nil }

// commpageRows is what is not zero in the two commpages of an Apple silicon Mac (macOS 27.2),
// in rows of 16 bytes: a Linux host has no commpage of its own to give the guest, of macOS or of
// iOS. The per-boot random values at 0x320 are left out. setupCommpage replaces the clock fields.
var commpageRows = []struct {
	addr  uint64
	bytes string
}{
	{commPage + 0x000, "\x63\x6f\x6d\x6d\x70\x61\x67\x65\x20\x36\x34\x2d\x62\x69\x74\x00"},
	{commPage + 0x010, "\xc8\x7f\x10\xa7\x97\xef\xff\x87\x00\x00\x00\x00\x00\x00\x03\x00"},
	{commPage + 0x020, "\xc8\x7f\x10\xa7\x0e\x0e\x80\x00\x00\x00\x00\x00\x00\x00\x00\x03"},
	{commPage + 0x030, "\x00\x00\x00\x00\x10\x10\x10\x0e\x00\x00\x00\x00\x20\x00\x00\x00"},
	{commPage + 0x080, "\x32\x58\x01\x72\x00\x00\x00\x00\x62\xce\xdc\x1f\x88\xfe\xff\xff"},
	{commPage + 0x090, "\x03\x01\x01\x00\x00\x00\x00\x00\x9f\x31\x23\xe0\x77\x01\x00\x00"},
	{commPage + 0x0a0, "\x16\x10\xc7\x30\xeb\x5f\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
	{commPage + 0x0c0, "\xd8\x5c\xc2\x20\x05\x0e\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00"},
	{commPage + 0x0d0, "\x00\x00\x10\x30\x30\x00\x10\x20\x00\x00\x10\x30\x20\x00\x10\x20"},
	{commPage + 0x100, "\x00\x00\x00\x00\x09\x00\x00\x00\x00\x00\x00\x00\x03\x01\x00\x00"},
	{commPage + 0x110, "\x00\x00\x30\x30\x20\x00\x10\x20\x00\x00\x10\x30\x20\x00\x10\x20"},
	{commPage + 0x120, "\xac\xc9\xc0\x20\x05\x0e\x00\x00\x37\xe1\xfd\x6a\x00\x00\x00\x00"},
	{commPage + 0x130, "\xc8\xe2\x1b\x7f\xcf\x1e\x66\x49\xa4\x01\x3b\xf5\xb2\x00\x00\x00"},
	{commPage + 0x140, "\x00\x36\x6e\x01\x00\x00\x00\x00\xfc\xff\xff\xff\xff\xff\xff\xff"},
	{commPage + 0x160, "\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
	{commPage + 0x180, "\x45\x3d\x42\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
	{commPage + 0x200, "\x00\x00\x00\x00\x01\x01\x01\x01\x01\x01\x02\x02\x02\x02\x02\x02"},
	{commPageRO + 0x020, "\x00\x00\x00\x00\x0e\x0e\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
	{commPageRO + 0x030, "\x00\x00\x00\x00\x00\x00\x00\x0e\x00\x00\x00\x00\x00\x00\x00\x00"},
}

// hostPage is a page of the recorded commpage.
func hostPage(addr uint64, size int) []byte {
	out := make([]byte, size)
	for _, row := range commpageRows {
		if row.addr >= addr && row.addr < addr+uint64(size) {
			copy(out[row.addr-addr:], row.bytes)
		}
	}
	return out
}

// uptimeNanos is nanoseconds of uptime, not counting suspend: the clock Unicorn's counter runs on.
func uptimeNanos() uint64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return uint64(ts.Nano())
}

// darwinErrno maps the Linux errno numbers that mean something else, or nothing, on Darwin.
// Those up to 34 (ERANGE) are the same on both but for EAGAIN.
var darwinErrno = map[syscall.Errno]int64{
	syscall.EAGAIN: 35, syscall.EDEADLK: 11, syscall.ENAMETOOLONG: 63, syscall.ENOLCK: 77, syscall.ENOSYS: eNOSYS,
	syscall.ENOTEMPTY: 66, syscall.ELOOP: 62, syscall.ENODATA: eNOATTR, syscall.EOVERFLOW: 84, syscall.EILSEQ: 92,
	syscall.ENOTSUP: eNOTSUP, syscall.ENOBUFS: 55, syscall.ETIMEDOUT: 60, syscall.ESTALE: 70, syscall.EDQUOT: 69,
}

func guestErrno(errno syscall.Errno) int64 {
	if mapped, ok := darwinErrno[errno]; ok {
		return mapped
	}
	if errno > syscall.ERANGE {
		return 5 // EIO
	}
	return int64(errno)
}

// hostWhence is lseek's whence for the host: SEEK_HOLE and SEEK_DATA (3 and 4 on Darwin) are the
// other way round on Linux.
func hostWhence(whence int) int {
	switch whence {
	case 3:
		return unix.SEEK_HOLE
	case 4:
		return unix.SEEK_DATA
	}
	return whence
}

func mtimeSeconds(st *syscall.Stat_t) int64 { return int64(st.Mtim.Sec) }
