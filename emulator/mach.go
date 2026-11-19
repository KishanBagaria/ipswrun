package emulator

import (
	"fmt"
	"strconv"
	"time"
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
	kernNoSpace     = 3
	kernInvalidArg  = 4
	kernFailure     = 5
	sendInvalidDest = 0x10000003
	rcvTimedOut     = 0x10004003
	bootstrapPort   = 0x707
)

var machHandlers map[string]handler

func init() {
	newPort := func(p *Process, a []uint64) int64 { return int64(p.newPort()) }
	portOut := func(arg int) func(*Process, []uint64) int64 {
		return func(p *Process, a []uint64) int64 {
			p.write(a[arg], le32(p.newPort()))
			return 0
		}
	}
	machHandlers = map[string]handler{
		"task_self": {0, constant(0x203)}, "thread_self": {0, machThreadSelf}, "host_self": {0, constant(0x303)},
		"reply_port": {0, newPort}, "thread_get_special_reply_port": {0, newPort},
		"vm_allocate": {4, machVMAllocate}, "vm_map": {6, machVMMap}, "vm_deallocate": {3, machVMDeallocate},
		"vm_protect": {5, constant(0)}, "vm_purgable_control": {4, constant(0)},
		"port_allocate": {3, portOut(2)}, "port_construct": {4, machPortConstruct}, "port_deallocate": {2, constant(0)},
		"port_mod_refs": {4, constant(0)}, "port_insert_right": {4, constant(0)}, "port_destruct": {4, constant(0)},
		"port_guard": {4, machPortGuard}, "port_unguard": {3, constant(0)}, "port_insert_member": {3, constant(0)},
		"port_move_member": {3, constant(0)}, "port_extract_member": {3, constant(0)}, "port_type": {3, machPortType},
		"port_request_notification":         {0, constant(0)},
		"task_dyld_process_info_notify_get": {2, constant(kernFailure)}, // nobody is watching
		"timebase_info":                     {1, machTimebaseInfo}, "generate_activity_id": {3, machGenerateActivityID},
		"host_create_mach_voucher": {4, portOut(3)}, "swtch_pri": {1, sysYield}, "swtch": {0, sysYield}, "thread_switch": {3, sysYield},
		"mk_timer_create": {0, newPort}, "mk_timer_destroy": {1, constant(0)}, "mk_timer_arm": {2, constant(0)},
		"mk_timer_arm_leeway": {4, constant(0)}, "mk_timer_cancel": {2, constant(0)}, "semaphore_signal": {1, machSemaphoreSignal},
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

func (p *Process) vmAllocate(addrp, size, mask, flags uint64, name string) int64 {
	start, ok := p.vmMap(p.u64(addrp), size, mask, flags, allocation{name: name})
	if !ok {
		return kernNoSpace
	}
	p.write(addrp, le64(start))
	return 0
}

func machVMAllocate(p *Process, a []uint64) int64 {
	return p.vmAllocate(a[1], a[2], 0, a[3], "vm_allocate")
}
func machVMMap(p *Process, a []uint64) int64 { return p.vmAllocate(a[1], a[2], a[3], a[4], "vm_map") }

func machVMDeallocate(p *Process, a []uint64) int64 { return sysMunmap(p, a[1:]) }

func machPortType(p *Process, a []uint64) int64 {
	p.write(a[2], le32(0x20000))
	return 0
}

func machTimebaseInfo(p *Process, a []uint64) int64 {
	p.write(a[0], le32(tickNanos, 1))
	return 0
}

func machGenerateActivityID(p *Process, a []uint64) int64 {
	p.write(a[2], le64(0x1000))
	return 0
}

// machPortConstruct makes a port and keeps the context it is given, which its owner reads back.
func machPortConstruct(p *Process, a []uint64) int64 {
	port := p.newPort()
	p.write(a[3], le32(port))
	p.contexts[port] = a[2]
	return 0
}

// machPortGuard guards a port with a value, which is also its context.
func machPortGuard(p *Process, a []uint64) int64 {
	p.contexts[uint32(a[1])] = a[2]
	return 0
}

func migPortGetContext(p *Process, body []byte) migReply {
	port := le.Uint32(body[32:])
	p.note("  mach_port_get_context %#x = %#x", port, p.contexts[port])
	return migData(le64(p.contexts[port]))
}

func machThreadSelf(p *Process, a []uint64) int64 { return int64(p.current.port) }

func machPidForTask(p *Process, a []uint64) int64 {
	p.write(a[1], le32(guestPID))
	return 0
}

// message is one mach_msg call in the form all three traps reduce to.
type message struct {
	msg, option, sendSize, rcvSize, rcvMsg uint64
	rcvName                                uint32
	inline                                 bool // mach_msg2 passes the header fields in registers
	remote, local, id                      uint32
}

func machMsg(p *Process, a []uint64) int64 {
	return p.doMsg(message{msg: a[0], option: a[1], sendSize: a[2], rcvSize: a[3], rcvMsg: a[0], rcvName: uint32(a[4])})
}

func machMsgOverwrite(p *Process, a []uint64) int64 {
	m := message{msg: a[0], option: a[1], sendSize: a[2], rcvSize: a[3], rcvMsg: a[7], rcvName: uint32(a[4])}
	if m.rcvMsg == 0 {
		m.rcvMsg = m.msg
	}
	return p.doMsg(m)
}

// machMsg2: x2 = msgh_bits | send_size<<32; x3 = remote | local<<32; x4 = voucher | id<<32;
// x5 = desc_count | rcv_name<<32; x6 = rcv_size | priority<<32
func machMsg2(p *Process, a []uint64) int64 {
	return p.doMsg(message{msg: a[0], option: a[1] & 0xFFFFFFFF, sendSize: a[2] >> 32, rcvSize: a[6] & 0xFFFFFFFF, rcvMsg: a[0],
		rcvName: uint32(a[5] >> 32), inline: true, remote: uint32(a[3]), local: uint32(a[3] >> 32), id: uint32(a[4] >> 32)})
}

func header(bits, size, local, id uint32) []byte { return le32(bits, size, 0, local, 0, id) }

func (p *Process) doMsg(m message) int64 {
	if m.option&1 == 0 { // receive only
		if m.option&0x100 != 0 {
			p.note("  mach_msg receive with timeout on %#x", m.rcvName)
			return rcvTimedOut
		}
		// Nobody will ever answer: there are no other threads or services. Deliver what the kernel
		// sends when a reply right dies unanswered, so the waiter sees an interrupted connection.
		p.note("  mach_msg receive on %#x would block: send-once notification", m.rcvName)
		p.write(m.rcvMsg, append(header(0x1200, 24, m.rcvName, 71), make([]byte, 32)...)) // MACH_NOTIFY_SEND_ONCE
		return 0
	}
	if !m.inline {
		h := p.read(m.msg, 24)
		m.remote, m.local, m.id = le.Uint32(h[8:]), le.Uint32(h[12:]), le.Uint32(h[20:])
	}
	body := p.read(m.msg, max(m.sendSize, 24))
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
	reply := routine(p, body)
	if m.option&2 == 0 {
		return 0
	}
	bits := uint32(0x1200)
	if reply.port {
		bits |= 0x80000000 // complex: one port descriptor
	}
	p.write(m.rcvMsg, append(header(bits, uint32(24+len(reply.body)), m.local, m.id+100), reply.body...))
	return 0
}

// ---- MIG routines ----

type migReply struct {
	body []byte
	port bool
}

var ndr = []byte{0, 0, 0, 0, 1, 0, 0, 0}

// migCode is a reply carrying only a return code.
func migCode(code uint32) migReply {
	return migReply{body: append(append([]byte{}, ndr...), le32(code)...)}
}

// migData is a successful reply with out-parameters.
func migData(out []byte) migReply { return migReply{body: append(migCode(0).body, out...)} }

// migPort is a reply moving one port right.
func migPort(name uint32) migReply {
	return migReply{body: append(le32(1, name, 0), 0, 0, 17, 0), port: true}
}

var migRoutines map[uint32]func(p *Process, body []byte) migReply

func init() {
	ok := func(*Process, []byte) migReply { return migCode(0) }
	freshPort := func(p *Process, _ []byte) migReply { return migPort(p.newPort()) }
	migRoutines = map[uint32]func(*Process, []byte) migReply{
		200:  migHostInfo,
		202:  func(*Process, []byte) migReply { return migData(le64(pageSize)) }, // host_page_size
		206:  freshPort,                                                          // host_get_clock_service
		1000: migClockGetTime,
		3405: migTaskInfo,
		3228: migPortGetContext,
		3409: migTaskGetSpecialPort,
		3410: ok,        // task_set_special_port
		3418: freshPort, // semaphore_create
		4811: migVMMap,
		4813: migVMRemap,
		8000: ok, // task_restartable_ranges_register
		8001: ok,
	}
}

// migTaskGetSpecialPort hands out only a (dead-end) bootstrap port.
func migTaskGetSpecialPort(p *Process, body []byte) migReply {
	which := int32(le.Uint32(body[32:]))
	p.note("  task_get_special_port %d", which)
	if which == 4 {
		return migPort(bootstrapPort)
	}
	return migPort(0)
}

func migVMMap(p *Process, body []byte) migReply { // mach_vm_map
	addr, size, mask, flags := le.Uint64(body[48:]), le.Uint64(body[56:]), le.Uint64(body[64:]), uint64(le.Uint32(body[72:]))
	start, ok := p.vmMap(addr, size, mask, flags, allocation{name: "vm_map"})
	if !ok {
		return migCode(kernNoSpace)
	}
	p.note("  mach_vm_map(%#x, %#x, flags %#x) = %#x", addr, size, flags, start)
	return migData(le64(start))
}

// migVMRemap (mach_vm_remap) gives the caller a second mapping of file-backed memory. It is a
// copy made now, which is the same thing as long as neither mapping is written afterwards: true
// of code, which is what gets remapped (the Objective-C runtime's block trampolines).
func migVMRemap(p *Process, body []byte) migReply {
	addr, size, mask, flags := le.Uint64(body[48:]), le.Uint64(body[56:]), le.Uint64(body[64:]), uint64(le.Uint32(body[72:]))
	source, copied := le.Uint64(body[76:]), le.Uint32(body[84:]) != 0
	r := p.findRegion(source)
	if r == nil || r.end < source+size || r.kind == zeroRegion && !copied {
		p.note("  mach_vm_remap of %#x size %#x: not file-backed memory", source, size)
		return migCode(kernInvalidArg)
	}
	start, ok := p.vmMap(addr, size, mask, flags, allocation{kind: fileRegion, data: p.read(source, size), name: "vm_remap:" + r.name})
	if !ok {
		return migCode(kernNoSpace)
	}
	p.note("  mach_vm_remap(%#x, %#x) = %#x", source, size, start)
	return migData(append(le64(start), le32(5, 7)...)) // current and maximum protection
}

func migClockGetTime(*Process, []byte) migReply {
	now := time.Now()
	return migData(le32(uint32(now.Unix()), uint32(now.Nanosecond())))
}

// counted is a MIG variable-length array of 32-bit words: its count, then the words.
func counted(info []byte) migReply { return migData(append(le32(uint32(len(info)/4)), info...)) }

func migHostInfo(p *Process, body []byte) migReply {
	flavor, count := int32(le.Uint32(body[32:])), uint64(le.Uint32(body[36:]))
	var info []byte
	switch flavor {
	case 1: // HOST_BASIC_INFO
		info = append(le32(1, 1, 0, cpuARM64, cpuSubtypeARM64E, 0x10000, cpuARM64, 0, 0, 1), le64(guestMemory)...)
	case 5: // HOST_PRIORITY_INFO
		info = le32(0, 31, 31, 31, 0, 31, 0, 127)
	default:
		return migCode(kernInvalidArg)
	}
	return counted(info[:min(count*4, uint64(len(info)))])
}

func migTaskInfo(p *Process, body []byte) migReply {
	flavor := int32(le.Uint32(body[32:]))
	p.note("  task_info flavor %d", flavor)
	switch flavor {
	case 17: // TASK_DYLD_INFO
		return counted(append(le64(p.imageInfo[:]...), le32(1)...))
	case 15: // TASK_AUDIT_TOKEN
		return counted(le32(0, guestUID, guestGID, guestUID, guestGID, guestPID, 100000, 1))
	}
	return migCode(kernInvalidArg)
}
