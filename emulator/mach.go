package emulator

import (
	"fmt"
	"slices"
	"strconv"
)

var machNames = map[int64]string{
	10: "vm_allocate", 11: "vm_purgable_control", 12: "vm_deallocate", 13: "task_dyld_process_info_notify_get", 14: "vm_protect",
	15: "vm_map", 16: "port_allocate", 18: "port_deallocate", 19: "port_mod_refs", 20: "port_move_member", 21: "port_insert_right",
	22: "port_insert_member", 23: "port_extract_member", 24: "port_construct", 25: "port_destruct", 26: "reply_port",
	27: "thread_self", 28: "task_self", 29: "host_self", 31: "mach_msg", 32: "mach_msg_overwrite", 33: "semaphore_signal",
	34: "semaphore_signal_all", 36: "semaphore_wait", 37: "semaphore_wait_signal", 38: "semaphore_timedwait", 40: "port_get_attributes", 41: "port_guard", 42: "port_unguard", 43: "generate_activity_id",
	46: "pid_for_task", 47: "mach_msg2", 50: "thread_get_special_reply_port", 59: "swtch_pri", 60: "swtch", 61: "thread_switch",
	70: "host_create_mach_voucher", 76: "port_type", 77: "port_request_notification", 89: "timebase_info", 90: "mach_wait_until",
	91: "mk_timer_create", 92: "mk_timer_destroy", 93: "mk_timer_arm", 94: "mk_timer_cancel", 95: "mk_timer_arm_leeway",
}

// Kernel and message return codes.
const (
	kernInvalidAddress = 1
	kernNoSpace        = 3
	kernInvalidArg     = 4
	kernFailure        = 5
	kernInvalidName    = 15
	kernInvalidRight   = 17
	kernInvalidValue   = 18
	kernNotSupported   = 46
	sendInvalidDest    = 0x10000003
	sendTooSmall       = 0x10000008
	rcvTimedOut        = 0x10004003
	bootstrapPort      = 0x707
	hostPort           = 0x303
	taskPort           = 0x203
)

var machHandlers map[string]handler

func init() {
	machHandlers = map[string]handler{
		"task_self": {0, constant(taskPort)}, "thread_self": {0, machThreadSelf}, "host_self": {0, constant(hostPort)},
		"reply_port": {0, machNewReceive}, "thread_get_special_reply_port": {0, machSpecialReplyPort},
		"vm_allocate": {4, machVMAllocate}, "vm_map": {6, machVMMap}, "vm_deallocate": {3, machVMDeallocate},
		"vm_protect": {5, machVMProtect}, "vm_purgable_control": {4, constant(0)},
		"port_allocate": {3, machPortAllocate}, "port_construct": {4, machPortConstruct}, "port_deallocate": {2, machPortDeallocate},
		"port_mod_refs": {4, machPortModRefs}, "port_insert_right": {4, machPortInsertRight}, "port_destruct": {4, machPortDestruct},
		"port_guard": {4, machPortGuard}, "port_unguard": {3, machPortUnguard}, "port_get_attributes": {5, machPortGetAttributes}, "port_insert_member": {3, machPortInsertMember},
		"port_move_member": {3, machPortInsertMember}, "port_extract_member": {3, machPortExtractMember}, "port_type": {3, machPortType},
		"port_request_notification":         {7, machPortRequestNotification},
		"task_dyld_process_info_notify_get": {2, constant(kernFailure)}, // nobody is watching
		"timebase_info":                     {1, machTimebaseInfo}, "generate_activity_id": {3, machGenerateActivityID},
		"host_create_mach_voucher": {4, machVoucher}, "swtch_pri": {1, sysYield}, "swtch": {0, sysYield}, "thread_switch": {3, sysYield},
		"mk_timer_create": {0, machNewReceive}, "mk_timer_destroy": {1, machTimerDestroy}, "mk_timer_arm": {2, machTimerArm},
		"mk_timer_arm_leeway": {4, machTimerArmLeeway}, "mk_timer_cancel": {2, machTimerCancel}, "semaphore_signal": {1, machSemaphoreSignal},
		"semaphore_signal_all": {1, machSemaphoreSignalAll}, "semaphore_wait": {1, machSemaphoreWait},
		"semaphore_timedwait": {3, machSemaphoreTimedwait}, "semaphore_wait_signal": {2, machSemaphoreWaitSignal}, "pid_for_task": {2, machPidForTask},
		"mach_msg": {7, machMsg}, "mach_msg_overwrite": {8, machMsgOverwrite}, "mach_msg2": {8, machMsg2},
	}
}

