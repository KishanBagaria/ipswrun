package emulator

// launchd is what a process asks for the port of a service by its name, and what a service gets
// its own port from. There is none here: these are launchd's answers for the services the guests
// were started as the providers of (Guest.Services). The guest that provides a service can check
// it in, and any guest can look it up, the provider among them. Every other name is a service
// that does not exist.

// xpcBody splits a Mach message carrying an XPC object into the names in its port descriptors and
// the serialized object; false if it is not laid out as its header says.
func xpcBody(bits uint32, raw []byte) (ports []uint32, payload []byte, ok bool) {
	at := 24
	if bits&bitsComplex != 0 {
		at, ok = descriptors(raw, func(at int, kind byte) {
			if kind == 0 {
				ports = append(ports, le.Uint32(raw[at:]))
			}
		})
		if !ok {
			return nil, nil, false
		}
	}
	return ports, raw[at:], len(raw) >= 24
}

const (
	xpcReply = 0x20000000 // the id of an answer

	// What is asked: in the low bits of a message's id, or in older releases in the message.
	routineLookUp       = 207
	routineLookUpLegacy = 804 // the same question, as bootstrap_look_up asks it
	routineCheckIn      = 805
	routineCheckInMach  = 206 // a check-in as bootstrap_check_in asks it: how a daemon that is not an XPC listener gets its port
	routineEventStream  = 403 // a daemon asking for the events launchd keeps for it (xpc_set_event_stream_handler)
	routineJobValue     = 301 // a process asking for one of the values launchd keeps about its job, by number
	routineStarted      = 802 // libxpc of iOS 14 to 16.0 telling launchd of the process as it starts: its environment, where it is from
	routineUncork       = 803 // and then that launchd may start the services the process declares (_xpc_uncork_domain)
	routineImages       = 800 // and, as it loads more (dlopen), the paths of those images, as 802 gives those it started with

	// The values a process asks for.
	jobManagerUID  = 3 // the user of the launchd domain the process is in
	jobManagerName = 6 // and the name of that domain

	noSuchService = 3 // ESRCH, as launchd says it
	launchdPID    = 1
)

// service is a service launchd knows: the guest that provides it, and its port, which is made
// when it is first asked for. launchd holds the port, and what is sent to it, until the provider
// checks in.
type service struct {
	provider *Process
	port     *port
}

// launchd answers a message a process sent to its bootstrap port; false if it has no answer.
func (m *Machine) launchd(p *Process, msg message, raw []byte) bool {
	ports, payload, ok := xpcBody(msg.bits, raw)
	if !ok {
		return false
	}
	request, err := xpcDecode(payload, ports)
	if err != nil {
		p.note("  to launchd: %v", err)
		return false
	}
	routine := uint64(msg.id & 0xFFFF)
	if r := request.uint("routine"); r != 0 {
		routine = r
	}
	reply := &xpcDictionary{}
	s := m.services[request.string("name")]
	if s != nil && s.port == nil {
		s.port = &port{m: m}
	}
	checkIn := routine == routineCheckIn || routine == routineCheckInMach
	switch {
	case routine == routineEventStream:
		// What launchd has for a daemon on one of its streams of events, by name: nothing.
		reply.set("events", []any{})
	case routine == routineStarted, routine == routineUncork, routine == routineImages:
		// An answer is all the process waits for, and it reads nothing in it. Without one it
		// stops, or, unanswered after it started, finds no service at all.
	case routine == routineJobValue:
		// Which of launchd's domains the process is in: the system's, or the one of a user who is
		// logged in at the screen. A program that serves both takes from it which it is to be.
		switch key := request.uint("outgsk"); {
		case !request.bool("get"):
			reply.set("error", int64(noSuchService))
		case key == jobManagerName && p.uid == 0:
			reply.set("out", "System")
		case key == jobManagerName:
			reply.set("out", "Aqua")
		case key == jobManagerUID:
			reply.set("out", int64(p.uid))
		default:
			p.note("  to launchd, routine %d: %v", routine, request)
			return false
		}
	case !checkIn && routine != routineLookUp && routine != routineLookUpLegacy:
		p.note("  to launchd, routine %d: %v", routine, request)
		return false
	case s == nil || s.port.dead || checkIn && s.provider != p:
		reply.set("error", int64(noSuchService))
	case checkIn:
		reply.set("port", xpcPort{xpcReceive, p.giveOut(carried{port: s.port, disposition: moveReceive}, nil)})
	default:
		reply.set("port", xpcPort{xpcSend, p.sendRight(s.port)})
	}
	// libxpc takes an answer only from launchd (root, pid 1, as the message's trailer says) and
	// only if it names the process that asked and which of its executions.
	reply.set("req_pid", uint64(p.pid)).set("rec_execcnt", uint64(guestExecution))
	p.note("  to launchd, routine %d: %v, answered %v", routine, request, reply)
	body, rights := xpcEncode(reply)
	if len(rights) > 0 {
		descriptors := le32(uint32(len(rights)))
		for _, right := range rights {
			disposition := byte(moveSend)
			if right.kind == xpcReceive {
				disposition = moveReceive
			}
			descriptors = append(descriptors, portDescriptor(right.name, disposition)...)
		}
		body = append(descriptors, body...)
	}
	p.answer(msg, xpcReply, migReply{body: body, port: len(rights) > 0}, launchdPID)
	return true
}
