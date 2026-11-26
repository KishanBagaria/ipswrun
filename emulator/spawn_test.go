package emulator

import "testing"

func TestHandOn(t *testing.T) {
	// What every process that comes of another has of it (examples/inherit.c prints the same
	// natively): its limits and policies are its own copies, and a thread's policy stays behind.
	p := &Process{uid: 501, gid: 20, cmask: 0o27, cmaskSet: true, nice: 5,
		rlimits:    map[uint64][2]uint64{8: {200, 300}},                    // RLIMIT_NOFILE
		iopolicies: map[[2]uint32]uint32{{0, 0}: 4, {1<<16 | 0x103, 0}: 2}} // the second a thread's
	to := &Process{}
	p.handOn(to, false)
	if to.uid != 501 || to.gid != 20 || to.cmask != 0o27 || !to.cmaskSet || to.nice != 5 || to.rlimits[8] != [2]uint64{200, 300} {
		t.Fatalf("handed on %+v", to)
	}
	if len(to.iopolicies) != 1 || to.iopolicies[[2]uint32{0, 0}] != 4 {
		t.Fatalf("policies handed on: %v", to.iopolicies)
	}
	copied := &Process{}
	if p.handOn(copied, true); len(copied.iopolicies) != 2 {
		t.Fatalf("policies handed to a copy: %v", copied.iopolicies)
	}
	to.rlimits[8] = [2]uint64{1, 1}
	if p.rlimits[8] != [2]uint64{200, 300} {
		t.Fatal("a limit the child changed is the parent's too")
	}
}