func (p *Process) machTrap(n int64) {
	name, ok := machNames[n]
	if !ok {
		name = strconv.FormatInt(n, 10)
	}
	args := p.args()
	h, ok := machHandlers[name]
	if !ok && p.unimplemented(fmt.Sprintf("mach trap %d %s", n, name), args) {
		p.setX(0, kernInvalidArg) // what the kernel's own table gives a trap it does not have
		return
	}
	r := p.call(h, args, 1)
	p.setX(0, uint64(r))
	p.note("mach_%s(%s) = %#x", name, hexArgs(args[:h.args]), uint64(r))
}

func (p *Process) newPort() uint32 {
	p.nextPort += 0x100
	return p.nextPort
}

// vmMap reserves memory for what, placed as the VM_FLAGS of vm_allocate, vm_map and vm_remap ask;
// false when it cannot.
func (p *Process) vmMap(addr, size, mask, flags uint64, what allocation) (uint64, bool) {
	what.addr, what.mask, what.fixed, what.keepOther = addr, mask, flags&1 == 0, flags&0x4000 == 0
	return p.alloc(size, what)
}

func (p *Process) vmAllocate(addrp, size, mask, flags uint64, prot uint32, name string) int64 {
	start, ok := p.vmMap(p.u64(addrp), size, mask, flags, allocation{name: name, deny: denied(prot)})
	if !ok {
		return kernNoSpace
	}
	p.write(addrp, le64(start))
	return 0
}

func machVMAllocate(p *Process, a []uint64) int64 {
	return p.vmAllocate(a[1], a[2], 0, a[3], vmProtDefault, "vm_allocate")
}

func machVMMap(p *Process, a []uint64) int64 {
	return p.vmAllocate(a[1], a[2], a[3], a[4], uint32(a[5]&protAll), "vm_map")
}

const vmProtDefault = 3 // VM_PROT_DEFAULT: read and write

func machVMDeallocate(p *Process, a []uint64) int64 { return sysMunmap(p, a[1:]) }

// machVMProtect is mprotect by the Mach name, of whole pages. Setting the maximum protection,
// which nothing here keeps, can only take away from the current one.
func machVMProtect(p *Process, a []uint64) int64 {
	start, prot := a[1]&tbiMask&^(pageSize-1), uint32(a[4]&protAll)
	end := align(a[1]&tbiMask+a[2], pageSize)
	if a[3] != 0 { // set_maximum
		if r := p.findRegion(start); r != nil {
			prot &= r.prot()
		}
	}
	if !p.protect(start, end, prot) {
		return kernInvalidAddress
	}
	return 0
}

func machPortType(p *Process, a []uint64) int64 {
	p.write(a[2], le32(0x20000))
	return 0
}

func machTimebaseInfo(p *Process, a []uint64) int64 {
	p.write(a[0], le32(125, 3))
	return 0
}

func machGenerateActivityID(p *Process, a []uint64) int64 {
	p.write(a[2], le64(0x1000))
	return 0
}

func machThreadSelf(p *Process, a []uint64) int64 { return int64(p.current.port) }

func machPidForTask(p *Process, a []uint64) int64 {
	p.write(a[1], le32(p.pid))
	return 0
}

// message is one mach_msg call in the form all three traps reduce to.
type message struct {
	msg, option, sendSize, rcvSize, rcvMsg uint64
	rcvName                                uint32
	timeout                                uint32 // milliseconds
	inline                                 bool   // mach_msg2 passes the header fields in registers
	bits, remote, local, id                uint32
	// Of a message given as a vector: where the data beside it is sent from and received to.
	aux, auxSendSize, auxRcv, auxRcvSize uint64
}

