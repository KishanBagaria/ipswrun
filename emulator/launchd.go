package emulator

// launchd is what a process asks for the port of a service by its name, and what a service gets
// its own port from. There is none here, and one process: these are launchd's answers for the
// services the guest was started as the provider of (Config.Services), which it can check in and
// then look up like any client. Every other name is a service that does not exist.

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
	routineCheckInMach  = 206 // the same, as bootstrap_check_in asks it: how a daemon that is not an XPC listener gets its port

	noSuchService = 3 // ESRCH, as launchd says it
	launchdPID    = 1
)

// service is the port of a service the guest provides, made when it is first asked for: launchd
// holds a service's port, and what is sent to it, until the service checks in.
func (p *Process) service(name string) (q *port, declared bool) {
	q, declared = p.services[name]
	if declared && q == nil {
		q = p.newReceive()
		p.services[name] = q
	}
	return q, declared
}

// launchd answers a message to the bootstrap port; false if it has no answer.
func (p *Process) launchd(m message, raw []byte) bool {
	ports, payload, ok := xpcBody(m.bits, raw)
	if !ok {
		return false
	}
	request, err := xpcDecode(payload, ports)
	if err != nil {
		p.note("  to launchd: %v", err)
		return false
	}
	routine := uint64(m.id & 0xFFFF)
	if r := request.uint("routine"); r != 0 {
		routine = r
	}
	reply := &xpcDictionary{}
	q, provided := p.service(request.string("name"))
	checkIn := routine == routineCheckIn || routine == routineCheckInMach
	switch {
	case !checkIn && routine != routineLookUp && routine != routineLookUpLegacy:
		p.note("  to launchd, routine %d: %v", routine, request)
		return false
	case !provided:
		reply.set("error", int64(noSuchService))
	case checkIn:
		reply.set("port", xpcPort{xpcReceive, q.name})
	default:
		p.transfer(q.name, makeSend)
		reply.set("port", xpcPort{xpcSend, q.name})
	}
	// libxpc takes an answer only from launchd (root, pid 1, as the message's trailer says) and
	// only if it names the process that asked and which of its executions.
	reply.set("req_pid", uint64(guestPID)).set("rec_execcnt", uint64(guestExecution))
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
	p.answer(m, xpcReply, migReply{body: body, port: len(rights) > 0}, launchdPID)
	return true
}
