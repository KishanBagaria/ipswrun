package emulator

// The kernel's network policy, as far as configd asks of it when it starts: a session, to which
// it adds policies and has them applied. The session is given as the kernel's source has it
// (bsd/net/necp.c) and as a Mac's configd was seen to be answered (dtrace, macOS 27.2 in a
// virtual machine): a descriptor, a number for each policy added (one more than the last, from
// 1), and 0 for applying them. A policy is kept as it was given and is not checked as the
// kernel checks one; none is ever applied, there being no traffic here for one to be about.
// Only a program that is part of the system may open a session (the source's test is that it is
// a platform binary), which no probe is, so what the kernel answers it was seen and not asked.
type necpSession struct {
	last     uint32
	policies map[uint32][]byte
}

const (
	eNODEV = 19

	necpPolicyAdd      = 1
	necpPolicyApplyAll = 4
	necpPolicyMost     = 1024 + 512 + 4096 // NECP_MAX_POLICY_SIZE: the most a policy may take
)

// sysNecpSessionOpen is necp_session_open(flags). A program that is not the system's own is
// refused (EACCES), as measured of a user's; of the system's, root is given a session, and one
// that is not root would be asked for a privilege, which was not seen and stops the run.
func sysNecpSessionOpen(p *Process, a []uint64) int64 {
	switch {
	case p.path == GuestExe: // a program of the host's: no part of the system
		return -eACCES
	case p.uid != 0:
		p.unimplemented("necp_session_open by a program of the system that is not root", a)
		return -eNOSYS
	case p.full():
		return -eMFILE
	}
	n := p.newFD(&fd{kind: dataFD, necp: &necpSession{policies: map[uint32][]byte{}}})
	p.cloexec[uint64(n)] = true
	return int64(n)
}

// sysNecpSessionAction is necp_session_action(session, action, in, its length, out, its length).
// Of a descriptor that is not open it is EBADF and of one that is no session ENODEV, as
// measured. Adding a policy and applying them all are what configd asks; any other action stops.
func sysNecpSessionAction(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	switch {
	case f == nil:
		return -eBADF
	case f.necp == nil:
		return -eNODEV
	}
	s := f.necp
	switch uint32(a[1]) {
	case necpPolicyAdd:
		in, inSize, out, outSize := a[2], a[3], a[4], a[5]
		if inSize == 0 || inSize > necpPolicyMost || in == 0 || outSize < 4 || out == 0 {
			return -eINVAL
		}
		policy := p.read(in, inSize)
		switch valid, routes := necpPolicyValid(policy); {
		case !valid:
			return -eINVAL
		case routes:
			p.unimplemented("a network policy whose result is rules for routes, which are not checked here", a)
			return -eNOSYS
		}
		s.last++
		s.policies[s.last] = policy
		p.write(out, le32(s.last))
		return 0
	case necpPolicyApplyAll:
		return 0
	}
	p.unimplemented("necp_session_action of a kind configd was not seen to ask", a)
	return -eNOSYS
}

// sessionOnly stops a call that is not one of a session's own on its descriptor (reading it,
// writing it, asking of it): the source has the kernel refuse each, and none was measured.
func (p *Process) sessionOnly(a []uint64) int64 {
	p.unimplemented("a call on a session with the network's policy that is not one of its own", a)
	return -eNOSYS
}

// necpFind is necp_buffer_find_tlv: where the first item of a kind starts in a policy, which is
// items of {kind, length, value} one after another, from an offset on; next skips the item at
// the offset itself. -1 if there is none, or the items run past the end.
func necpFind(b []byte, at int, kind byte, next bool) int {
	for {
		if at+5 > len(b) {
			return -1
		}
		is, size := b[at], int(le.Uint32(b[at+1:]))
		if next {
			is, next = 0, false
		}
		if size > len(b)-(at+5) {
			return -1
		}
		if is == kind {
			return at
		}
		at += 5 + size
	}
}

// necpPolicyValid is what necp_handle_policy_add checks of a policy before it keeps it, from the
// source and not measured (only that configd's own were kept was seen): that it has an order of
// no more than four bytes, a result of 1 to 512 bytes whose kind is one there is with as much
// after it as that kind takes, and at least one condition that is not empty, the conditions
// taking no more than 4096 bytes in all. What each condition says is not checked. routes is
// whether the result is rules for routes, which the kernel then checks too.
func necpPolicyValid(b []byte) (valid, routes bool) {
	const order, condition, result = 2, 3, 4
	at := necpFind(b, 0, order, false)
	if at < 0 || le.Uint32(b[at+1:]) > 4 {
		return false, false
	}
	if at = necpFind(b, 0, result, false); at < 0 {
		return false, false
	}
	size := le.Uint32(b[at+1:])
	if size == 0 || size > 512 {
		return false, false
	}
	switch kind, more := b[at+5], size-1; kind {
	case 1, 3: // pass, drop
		valid = more == 0 || more == 4
	case 13, 16, 17: // rules for routes, the first interface there is, allowed with no entitlement
		valid, routes = true, kind == 13
	case 2, 4, 5: // skip to an order, divert, filter
		valid = more >= 4
	case 6: // a tunnel
		valid = more > 4
	case 12: // an interface, by name
		valid = more > 0
	case 14, 15, 18: // an agent, by its identifier
		valid = more >= 16
	case 19: // agents of a type: a domain and a type of 32 bytes each
		valid = more >= 64
	}
	if !valid {
		return false, false
	}
	conditions := 0
	for at, next := necpFind(b, 0, condition, false), true; at >= 0; at = necpFind(b, at, condition, next) {
		if n := int(le.Uint32(b[at+1:])); n > 0 {
			conditions += 5 + n
		}
	}
	return conditions > 0 && conditions <= 4096, routes
}