func machMsg(p *Process, a []uint64) int64 {
	return p.doMsg(message{msg: a[0], option: a[1] & 0xFFFFFFFF, sendSize: a[2], rcvSize: a[3], rcvMsg: a[0], rcvName: uint32(a[4]), timeout: uint32(a[5])})
}

func machMsgOverwrite(p *Process, a []uint64) int64 {
	m := message{msg: a[0], option: a[1] & 0xFFFFFFFF, sendSize: a[2], rcvSize: a[3], rcvMsg: a[7], rcvName: uint32(a[4]), timeout: uint32(a[5])}
	if m.rcvMsg == 0 {
		m.rcvMsg = m.msg
	}
	return p.doMsg(m)
}

// machMsg2: x2 = msgh_bits | send_size<<32; x3 = remote | local<<32; x4 = voucher | id<<32;
// x5 = desc_count | rcv_name<<32; x6 = rcv_size | priority<<32; x7 = timeout
//
// With MACH64_MSG_VECTOR x0 is not the message but a vector of two (address to send from, address
// to receive to if not the same, and the size of each): the message, and data that travels beside
// it. The sizes in the registers are then how many of the two there are.
func machMsg2(p *Process, a []uint64) int64 {
	m := message{msg: a[0], option: a[1] & 0xFFFFFFFF, sendSize: a[2] >> 32, rcvSize: a[6] & 0xFFFFFFFF, rcvMsg: a[0],
		rcvName: uint32(a[5] >> 32), timeout: uint32(a[7]), inline: true, bits: uint32(a[2]), remote: uint32(a[3]), local: uint32(a[3] >> 32), id: uint32(a[4] >> 32)}
	if a[1]&msgVector != 0 {
		count := m.rcvSize
		if m.option&machSendMsg != 0 {
			count = m.sendSize
		}
		part := func(i uint64) (data, rcv, sendSize, rcvSize uint64) {
			v := p.read(a[0]+24*i, 24)
			data, rcv = le.Uint64(v), le.Uint64(v[8:])
			if rcv == 0 {
				rcv = data
			}
			return data, rcv, uint64(le.Uint32(v[16:])), uint64(le.Uint32(v[20:]))
		}
		m.msg, m.rcvMsg, m.sendSize, m.rcvSize = part(0)
		if count > 1 {
			m.aux, m.auxRcv, m.auxSendSize, m.auxRcvSize = part(1)
		}
	}
	return p.doMsg(m)
}

func header(bits, size, local, id uint32) []byte { return le32(bits, size, 0, local, 0, id) }

// doMsg sends, receives, or does one and then the other.
func (p *Process) doMsg(m message) int64 {
	if m.option&machSendMsg != 0 {
		if failed := p.send(m); failed != 0 {
			return failed
		}
	}
	if m.option&machRcvMsg != 0 {
		return p.receive(m)
	}
	return 0
}

