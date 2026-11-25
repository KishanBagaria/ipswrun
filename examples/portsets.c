// A port in several port sets at once: a message to it is for whichever set is received from
// first. It prints the same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <stdio.h>
#include <string.h>
static mach_port_t self;
static void send(mach_port_t to, int id) { mach_msg_header_t m = {.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_MAKE_SEND, 0), .msgh_size = sizeof m, .msgh_remote_port = to, .msgh_id = id}; mach_msg(&m, MACH_SEND_MSG, sizeof m, 0, 0, 0, 0); }
static void rcv(const char *what, mach_port_t from) { struct { mach_msg_header_t h; char pad[64]; } m; memset(&m, 0, sizeof m);
	kern_return_t kr = mach_msg(&m.h, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, sizeof m, from, 0, 0);
	if (kr) printf("%s: nothing (%#x)\n", what, kr); else printf("%s: message %d, from the port: %d\n", what, m.h.msgh_id, 1); }
static int inset(mach_port_t p) { mach_port_status_t s; mach_msg_type_number_t c = MACH_PORT_RECEIVE_STATUS_COUNT; mach_port_get_attributes(self, p, MACH_PORT_RECEIVE_STATUS, (mach_port_info_t)&s, &c); return s.mps_pset != 0; }
int main(void) { self = mach_task_self(); mach_port_t p, a, b;
	mach_port_allocate(self, MACH_PORT_RIGHT_RECEIVE, &p); mach_port_allocate(self, MACH_PORT_RIGHT_PORT_SET, &a); mach_port_allocate(self, MACH_PORT_RIGHT_PORT_SET, &b);
	printf("insert into the first set: %#x; into the second: %#x; in a set: %d\n", mach_port_insert_member(self, p, a), mach_port_insert_member(self, p, b), inset(p));
	send(p, 1); rcv("the first set", a); rcv("the second set", b);
	send(p, 2); rcv("the second set", b); rcv("the first set", a);
	printf("take it out of the first: %#x; in a set: %d\n", mach_port_extract_member(self, p, a), inset(p));
	send(p, 3); rcv("the first set", a); rcv("the second set", b);
	printf("take it out of the first again: %#x\n", mach_port_extract_member(self, p, a));
	printf("insert into the first again: %#x, twice: %#x\n", mach_port_insert_member(self, p, a), mach_port_insert_member(self, p, a));
	printf("move it to the second: %#x\n", mach_port_move_member(self, p, b));
	send(p, 4); rcv("the first set", a); rcv("the second set", b);
	printf("move it to no set: %#x; in a set: %d\n", mach_port_move_member(self, p, MACH_PORT_NULL), inset(p));
	send(p, 5); rcv("the second set", b); rcv("the port itself", p);
	return 0; }
