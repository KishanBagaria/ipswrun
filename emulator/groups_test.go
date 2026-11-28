package emulator

import "testing"

func TestKillGroupAsksWhoIsThere(t *testing.T) {
	// With signal 0, kill of a group only says whether anyone in it can be sent a signal, as
	// tests/kernel/groups.c prints natively: no one in it is ESRCH, and no one who can be sent one
	// (another user's, or ended and not yet waited for) is EPERM.
	m := &Machine{}
	parent := &Process{m: m, pid: 10, uid: 501}
	root := &Process{m: m, pid: 15, uid: 0, pgroup: 15}
	m.procs = []*Process{
		parent,
		{m: m, pid: 11, uid: 501, parent: parent, pgroup: 10},               // in its parent's group
		{m: m, pid: 12, uid: 501, parent: parent, pgroup: 12},               // in one of its own
		{m: m, pid: 13, uid: 501, parent: parent, pgroup: 13, exited: true}, // ended, not waited for
		{m: m, pid: 14, uid: 501, parent: parent, pgroup: 14, exited: true, reaped: true},
		root,
	}
	for group, want := range map[uint32]int64{10: 0, 11: -eSRCH, 12: 0, 13: -ePERM, 14: -eSRCH, 15: -ePERM, 99: -eSRCH} {
		if got := parent.killGroup(group, 0); got != want {
			t.Errorf("kill of group %d: %d, want %d", group, got, want)
		}
	}
	if got := root.killGroup(10, 0); got != 0 {
		t.Errorf("root's kill of a user's group: %d", got)
	}
}