// send delivers a message: to the queue of a port, whichever process receives from it, or to the
// kernel, whose answer is queued on the reply port at once.
func (p *Process) send(m message) int64 {
	if m.sendSize < 24 || m.sendSize&3 != 0 {
		return sendTooSmall // no header, or not a whole number of words
	}
	body := p.read(m.msg, m.sendSize)
	if !m.inline {
		m.bits, m.remote, m.local, m.id = le.Uint32(body), le.Uint32(body[8:]), le.Uint32(body[12:]), le.Uint32(body[20:])
	}
	q := p.destination(m.remote)
	if q != nil && q.object != nil {
		// A port of the kernel's: it answers at once, and a request it does not know is one
		// nothing implements.
		reply, ok := q.object.request(p, m.id, body)
		p.note("  mach_msg id %d to the kernel's port %#x send %d rcv %d", m.id, m.remote, m.sendSize, m.rcvSize)
		if !ok {
			p.unknown[unknownMIG{m.id, m.remote, p.uc.reg(regLR)}]++
			return sendInvalidDest
		}
		p.answer(m, m.id+100, reply, 0)
		if m.bits&0x1F == moveSend {
			p.releaseSend(m.remote)
		}
		return 0
	}
	if q != nil {
		said := ""
		if p.trace { // what the message says, read before its rights are taken out of it
			ports, payload, ok := xpcBody(m.bits, body)
			if d, err := xpcDecode(payload, ports); ok && err == nil {
				said = ": " + d.String()
			} else { // not XPC: how it starts, as it is
				said = fmt.Sprintf(": %x", body[24:min(len(body), 24+64)])
			}
		}
		sent, ok := p.outgoing(m, body)
		if !ok {
			return sendTooSmall // shorter than what it says it carries
		}
		p.note("  mach_msg id %d to port %#x: %d bytes queued%s", m.id, m.remote, len(sent.data), said)
		switch r := p.names[m.remote]; {
		case m.bits&0x1F == makeSendOnce: // made for this message, and used up by it
			q.onces++
			sent.once = true
		case m.bits&0x1F == moveSendOnce:
			sent.once = true
		case m.bits&0x1F == makeSend:
			q.made++
		case m.bits&0x1F == moveSend && r != nil && r.sends > 0:
			// The right it was sent with is given up now. The port counts it until the message
			// is received.
			r.sends--
			p.forget(m.remote)
			sent.moved = true
		}
		if m.auxSendSize > 0 {
			sent.aux = p.read(m.aux, m.auxSendSize)
		}
		post(q, sent)
		return 0
	}
	if m.remote == bootstrapPort && p.m.launchd(p, m, body) {
		return 0
	}
	routine := migRoutines[m.id]
	unhandled := ""
	if routine == nil {
		unhandled = " (unhandled)"
	}
	p.note("  mach_msg id %d to port %#x send %d rcv %d%s", m.id, m.remote, m.sendSize, m.rcvSize, unhandled)
	if routine == nil {
		p.unknown[unknownMIG{m.id, m.remote, p.uc.reg(regLR)}]++ // symbolized only if the run fails
		return sendInvalidDest
	}
	p.answer(m, m.id+100, routine(p, body), 0)
	return 0
}

// ---- MIG routines ----

type migReply struct {
	body    []byte
	complex bool        // it starts with descriptors
	memory  []outOfLine // what its memory descriptors hold, each by where in the message it is
}

var ndr = []byte{0, 0, 0, 0, 1, 0, 0, 0}

// migCode is a reply carrying only a return code.
func migCode(code uint32) migReply {
	return migReply{body: append(append([]byte{}, ndr...), le32(code)...)}
}

// migData is a successful reply with out-parameters.
func migData(out []byte) migReply { return migReply{body: append(migCode(0).body, out...)} }

// migPort is a reply moving one port right.
// machVoucher (host_create_mach_voucher) gives out a name for a voucher, which is nothing more.
func machVoucher(p *Process, a []uint64) int64 {
	p.write(a[3], le32(p.newPort()))
	return 0
}

func migPort(name uint32) migReply {
	return migReply{body: append(le32(1), portDescriptor(name, moveSend)...), complex: true}
}

// migMemory is a reply of memory out of line, which may be none, and data after it.
func migMemory(memory, data []byte) migReply {
	reply := migReply{body: append(append(append(le32(1), memoryDescriptor(len(memory))...), ndr...), data...), complex: true}
	if len(memory) > 0 {
		reply.memory = []outOfLine{{at: 24 + 4, data: memory}} // after the header and the count of descriptors
	}
	return reply
}

// migPortData is a reply of a port and data after it.
func migPortData(name uint32, data []byte) migReply {
	return migReply{body: append(append(migPort(name).body, ndr...), data...), complex: true}
}

