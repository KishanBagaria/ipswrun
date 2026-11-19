package emulator

import (
	"bytes"
	"testing"
)

// Two processes of one machine: a message from one to the other, with what it carries, the
// death of the one that received it, and memory they share.
func TestProcesses(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		q, err := p.m.newProcess(p.exePath, p.exe)
		if err != nil {
			t.Fatal(err)
		}
		const out, msg, in_, far = dataAddr + 0x40, dataAddr + 0x100, dataAddr + 0x400, dataAddr + 0x4000
		number := func(name string) int64 {
			for n, candidate := range bsdNames {
				if candidate == name {
					return n
				}
			}
			t.Fatalf("no system call named %s", name)
			return 0
		}
		allocate := func(in *Process) uint32 {
			in.trap(-16, 0x203, rightReceive, out)
			return in.u32(out)
		}
		receive := func(in *Process, from uint32, option uint64) uint64 {
			in.write(in_, make([]byte, 256))
			x0, _, _ := in.trap(-31, in_, machRcvMsg|machRcvTimeout|option, 0, 256, uint64(from), 0)
			return x0
		}
		if p.pid == q.pid || q.pid != guestPID+1 {
			t.Errorf("pids %d and %d", p.pid, q.pid)
		}

		// q has a port, and p a right to send to it under a name of its own.
		served := q.newReceive()
		toServed := p.giveOut(q.takeIn(served.name, makeSend), q)
		if p.owned(toServed) != nil || p.names[toServed].port != served || served.sends != 1 {
			t.Errorf("the send right in p: %+v, of %d", p.names[toServed], served.sends)
		}

		// p sends it a message that asks for one answer, gives a port away and has memory beside it.
		answers, given := allocate(p), allocate(p)
		text := bytes.Repeat([]byte("memory "), 3000)
		p.write(far, text)
		body := header(bitsComplex|copySend|makeSendOnce<<8, 56, answers, 77)
		le.PutUint32(body[8:], toServed)
		body = append(append(body, le32(2)...), portDescriptor(given, moveReceive)...)
		body = append(append(append(body, le64(far)...), 0, 0, 0, 1), le32(uint32(len(text)))...)
		p.write(msg, body)
		if x0, _, stopped := p.trap(-31, msg, machSendMsg, uint64(len(body))); x0 != 0 || stopped != "" {
			t.Fatalf("send to the other process: %#x %s", x0, stopped)
		}
		if p.owned(given) != nil || len(served.queue) != 1 {
			t.Errorf("after the send p still has the port it gave: %v; %d queued", p.owned(given) != nil, len(served.queue))
		}
		if r := receive(q, served.name, 3<<24); r != 0 { // with the sender's audit token
			t.Fatalf("receive in the other process: %#x", r)
		}
		got := q.read(in_, 56+52)
		once, moved, memory := le.Uint32(got[8:]), le.Uint32(got[28:]), le.Uint64(got[40:])
		if le.Uint32(got[12:]) != served.name || le.Uint32(got[20:]) != 77 || le.Uint32(got[56+40:]) != p.pid || le.Uint32(got[56+12:]) != guestUID {
			t.Errorf("received % x", got)
		}
		if r := q.names[once]; r == nil || !r.once || r.port != p.owned(answers) {
			t.Errorf("the right to answer arrived in q as %#x: %+v", once, r)
		}
		if port := q.owned(moved); port == nil || port.owner != q || port.name != moved || got[28+10] != moveReceive {
			t.Errorf("the port p gave arrived in q as %#x: %+v", moved, port)
		}
		if q.findRegion(memory) == nil || !bytes.Equal(q.read(memory, uint64(len(text))), text) {
			t.Errorf("the memory arrived in q at %#x", memory)
		}

		// q answers with the right it was given for that, and p asks to be told when q's port dies.
		q.write(msg, append(header(moveSendOnce, 28, 0, 78)[:8], append(le32(once, 0, 0, 78), "done"...)...))
		if x0, _, _ := q.trap(-31, msg, machSendMsg, 28); x0 != 0 {
			t.Errorf("answer: %#x", x0)
		}
		if r := receive(p, answers, 0); r != 0 || p.u32(in_+20) != 78 || string(p.read(in_+24, 4)) != "done" || p.u32(in_+12) != answers {
			t.Errorf("the answer in p: %#x, id %d", r, p.u32(in_+20))
		}
		p.trap(-77, 0x203, uint64(toServed), notifyDeadName, 0, uint64(answers), makeSendOnce, out)
		q.exited = true
		q.reap()
		if r := receive(p, answers, 0); r != 0 || p.u32(in_+20) != notifyDeadName || p.u32(in_+32) != toServed {
			t.Errorf("after q exited: receive %#x, id %d, name %#x", r, p.u32(in_+20), p.u32(in_+32))
		}
		p.write(msg, body)
		if x0, _, _ := p.trap(-31, msg, machSendMsg, uint64(len(body))); x0 != sendInvalidDest {
			t.Errorf("send to the port of a process that exited: %#x", x0)
		}

		// Memory with a name is one memory in every process that maps it.
		r, err := p.m.newProcess(p.exePath, p.exe)
		if err != nil {
			t.Fatal(err)
		}
		open, truncate, mmap := number("shm_open"), number("ftruncate"), number("mmap")
		p.write(msg, str("com.example.shared"))
		r.write(msg, str("com.example.shared"))
		if _, carry, _ := r.trap(open, msg, 2); !carry {
			t.Errorf("shm_open of memory nobody made succeeded")
		}
		made, _, _ := p.trap(open, msg, 2|oCreate, 0o600)
		p.trap(truncate, made, 100)
		opened, carry, _ := r.trap(open, msg, 2)
		mine, _, _ := p.trap(mmap, 0, pageSize, 3, mapShared, made, 0)
		theirs, failed, _ := r.trap(mmap, 0, pageSize, 3, mapShared, opened, 0)
		if carry || failed || mine == 0 || theirs == 0 {
			t.Fatalf("shared memory: opened %d (failed %v), mapped at %#x and %#x (failed %v)", opened, carry, mine, theirs, failed)
		}
		p.write(mine+8, []byte("seen by both"))
		r.write(theirs+100, []byte("and back"))
		if string(r.read(theirs+8, 12)) != "seen by both" || string(p.read(mine+100, 8)) != "and back" {
			t.Errorf("one process does not see what the other wrote: %q, %q", r.read(theirs+8, 12), p.read(mine+100, 8))
		}
	})
}
