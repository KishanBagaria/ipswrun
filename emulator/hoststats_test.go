package emulator

import "testing"

func TestHostStatistics64(t *testing.T) {
	request := make([]byte, 40)
	copy(request[32:], le32(4, 38)) // HOST_VM_INFO64, HOST_VM_INFO64_COUNT
	reply := migRoutines[219](&Process{}, request).body
	if code := le.Uint32(reply[8:]); code != 0 {
		t.Fatalf("return code %d", code)
	}
	if n := le.Uint32(reply[12:]); n != 38 {
		t.Fatalf("count %d, want 38", n)
	}
	info := reply[16:]
	free, active, inactive, wired := le.Uint32(info), le.Uint32(info[4:]), le.Uint32(info[8:]), le.Uint32(info[12:])
	if total := uint64(free + active + inactive + wired); total*pageSize != guestMemory {
		t.Errorf("pages add up to %d bytes, want hw.memsize %d", total*pageSize, guestMemory)
	}
	if external, internal := le.Uint32(info[136:]), le.Uint32(info[140:]); external+internal != active+inactive {
		t.Errorf("file-backed %d and anonymous %d pages are not the active and inactive ones", external, internal)
	}

	// A caller with room for less is given less, and the 32-bit HOST_VM_INFO is host_statistics's,
	// not host_statistics64's.
	copy(request[32:], le32(4, 4))
	if n := le.Uint32(migRoutines[219](&Process{}, request).body[12:]); n != 4 {
		t.Errorf("count %d with room for 4", n)
	}
	copy(request[32:], le32(2, 15))
	if code := le.Uint32(migRoutines[219](&Process{}, request).body[8:]); code != kernInvalidArg {
		t.Errorf("host_statistics64 HOST_VM_INFO: code %d", code)
	}
	if n := le.Uint32(migRoutines[216](&Process{}, request).body[12:]); n != 15 {
		t.Errorf("host_statistics HOST_VM_INFO: count %d, want 15", n)
	}
}