// answer queues the reply to a message on the port its sender gave for one, if it gave one.
func (p *Process) answer(m message, id uint32, reply migReply, sender uint32) {
	c := p.takeIn(m.local, m.bits>>8&0x1F)
	to := c.port
	if to == nil {
		if m.local != 0 {
			p.note("  no port %#x to answer on", m.local)
		}
		return
	}
	// It arrives through the right it was given to answer with, which that uses up: a right to
	// send once, as a rule.
	bits := c.disposition << 8
	if reply.complex {
		bits |= bitsComplex
	}
	post(to, machMessage{data: append(header(bits, uint32(24+len(reply.body)), 0, id), reply.body...), memory: reply.memory, sender: sender,
		once: c.disposition == moveSendOnce, moved: c.disposition == moveSend})
}

var migRoutines map[uint32]func(p *Process, body []byte) migReply

func init() {
	ok := func(*Process, []byte) migReply { return migCode(0) }
	freshPort := func(p *Process, _ []byte) migReply { return migPort(p.newPort()) }
	migRoutines = map[uint32]func(*Process, []byte) migReply{
		200:  migHostInfo,
		202:  func(*Process, []byte) migReply { return migData(le64(pageSize)) }, // host_page_size
		205:  migHostGetIOMain,
		216:  migHostStatistics,
		217:  ok, // host_request_notification: of the calendar changing, which it does not
		219:  migHostStatistics64,
		206:  freshPort, // host_get_clock_service
		412:  migHostGetSpecialPort,
		1000: migClockGetTime,
		3405: migTaskInfo,
		3218: migPortSetAttributes,
		3228: migPortGetContext,
		3240: func(*Process, []byte) migReply { return migData(le64(0)) }, // mach_port_is_connection_for_service: it is, under no sandbox filter
		3409: migTaskGetSpecialPort,
		3410: ok,        // task_set_special_port
		3418: freshPort, // semaphore_create
		4811: migVMMap,
		4817: migMakeMemoryEntry,
		4813: migVMRemap,
		4822: func(*Process, []byte) migReply { return migCode(kernNotSupported) }, // mach_vm_reclaim_ring_allocate: malloc frees at once instead
		8000: ok,                                                                   // task_restartable_ranges_register
		8001: ok,
	}
}

// migTaskGetSpecialPort hands out only the bootstrap port, which leads to launchd (see launchd.go).
func migTaskGetSpecialPort(p *Process, body []byte) migReply {
	which := int32(le.Uint32(body[32:]))
	p.note("  task_get_special_port %d", which)
	if which == 4 {
		return migPort(bootstrapPort)
	}
	return migPort(0)
}

// migHostGetSpecialPort gives the host port itself, which libdispatch asks for by this route
// before it watches Mach ports. The privileged ones are not for a user process.
func migHostGetSpecialPort(p *Process, body []byte) migReply {
	which := int32(le.Uint32(body[36:]))
	p.note("  host_get_special_port %d", which)
	if which == 1 { // HOST_PORT
		return migPort(hostPort)
	}
	return migCode(kernInvalidArg)
}

// How mach_make_memory_entry is asked for memory that is not the caller's own as it stands.
const (
	memNamedCreate = 0x020000 // new memory, of no address
	memCopy        = 0x200000 // a copy of the caller's, made now
)

// migMakeMemoryEntry (mach_make_memory_entry_64) names a stretch of memory with a port, by which
// it can be mapped again, here or in a process the port is sent to: how large data is passed
// along without copying it into a message. Unless a copy is asked for, the port names the
// caller's memory itself, and a mapping of it that is not a copy is the same memory.
func migMakeMemoryEntry(p *Process, body []byte) migReply {
	size, offset, how := le.Uint64(body[48:]), le.Uint64(body[56:]), le.Uint32(body[64:])
	start := offset &^ (pageSize - 1)
	size = align(offset+size, pageSize) - start
	var entry *span
	if how&(memNamedCreate|memCopy) == 0 {
		entry = p.share(start, size)
	} else if s := p.m.newShared(size); s != nil {
		if how&memNamedCreate == 0 {
			p.readInto(s.data[:size], start)
		}
		entry = &span{s, 0, size}
	}
	if entry == nil {
		return migCode(kernInvalidArg)
	}
	entry.of.retain()
	name := p.sendRight(&port{m: p.m, memory: entry})
	p.note("  mach_make_memory_entry(%#x, %#x, %#x) = %#x", offset, size, how, name)
	return migPortData(name, le64(size))
}

