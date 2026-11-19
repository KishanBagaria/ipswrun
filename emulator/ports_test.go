package emulator

import (
	"bytes"
	"testing"
)

func TestXPCRoundTrip(t *testing.T) {
	sent := (&xpcDictionary{}).set("name", "com.example.service").set("type", uint64(7)).set("flags", int64(-2)).
		set("instance", []byte{1, 2, 3, 4, 5}).set("yes", true).set("port", xpcPort{xpcSend, 0x1234}).set("nothing", nil)
	data, rights := xpcEncode(sent)
	if len(data)%4 != 0 || len(rights) != 1 || rights[0].name != 0x1234 {
		t.Errorf("encoded to %d bytes with rights %v", len(data), rights)
	}
	got, err := xpcDecode(data, []uint32{0x1234})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != sent.String() || got.string("name") != "com.example.service" || got.uint("type") != 7 || got.values["flags"] != int64(-2) ||
		!bytes.Equal(got.values["instance"].([]byte), []byte{1, 2, 3, 4, 5}) || got.values["port"] != (xpcPort{xpcSend, 0x1234}) {
		t.Errorf("decoded %v, sent %v", got, sent)
	}
	for name, bad := range map[string][]byte{
		"cut short":        data[:len(data)-6],
		"another version":  append(le32(xpcMagic, 4), data[8:]...),
		"not a dictionary": append(le32(xpcMagic, xpcVersion), le32(xpcBool, 1)...),
		"unknown type":     append(le32(xpcMagic, xpcVersion), le32(0x77000)...),
	} {
		if d, err := xpcDecode(bad, nil); err == nil {
			t.Errorf("%s decoded to %v", name, d)
		}
	}
}

