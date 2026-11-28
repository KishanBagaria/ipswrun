// The status the kernel reports of a receive right (mach_port_get_attributes), as the port is
// sent to, received from, limited, guarded and asked to notify: what a daemon reads of its
// service's port as it checks in. It prints the same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <stdio.h>
#include <string.h>

static mach_port_t self;

static void status(const char *what, mach_port_t name) {
	mach_port_status_t s;
	mach_msg_type_number_t count = MACH_PORT_RECEIVE_STATUS_COUNT;
	kern_return_t kr = mach_port_get_attributes(self, name, MACH_PORT_RECEIVE_STATUS, (mach_port_info_t)&s, &count);
	if (kr) {
		printf("%s: error %#x\n", what, kr);
		return;
	}
	printf("%s: in a set %d, seqno %u, mscount %u, qlimit %u, msgcount %u, sorights %u, srights %u, pdrequest %u, nsrequest %u, flags %#x\n", what,
	       s.mps_pset != 0, s.mps_seqno, s.mps_mscount, s.mps_qlimit, s.mps_msgcount, s.mps_sorights, s.mps_srights, s.mps_pdrequest, s.mps_nsrequest, s.mps_flags);
}

// send sends an empty message to a port, with a right to answer on another if one is given.
static void send(mach_port_t to, mach_msg_type_name_t how, mach_port_t reply, mach_msg_type_name_t reply_how) {
	mach_msg_header_t m = {.msgh_bits = MACH_MSGH_BITS(how, reply_how), .msgh_size = sizeof m, .msgh_remote_port = to, .msgh_local_port = reply, .msgh_id = 7};
	kern_return_t kr = mach_msg(&m, MACH_SEND_MSG, sizeof m, 0, MACH_PORT_NULL, 0, MACH_PORT_NULL);
	if (kr)
		printf("send: %#x\n", kr);
}

// receive takes the next message of a port, and returns the right it came with to answer on.
static mach_port_t receive(mach_port_t from, const char *what) {
	struct {
		mach_msg_header_t h;
		char more[64];
		mach_msg_seqno_trailer_t t;
	} m;
	memset(&m, 0, sizeof m);
	kern_return_t kr = mach_msg(&m.h, MACH_RCV_MSG | MACH_RCV_TIMEOUT | MACH_RCV_TRAILER_ELEMENTS(MACH_RCV_TRAILER_SEQNO), 0, sizeof m, from, 0, MACH_PORT_NULL);
	if (kr) {
		printf("%s: receive %#x\n", what, kr);
		return 0;
	}
	mach_msg_seqno_trailer_t *t = (mach_msg_seqno_trailer_t *)((char *)&m.h + m.h.msgh_size);
	printf("%s: received id %d, %u bytes, seqno %u\n", what, m.h.msgh_id, m.h.msgh_size, t->msgh_seqno);
	return m.h.msgh_remote_port;
}

