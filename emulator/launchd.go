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
	routineEventPoll    = 406 // a daemon that publishes events asking to be told when those who subscribe to them change (xpc_event_publisher_activate)
	routineJobValue     = 301 // a process asking for one of the values launchd keeps about its job, by number
	routineStarted      = 802 // libxpc of iOS 14 to 16.0 telling launchd of the process as it starts: its environment, where it is from
	routineUncork       = 803 // and then that launchd may start the services the process declares (_xpc_uncork_domain)
	routineSession      = 824 // a process asking to be moved to another of launchd's sessions, as nohup does to leave the console's (_vprocmgr_detach_from_console)
	routineImages       = 800 // and, as it loads more (dlopen), the paths of those images, as 802 gives those it started with

	// The values a process asks for.
	jobManagerUID  = 3 // the user of the launchd domain the process is in
	jobManagerName = 6 // and the name of that domain
	jobManagerPID  = 4 // the launchd that manages it
	jobOther       = 9 // what it is of is not known: 0, of a program started from a shell

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
	if name := request.string("name"); m.services[name] == nil && (routine == routineLookUp || routine == routineLookUpLegacy) {
		m.onDemand(p, name)
	}
	s := m.services[request.string("name")]
	if s != nil && s.port == nil {
		s.port = &port{m: m}
	}
	checkIn := routine == routineCheckIn || routine == routineCheckInMach
	if routine == routineEventPoll {
		// libxpc sends this with a port to be answered on and goes on: the answer is for when
		// what subscribes to the stream has changed, after which it asks for the events again
		// (routine 403) and then this again. Nothing subscribes here, as no plist's LaunchEvents
		// are acted on, so nothing changes: launchd keeps the right to answer, and does not.
		m.polls = append(m.polls, p.takeIn(msg.local, msg.bits>>8&0x1F))
		p.note("  to launchd, routine %d: %v, to be answered when something changes", routine, request)
		return true
	}
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
		case key == jobManagerPID:
			reply.set("out", int64(launchdPID))
		case jobRefused[key] != 0 && !p.m.job(p):
			reply.set("error", jobRefused[key])
		case key == jobOther && !p.m.job(p):
			reply.set("out", int64(0))
		default:
			p.note("  to launchd, routine %d: %v", routine, request)
			return false
		}
	case routine == routineSession:
		// As measured on macOS 27.2.1 and 27.2 (26B123, in a virtual machine, as a user and as
		// root, from the console's session and from a background one): asked for the session
		// of what runs in the background, launchd answers with a right to send to a port of
		// its own, named for exceptions, and a second time refuses (37, already done); for
		// any other session's name it refuses (125). What it then does for the process was
		// not seen from outside it: nothing is sent to that port here. The other kinds of
		// request this routine takes, and another name after the move, stop being answered.
		const already, noSession = 37, 125
		background := request.string("session") == "Background"
		switch {
		case request.uint("type") != 7 || request.uint("handle") != 0 || p.detached && !background:
			p.note("  to launchd, routine %d: %v", routine, request)
			return false
		case !background:
			reply.set("error", int64(noSession))
		case p.detached:
			reply.set("error", int64(already))
		default:
			if m.exceptions == nil {
				m.exceptions = &port{m: m}
			}
			p.detached = true
			reply.set("exc-port", xpcPort{xpcSend, p.sendRight(m.exceptions)})
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
	// The answer to a move between sessions was measured to hold neither, and is taken without.
	if routine != routineSession {
		reply.set("req_pid", uint64(p.pid)).set("rec_execcnt", uint64(p.version))
	}
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
	p.answer(msg, xpcReply, migReply{body: body, complex: len(rights) > 0}, launchdPID)
	return true
}

// onDemand is launchd starting the build's own daemon or agent for a service that is looked up
// and that no guest provides, if the machine was told to (Config.Launch): the one whose launchd
// plist names the service. The service is there at once, as launchd has it, and what is sent to
// it waits for the daemon to check in. The daemon is started between two stretches of
// emulation (see spawn.go).
func (m *Machine) onDemand(p *Process, name string) {
	if m.cfg.Launch == nil || name == "" || m.asked[name] {
		return
	}
	m.asked[name] = true
	g, err := away(m.clock, func() (*Guest, error) { return m.cfg.Launch(name, p.uid == 0) })
	if err != nil {
		stopf("looking for the build's provider of %s: %v", name, err)
	}
	if g == nil {
		return
	}
	for _, provided := range g.Services {
		m.asked[provided] = true
		if m.services[provided] == nil {
			m.services[provided] = &service{}
		}
	}
	p.note("  launchd starts %s for %s", g.Executable, name)
	m.deferred = append(m.deferred, func() {
		started, err := m.start(*g)
		if err != nil {
			stopf("starting %s for the service %s: %v", g.Executable, name, err)
		}
		started.demanded = true
	})
}

// jobRefused is what launchd answers a program started from a shell that asks it for the values
// it does not give, by key, as measured on macOS 27.2.1 (whether launchd manages the process,
// key 5, is one: libxpc answers that itself). What it answers a job of its own was not measured,
// and such a question stops being answered, as before.
var jobRefused = map[uint64]int64{1: 135, 2: 45, 5: 45, 10: 135, 11: 135, 12: 126}

// job says whether launchd started a process to provide a service: one of the daemons and agents
// given to the run, or started when its service was first looked up. A copy such a process makes
// of itself is none.
func (m *Machine) job(p *Process) bool {
	for _, s := range m.services {
		if s.provider == p {
			return true
		}
	}
	return false
}