func TestPorts(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		const out, msg, in = dataAddr + 0x40, dataAddr + 0x100, dataAddr + 0x400
		allocate := func() uint32 {
			if x0, _, stopped := p.trap(-16, 0x203, rightReceive, out); x0 != 0 || stopped != "" {
				t.Errorf("mach_port_allocate: %#x %s", x0, stopped)
			}
			return p.u32(out)
		}
		send := func(to, reply, bits, id uint32, text string) uint64 {
			body := append(header(bits, 0, reply, id), text...)
			le.PutUint32(body[8:], to)
			p.write(msg, body)
			x0, _, _ := p.trap(-31, msg, machSendMsg, uint64(len(body)))
			return x0
		}
		receive := func(from uint32, option uint64) uint64 {
			p.write(in, make([]byte, 256))
			x0, _, _ := p.trap(-31, in, machRcvMsg|option, 0, 256, uint64(from), 0)
			return x0
		}

		// A message waits in its port's queue, and arrives with its header turned around and a trailer.
		a, b := allocate(), allocate()
		if r := receive(a, machRcvTimeout); r != rcvTimedOut {
			t.Errorf("receive from an empty port, not waiting: %#x", r)
		}
		if r := send(a, b, copySend|makeSendOnce<<8, 7, "hello123"); r != 0 {
			t.Errorf("send: %#x", r)
		}
		if r := receive(a, machRcvTimeout); r != 0 {
			t.Errorf("receive: %#x", r)
		}
		got := p.read(in, 24+8+8)
		once := le.Uint32(got[8:])
		if le.Uint32(got) != moveSendOnce|moveSend<<8 || le.Uint32(got[4:]) != 32 || le.Uint32(got[12:]) != a || le.Uint32(got[20:]) != 7 ||
			string(got[24:32]) != "hello123" || le.Uint32(got[36:]) != 8 {
			t.Errorf("received % x", got)
		}
		// The right to reply once has a name of its own, and its port is told if it is dropped unused.
		if r := p.names[once]; once == b || r == nil || !r.once || r.port != p.owned(b) {
			t.Errorf("the reply right arrived as %#x; the reply port is %#x", once, b)
		}
		p.trap(-18, 0x203, uint64(once))
		if r := receive(b, machRcvTimeout); r != 0 || p.u32(in+20) != notifySendOnce {
			t.Errorf("after dropping the reply right: receive %#x, message id %d", r, p.u32(in+20))
		}
		if r := send(a, 0, copySend, 8, "odd"); r != sendTooSmall {
			t.Errorf("send of a message that is not whole words: %#x", r)
		}
		if r := send(once, 0, moveSendOnce, 8, ""); r != sendInvalidDest {
			t.Errorf("send to a right that is gone: %#x", r)
		}

		// The last send right going is told to whoever asked.
		p.trap(-21, 0x203, uint64(a), uint64(a), makeSend)
		p.trap(-77, 0x203, uint64(a), notifyNoSenders, 0, uint64(b), makeSendOnce, out)
		if r := receive(b, machRcvTimeout); r != rcvTimedOut {
			t.Errorf("a no-senders notification while there is a sender: %#x", r)
		}
		p.trap(-18, 0x203, uint64(a))
		if r := receive(b, machRcvTimeout); r != 0 || p.u32(in+20) != notifyNoSenders {
			t.Errorf("after the last send right went: receive %#x, message id %d", r, p.u32(in+20))
		}

		// A message too large for the room given stays, and says how large it is.
		send(a, 0, copySend, 9, "0123456789ab")
		p.write(in, make([]byte, 256))
		if x0, _, _ := p.trap(-31, in, machRcvMsg|machRcvLarge|machRcvTimeout, 0, 30, uint64(a), 0); x0 != rcvTooLarge || p.u32(in+4) != 36 {
			t.Errorf("receive into too little room: %#x, size %d", x0, p.u32(in+4))
		}

		// libdispatch is told of it once, as a message too large to have been delivered with the event.
		q := p.kqueue(0)
		p.changeMachport(q, kevent{ident: uint64(a), filter: filterMachport, flags: evAdd | evEnable | evDispatch | evVanished, fflags: machRcvMsg, udata: 5})
		p.watched(q)
		p.watched(q)
		if len(q.ready) != 1 || q.ready[0].fflags != rcvTooLarge || q.ready[0].data != int64(a) || q.ready[0].ext[1] != 36 ||
			q.ready[0].flags&(evVanished|evAdd) != 0 || q.ready[0].ext[0] == 0 {
			t.Errorf("events for a port with a message: %+v", q.ready)
		}

		// A receive from an empty port waits, and a message posted meanwhile is handed straight to it.
		if r := receive(a, 0); r != 0 || p.u32(in+20) != 9 {
			t.Errorf("receive of the waiting message: %#x id %d", r, p.u32(in+20))
		}
		receive(a, 0)
		waiter := p.current
		if waiter.state != waiting || waiter.kind != waitPort {
			t.Errorf("receive from an empty port: state %d", waiter.state)
		}
		post(p.owned(a), machMessage{data: header(0, 24, 0, 55)})
		if waiter.state != runnable || waiter.answer == nil || waiter.answer.x0 != 0 || p.u32(in+20) != 55 || p.u32(in+12) != a || len(p.owned(a).queue) != 0 {
			t.Errorf("after a message for the waiter: state %d answer %v id %d", waiter.state, waiter.answer, p.u32(in+20))
		}
		waiter.answer = nil

		// launchd knows the services the guest provides, and no other.
		p.m.services["com.example.provided"] = &service{provider: p}
		ask := func(routine uint32, name string) *xpcDictionary {
			reply := allocate()
			request, _ := xpcEncode((&xpcDictionary{}).set("name", name))
			if !p.m.launchd(p, message{bits: makeSendOnce << 8, id: 0x40000000 | routine, local: reply}, append(header(0, 0, bootstrapPort, 0), request...)) {
				t.Errorf("launchd has no answer to routine %d", routine)
			}
			answer := p.owned(reply).queue[0]
			ports, payload, _ := xpcBody(le.Uint32(answer.data), answer.data)
			d, err := xpcDecode(payload, ports)
			if err != nil || answer.sender != launchdPID || d.uint("req_pid") != uint64(p.pid) {
				t.Errorf("launchd's answer: %v %v from pid %d", d, err, answer.sender)
			}
			return d
		}
		// Asked which of its domains a process is in, launchd says the user's, or for root the system's.
		domain := func() string {
			reply := allocate()
			request, _ := xpcEncode((&xpcDictionary{}).set("get", true).set("outgsk", uint64(jobManagerName)))
			p.m.launchd(p, message{bits: makeSendOnce << 8, id: 0x40000000 | routineJobValue, local: reply}, append(header(0, 0, bootstrapPort, 0), request...))
			answer := p.owned(reply).queue[0]
			ports, payload, _ := xpcBody(le.Uint32(answer.data), answer.data)
			d, _ := xpcDecode(payload, ports)
			return d.string("out")
		}
		user := domain()
		p.uid = 0
		if system := domain(); user != "Aqua" || system != "System" {
			t.Errorf("launchd's domains: %q for the user, %q for root", user, system)
		}
		p.uid = guestUID
		if d := ask(routineLookUp, "com.example.other"); d.uint("error") != noSuchService {
			t.Errorf("looking up a service nobody provides: %v", d)
		}
		if d := ask(routineCheckIn, "com.example.other"); d.uint("error") != noSuchService {
			t.Errorf("checking in a service the guest does not provide: %v", d)
		}
		checked, _ := ask(routineCheckIn, "com.example.provided").values["port"].(xpcPort)
		found, _ := ask(routineLookUpLegacy, "com.example.provided").values["port"].(xpcPort)
		if checked.kind != xpcReceive || found.kind != xpcSend || checked.name != found.name || p.owned(found.name) == nil || p.owned(found.name).sends != 1 {
			t.Errorf("checked in as %+v, found as %+v", checked, found)
		}
		// bootstrap_check_in asks with a routine of its own, and gets the same port.
		if again, _ := ask(routineCheckInMach, "com.example.provided").values["port"].(xpcPort); again != checked {
			t.Errorf("checked in as %+v, and by bootstrap_check_in as %+v", checked, again)
		}
		if d := ask(routineCheckInMach, "com.example.other"); d.uint("error") != noSuchService {
			t.Errorf("bootstrap_check_in of a service the guest does not provide: %v", d)
		}
	})
}