func migVMMap(p *Process, body []byte) migReply { // mach_vm_map
	addr, size, mask, flags := le.Uint64(body[48:]), le.Uint64(body[56:]), le.Uint64(body[64:]), uint64(le.Uint32(body[72:]))
	what := allocation{name: "vm_map"}
	if r := p.names[le.Uint32(body[28:])]; r != nil && r.port.memory != nil {
		entry, from, copied := r.port.memory, le.Uint64(body[76:]), le.Uint32(body[84:]) != 0
		if from > entry.size || align(size, pageSize) > entry.size-from || !entry.of.window(entry.off+from, size) {
			return migCode(kernInvalidArg)
		}
		what = allocation{kind: sharedRegion, shared: entry.of, off: entry.off + from, name: "memory entry"}
		if copied {
			what = allocation{kind: fileRegion, data: slices.Clone(entry.of.data[what.off : what.off+align(size, pageSize)]), name: "memory entry"}
		}
	}
	start, ok := p.vmMap(addr, size, mask, flags, what)
	if !ok {
		return migCode(kernNoSpace)
	}
	p.note("  mach_vm_map(%#x, %#x, flags %#x) = %#x", addr, size, flags, start)
	return migData(le64(start))
}

// migVMRemap (mach_vm_remap) gives the caller a second mapping of memory it has. Of memory that
// is a file's, it is a copy made now, which is the same thing as long as neither mapping is
// written afterwards: true of code, which is what gets remapped that way (the Objective-C
// runtime's block trampolines). Of any other memory it is the same memory, unless a copy was
// asked for.
func migVMRemap(p *Process, body []byte) migReply {
	addr, size, mask, flags := le.Uint64(body[48:]), le.Uint64(body[56:]), le.Uint64(body[64:]), uint64(le.Uint32(body[72:]))
	source, copied := le.Uint64(body[76:]), le.Uint32(body[84:]) != 0
	r := p.findRegion(source)
	if r == nil {
		p.note("  mach_vm_remap of %#x size %#x: not memory the process has", source, size)
		return migCode(kernInvalidArg)
	}
	what := allocation{name: "vm_remap:" + r.name}
	if copied || r.kind == fileRegion || r.kind == cacheRegion {
		if r.end < source+size {
			return migCode(kernInvalidArg)
		}
		what.kind, what.data = fileRegion, p.read(source, size)
	} else if shared := p.share(source, size); shared != nil {
		what.kind, what.shared, what.off = sharedRegion, shared.of, shared.off
	} else {
		return migCode(kernInvalidArg)
	}
	start, ok := p.vmMap(addr, size, mask, flags, what)
	if !ok {
		return migCode(kernNoSpace)
	}
	p.note("  mach_vm_remap(%#x, %#x) = %#x", source, size, start)
	return migData(append(le64(start), le32(5, 7)...)) // current and maximum protection
}

func migClockGetTime(p *Process, _ []byte) migReply {
	now := p.m.clock.now()
	return migData(le32(uint32(now.Unix()), uint32(now.Nanosecond())))
}

// counted is a MIG variable-length array of 32-bit words: its count, then the words.
func counted(info []byte) migReply { return migData(append(le32(uint32(len(info)/4)), info...)) }

// hostFlavor answers a host_info-shaped request, a flavor and how many words the caller has room
// for, with what info says for the flavor, cut to fit; nil from info is a flavor it does not know.
func hostFlavor(p *Process, body []byte, call string, info func(flavor int32) []byte) migReply {
	flavor, count := int32(le.Uint32(body[32:])), uint64(le.Uint32(body[36:]))
	p.note("  %s flavor %d", call, flavor)
	out := info(flavor)
	if out == nil {
		return migCode(kernInvalidArg)
	}
	return counted(out[:min(count*4, uint64(len(out)))])
}

