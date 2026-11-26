package emulator

import "github.com/KishanBagaria/ipswrun/dyldcache"

// What a task asks of how it is run, and is told back (task_policy_set and task_policy_get), as
// the kernel's source has them (xnu's task_policy.c) and as macOS 27.2.1 was measured to answer
// a user's process (examples/taskpolicy.c): the kind of program it says it is, and the tiers it
// asks for its timers and its work. Nothing here runs a task sooner or later for it, so what is
// asked is kept and said back, and no more. A copy of a process has none of it, and the program
// a process becomes has the kind of program it said it was and not the tiers, as measured.
type taskPolicy struct {
	role     uint32
	base     [2]uint32 // the tiers it asks for: how soon its timers, and its work
	override [2]uint32 // and those to be over what it asks for itself
}

const (
	policyCategory, policyBaseQoS, policyOverrideQoS = 1, 8, 9
	latencyTiers, throughputTiers                    = 0xFF0000, 0xFE0000 // a tier is one of these and a number from 1 to 6
)

// tier is whether a word is no tier, or one of a kind.
func tier(word, kind uint32) bool { return word == 0 || word > kind && word <= kind+6 }

// kept is where a task keeps the tiers of a flavour.
func (t *taskPolicy) kept(flavor uint32) *[2]uint32 {
	if flavor == policyOverrideQoS {
		return &t.override
	}
	return &t.base
}

// migTaskPolicySet is task_policy_set(task, flavour, words, how many). A task may say it is a
// program in front (1), behind (2) or one for the user (7); the kinds that are for the system's
// own are refused a user, as is anything else (KERN_INVALID_ARGUMENT). Tiers are each none or
// one of its own kind. A flavour the kernel has none of to set is KERN_INVALID_ARGUMENT. What
// was not measured stops: the other flavours it has (the tiers one at a time, and suppression),
// what root may say, more words than a message of this kind holds, the port a task is only
// read by, and on an iPhone the kind of program, which the source has the kernel refuse a task
// that says it of itself there.
func migTaskPolicySet(p *Process, body []byte) migReply {
	const most = 16 // the words a message of this kind holds
	if len(body) < 40 {
		return migReply{none: true}
	}
	flavor, count := le.Uint32(body[32:]), le.Uint32(body[36:])
	if r := p.names[p.asked.remote]; count > most || uint64(len(body)) < 40+4*uint64(count) || r != nil && r.port == p.taskRead {
		return migReply{none: true}
	}
	word := func(i int) uint32 { return le.Uint32(body[40+4*i:]) }
	switch flavor {
	case policyCategory:
		switch role := word; {
		case count < 1:
			return migCode(kernInvalidArg)
		case p.uid == 0 || p.cache.Platform == dyldcache.IOS:
			return migReply{none: true}
		case role(0) == 1 || role(0) == 2 || role(0) == 7:
			p.policy.role = role(0)
		default:
			return migCode(kernInvalidArg)
		}
	case policyBaseQoS, policyOverrideQoS:
		if count < 2 || !tier(word(0), latencyTiers) || !tier(word(1), throughputTiers) {
			return migCode(kernInvalidArg)
		}
		*p.policy.kept(flavor) = [2]uint32{word(0), word(1)}
	case 3, 10, 11:
		return migReply{none: true}
	default:
		return migCode(kernInvalidArg)
	}
	return migCode(0)
}

// migTaskPolicyGet is task_policy_get(task, flavour, words, how many, whether what is usual is
// asked for), into room for just the words of the flavour: less is KERN_INVALID_ARGUMENT, and
// more, which the kernel answers with as many words as there was room for, was not measured and
// stops. What is usual for the kind of program is none. A flavour the kernel has none of to
// tell is KERN_INVALID_ARGUMENT; what is usual of the tiers, and the other flavours it has
// (suppression and state), stop.
func migTaskPolicyGet(p *Process, body []byte) migReply {
	if len(body) < 44 {
		return migReply{none: true}
	}
	flavor, room, usual := le.Uint32(body[32:]), le.Uint32(body[36:]), le.Uint32(body[40:]) != 0
	var words []uint32
	switch flavor {
	case policyCategory:
		if words = []uint32{p.policy.role}; usual {
			words = []uint32{0}
		}
	case policyBaseQoS, policyOverrideQoS:
		if words = p.policy.kept(flavor)[:]; usual && room >= 2 {
			return migReply{none: true}
		}
	case 3, 4:
		return migReply{none: true}
	default:
		return migCode(kernInvalidArg)
	}
	switch {
	case room < uint32(len(words)):
		return migCode(kernInvalidArg)
	case room > uint32(len(words)):
		return migReply{none: true}
	}
	reply := counted(le32(words...))
	reply.body = append(reply.body, le32(uint32(truth(usual)))...)
	return reply
}