int main(void) {
	self = mach_task_self();
	setvbuf(stdout, NULL, _IOLBF, 0); // a line at a time, as a guard broken would end this at once
	mach_port_t a = 0, b = 0, set = 0, once = 0, previous = 0, g = 0;
	mach_port_allocate(self, MACH_PORT_RIGHT_RECEIVE, &a);
	status("a new port", a);
	mach_port_insert_right(self, a, a, MACH_MSG_TYPE_MAKE_SEND);
	status("with a send right", a);
	send(a, MACH_MSG_TYPE_COPY_SEND, 0, 0);
	send(a, MACH_MSG_TYPE_COPY_SEND, 0, 0);
	status("sent two messages", a);
	receive(a, "the first");
	status("received one", a);
	receive(a, "the second");
	status("received both", a);
	send(a, MACH_MSG_TYPE_MAKE_SEND_ONCE, 0, 0);
	status("sent one through a right to send once", a);
	receive(a, "that one");
	status("and received it", a);

	// A right to send once to a second port, which arrives as the right to answer a message.
	mach_port_allocate(self, MACH_PORT_RIGHT_RECEIVE, &b);
	send(a, MACH_MSG_TYPE_COPY_SEND, b, MACH_MSG_TYPE_MAKE_SEND_ONCE);
	status("the second port, a right to send to it once on its way in a message", b);
	once = receive(a, "the message carrying it");
	status("the second port, that right held", b);
	send(once, MACH_MSG_TYPE_MOVE_SEND_ONCE, 0, 0);
	status("the second port, sent to through it", b);
	receive(b, "what was sent through it");
	status("the second port, that received", b);
	send(a, MACH_MSG_TYPE_COPY_SEND, b, MACH_MSG_TYPE_MAKE_SEND_ONCE);
	once = receive(a, "another message carrying one");
	printf("giving the right up unused: %#x\n", mach_port_deallocate(self, once));
	status("the second port, told that it was given up", b);
	receive(b, "the notification of that");
	status("the second port, that received", b);

	// Notifications asked for are rights to send once, held by the kernel until they are due.
	printf("asking to be told of no senders: %#x\n", mach_port_request_notification(self, a, MACH_NOTIFY_NO_SENDERS, 0, b, MACH_MSG_TYPE_MAKE_SEND_ONCE, &previous));
	status("the first port, asked of", a);
	status("the second port, to be told", b);
	printf("asking to be told of its name dying: %#x\n", mach_port_request_notification(self, a, MACH_NOTIFY_DEAD_NAME, 0, b, MACH_MSG_TYPE_MAKE_SEND_ONCE, &previous));
	status("the second port, to be told of both", b);
	printf("asking to be told of its destruction: %#x\n", mach_port_request_notification(self, a, MACH_NOTIFY_PORT_DESTROYED, 0, b, MACH_MSG_TYPE_MAKE_SEND_ONCE, &previous));
	status("the first port, asked of that too", a);
	status("the second port, to be told of all three", b);

	mach_port_limits_t limits = {.mpl_qlimit = 3};
	printf("setting a limit of 3: %#x\n", mach_port_set_attributes(self, a, MACH_PORT_LIMITS_INFO, (mach_port_info_t)&limits, MACH_PORT_LIMITS_INFO_COUNT));
	status("with a limit of 3", a);
	mach_msg_type_number_t count = MACH_PORT_LIMITS_INFO_COUNT;
	limits.mpl_qlimit = 99;
	kern_return_t kr = mach_port_get_attributes(self, a, MACH_PORT_LIMITS_INFO, (mach_port_info_t)&limits, &count);
	printf("its limit, asked for by itself: %#x, %u, count %u\n", kr, limits.mpl_qlimit, count);
	limits.mpl_qlimit = 5000;
	printf("setting a limit of 5000: %#x\n", mach_port_set_attributes(self, a, MACH_PORT_LIMITS_INFO, (mach_port_info_t)&limits, MACH_PORT_LIMITS_INFO_COUNT));
	mach_port_allocate(self, MACH_PORT_RIGHT_PORT_SET, &set);
	mach_port_insert_member(self, a, set);
	status("in a set", a);
	mach_port_deallocate(self, a);
	status("the first port, its send right given up", a);
	status("the second port, told of no senders", b);
	receive(b, "the notification of no senders");
	status("the second port, that received", b);

	mach_port_options_t plain = {0}, guarded = {.flags = MPO_CONTEXT_AS_GUARD | MPO_QLIMIT | MPO_INSERT_SEND_RIGHT, .mpl = {.mpl_qlimit = 2}},
	                    strict = {.flags = MPO_CONTEXT_AS_GUARD | MPO_STRICT}, importance = {.flags = MPO_IMPORTANCE_RECEIVER},
	                    temporary = {.flags = MPO_TEMPOWNER | MPO_INSERT_SEND_RIGHT};
	mach_port_construct(self, &plain, 0, &g);
	status("constructed with no options", g);
	mach_port_construct(self, &guarded, 0x1234, &g);
	status("constructed guarded, with a limit of 2 and a send right", g);
	mach_port_construct(self, &strict, 0x1234, &g);
	status("constructed strictly guarded", g);
	mach_port_construct(self, &importance, 0, &g);
	status("constructed to receive importance", g);
	mach_port_construct(self, &temporary, 0, &g);
	status("constructed with a temporary owner", g);
	mach_port_allocate(self, MACH_PORT_RIGHT_RECEIVE, &g);
	printf("guarding a port: %#x\n", mach_port_guard(self, g, 0x55, 0));
	status("guarded", g);
	printf("guarding it again: %#x\n", mach_port_guard(self, g, 0x55, 1));
	status("guarded twice", g);
	printf("unguarding it: %#x\n", mach_port_unguard(self, g, 0x55));
	status("unguarded", g);
	printf("guarding it strictly: %#x\n", mach_port_guard(self, g, 0x55, 1));
	status("strictly guarded", g);

	status("the port set", set);
	status("the task's own port", self);
	status("no name", 0x12345);
	mach_port_status_t s;
	count = 5;
	printf("too little room: %#x\n", mach_port_get_attributes(self, b, MACH_PORT_RECEIVE_STATUS, (mach_port_info_t)&s, &count));
	int room[40];
	count = 40;
	kr = mach_port_get_attributes(self, b, MACH_PORT_RECEIVE_STATUS, (mach_port_info_t)room, &count);
	printf("more room than it takes: %#x, count %u\n", kr, count);
	count = 10;
	printf("a flavor there is not: %#x\n", mach_port_get_attributes(self, b, 99, (mach_port_info_t)&s, &count));
	return 0;
}