func migHostInfo(p *Process, body []byte) migReply {
	return hostFlavor(p, body, "host_info", func(flavor int32) []byte {
		switch flavor {
		case 1: // HOST_BASIC_INFO
			return append(le32(1, 1, 0, cpuARM64, cpuSubtypeARM64E, 0x10000, cpuARM64, 0, 0, 1), le64(guestMemory)...)
		case 5: // HOST_PRIORITY_INFO
			return le32(0, 31, 31, 31, 0, 31, 0, 127)
		}
		return nil
	})
}

// The pages of guestMemory as host_statistics tells them: half free, as kern.memorystatus_level
// says, and of the rest, what is in use split between active, inactive and wired. They and the
// counters are the same on every run, so that a guest that prints them prints the same thing.
const (
	pagesFree     = guestMemory / pageSize / 2
	pagesActive   = guestMemory / pageSize / 4
	pagesInactive = guestMemory / pageSize / 8
	pagesWired    = guestMemory/pageSize - pagesFree - pagesActive - pagesInactive
)

// vmCounters is what the pager has done since boot, as vm_statistics has it: zero fills,
// reactivations, pageins, pageouts, faults, copy-on-write faults, lookups, hits and purges.
var vmCounters = []uint64{100000, 0, 2000, 0, 300000, 5000, 0, 0, 0}

// hostStatistics is what host_statistics (wide false) and host_statistics64 answer for a flavor,
// or nil for one they do not know.
func hostStatistics(wide bool) func(flavor int32) []byte {
	return func(flavor int32) []byte {
		pages := le32(pagesFree, pagesActive, pagesInactive, pagesWired)
		switch {
		case flavor == 1: // HOST_LOAD_INFO: load averages and Mach factors, in fixed point of 1000
			return le32(0, 0, 0, 1000, 1000, 1000)
		case flavor == 2 && !wide: // HOST_VM_INFO: the counters cut to 32 bits, and no purgeable or speculative pages
			for _, c := range vmCounters {
				pages = append(pages, le32(uint32(c))...)
			}
			return append(pages, le32(0, 0)...)
		case flavor == 3: // HOST_CPU_LOAD_INFO: ticks spent in user, system, idle and nice
			return le32(0, 0, 0, 0)
		case flavor == 4: // HOST_VM_INFO64
			b := append(pages, le64(vmCounters...)...)
			b = append(b, le32(0, 0)...)                             // purgeable, speculative
			b = append(b, le64(0, 0, 0, 0)...)                       // decompressions, compressions, swapins, swapouts
			b = append(b, le32(0, 0, pagesInactive, pagesActive)...) // compressor, throttled, file-backed and anonymous pages
			return append(b, le64(0)...)                             // pages held compressed
		}
		return nil
	}
}

// migHostStatistics (host_statistics) and migHostStatistics64 (host_statistics64) are how vm_stat
// and top see memory and load.
func migHostStatistics(p *Process, body []byte) migReply {
	return hostFlavor(p, body, "host_statistics", hostStatistics(false))
}

func migHostStatistics64(p *Process, body []byte) migReply {
	return hostFlavor(p, body, "host_statistics64", hostStatistics(true))
}

// auditToken is who a process is, as the kernel says it: its audit user (none), its user and
// group twice over, effective and real, its pid, its audit session, and which of its executions
// this is.
func auditToken(uid, gid, pid uint32) []byte {
	return le32(0, uid, gid, uid, gid, pid, 100000, guestExecution)
}

func migTaskInfo(p *Process, body []byte) migReply {
	flavor := int32(le.Uint32(body[32:]))
	p.note("  task_info flavor %d", flavor)
	switch flavor {
	case 17: // TASK_DYLD_INFO
		return counted(append(le64(p.imageInfo[:]...), le32(1)...))
	case 15: // TASK_AUDIT_TOKEN
		return counted(auditToken(p.uid, p.gid, p.pid))
	}
	return migCode(kernInvalidArg)
}
