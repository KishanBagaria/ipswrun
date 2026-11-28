package emulator

import "testing"

func TestHandOn(t *testing.T) {
	// What every process that comes of another has of it (tests/kernel/inherit.c prints the same
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

func TestSessionsAndGroups(t *testing.T) {
	// As tests/kernel/sessions.c prints natively: the first of a group stays in its session, a copy
	// of it may leave for its own, and a process is moved between the groups of its session by
	// itself or by its parent while it is still the program its parent was.
	m := &Machine{}
	first := &Process{m: m, pid: 10}
	copied := &Process{m: m, pid: 11, parent: first, pgroup: 10, session: 10}
	started := &Process{m: m, pid: 12, parent: first, pgroup: 10, session: 10, execed: true}
	leader := &Process{m: m, pid: 13, parent: first}
	m.procs = []*Process{first, copied, started, leader}
	step := func(what string, got, want int64) { // in order: each goes on from the last
		t.Helper()
		if got != want {
			t.Errorf("%s: %d, want %d", what, got, want)
		}
	}
	step("setsid of the first of a group", sysSetsid(first, nil), -ePERM)
	step("a copy into its own group, by its parent", sysSetpgid(first, []uint64{11, 11}), 0)
	step("a copy back into its parent's, by itself", sysSetpgid(copied, []uint64{0, 10}), 0)
	step("into a group there is not", sysSetpgid(first, []uint64{11, 99}), -ePERM)
	step("into one below zero", sysSetpgid(first, []uint64{11, ^uint64(4)}), -eINVAL) // -5
	step("of a started program", sysSetpgid(first, []uint64{12, 12}), -eACCES)
	step("of one that leads a session", sysSetpgid(first, []uint64{13, 10}), -ePERM)
	step("of no child of its own", sysSetpgid(copied, []uint64{12, 12}), -eSRCH)
	step("setsid of a copy", sysSetsid(copied, nil), 11)
	step("setsid again", sysSetsid(copied, nil), -ePERM)
	if copied.sessionOf() != 11 || copied.group() != 11 {
		t.Errorf("after setsid: session %d, group %d", copied.sessionOf(), copied.group())
	}
}
