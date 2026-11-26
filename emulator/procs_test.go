package emulator

import (
	"slices"
	"testing"
	"time"
)

func TestProcRecords(t *testing.T) {
	// The processes ps is told of (examples/procs.c prints the same natively): the newest first,
	// those that have ended and are still to be waited for among them as zombies, those that
	// were waited for not, and launchd last.
	m := &Machine{clock: &clock{boot: time.Unix(1000, 0)}}
	first := &Process{m: m, pid: 10, ppid: launchdPID, uid: 501, gid: 20, name: "first", began: time.Unix(2000, 5000)}
	m.procs = []*Process{
		first,
		{m: m, pid: 11, uid: 501, gid: 20, name: "copy", parent: first, ppid: 10, pgroup: 10},
		{m: m, pid: 12, uid: 501, gid: 20, name: "ended", parent: first, ppid: 10, exited: true},
		{m: m, pid: 13, uid: 501, gid: 20, name: "waited-for", parent: first, ppid: 10, exited: true, reaped: true},
		{m: m, pid: 14, uid: 501, gid: 20, name: "a-program-with-a-long-name", parent: first, ppid: 10, execed: true},
	}
	records := m.procRecords()
	var pids []uint32
	for _, r := range records {
		pids = append(pids, r.pid)
	}
	if want := []uint32{14, 12, 11, 10, launchdPID}; !slices.Equal(pids, want) {
		t.Fatalf("the processes listed: %v, want %v", pids, want)
	}
	if records[1].state != procZombie || records[2].state != procRunning || records[2].execed || !records[0].execed || !records[3].execed || le.Uint32(records[3].bytes()[32:]) != procLP64|procExeced {
		t.Errorf("states and flags: %+v", records)
	}
	b := records[3].bytes()
	if len(b) != kinfoProcSize || le.Uint64(b) != 2000 || le.Uint32(b[8:]) != 5 || le.Uint32(b[40:]) != 10 || le.Uint32(b[560:]) != launchdPID || le.Uint32(b[564:]) != 10 || le.Uint32(b[420:]) != 501 || string(b[243:248]) != "first" {
		t.Errorf("the first process's record: % x", b[:64])
	}
	if long := records[0].bytes(); long[243+16] != 0 || string(long[243:243+16]) != "a-program-with-a" {
		t.Errorf("a long name: %q", long[243:243+17])
	}
}
