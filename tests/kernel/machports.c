// The ports the kernel answers on: the task's own, the host's, a clock's and a thread's. Each is
// a right to send that is counted like any other, each answers only what is asked of its kind,
// and a message to a name that is no right to send is refused. It prints the same natively on a
// Mac as under ipswrun.
#include <mach/clock.h>
#include <mach/host_priv.h>
#include <mach/mach.h>
#include <mach/mach_traps.h>
#include <stdio.h>
#include <unistd.h>

static int refs(mach_port_t name, mach_port_right_t right) {
	mach_port_urefs_t n = 0;
	kern_return_t r = mach_port_get_refs(mach_task_self(), name, right, &n);
	return r ? -(int)r : (int)n;
}

static const char *kind(mach_port_t name) {
	static char text[4][48];
	static int next;
	char *t = text[next++ % 4];
	mach_port_type_t type = 0;
	kern_return_t r = mach_port_type(mach_task_self(), name, &type);
	if (r)
		snprintf(t, 48, "refused (%d)", r);
	else
		snprintf(t, 48, "%#x", type);
	return t;
}

// asked is what a port says to a request of the host's (host_info).
static int asked(mach_port_t of) {
	host_basic_info_data_t info;
	mach_msg_type_number_t count = HOST_BASIC_INFO_COUNT;
	return host_info(of, HOST_BASIC_INFO, (host_info_t)&info, &count);
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	mach_port_t task = mach_task_self(), host = mach_host_self();
	struct mach_task_basic_info basic;
	mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;

	// The task's own port: asked for again it is the same name, held once more each time.
	int before = refs(task, MACH_PORT_RIGHT_SEND);
	mach_port_t again = task_self_trap(), third = task_self_trap();
	printf("the task's port: a right of kind %s; asked for twice more it is the same name %d, held %d more times", kind(task), again == task && third == task, refs(task, MACH_PORT_RIGHT_SEND) - before);
	mach_port_deallocate(task, task);
	mach_port_deallocate(task, task);
	printf("; given up twice, %d more\n", refs(task, MACH_PORT_RIGHT_SEND) - before);
	printf("counted as a right to receive: %d; to send once: %d; as a dead name: %d\n", refs(task, MACH_PORT_RIGHT_RECEIVE), refs(task, MACH_PORT_RIGHT_SEND_ONCE), refs(task, MACH_PORT_RIGHT_DEAD_NAME));
	printf("held more by mach_port_mod_refs: %d", mach_port_mod_refs(task, task, MACH_PORT_RIGHT_SEND, 3));
	printf(", %d more times; less by as many: %d", refs(task, MACH_PORT_RIGHT_SEND) - before, mach_port_mod_refs(task, task, MACH_PORT_RIGHT_SEND, -3));
	printf(", %d more\n", refs(task, MACH_PORT_RIGHT_SEND) - before);

	// The host's.
	before = refs(host, MACH_PORT_RIGHT_SEND);
	again = mach_host_self();
	printf("the host's port: a right of kind %s, not the task's %d; asked for again it is the same name %d, held %d more times", kind(host), host != task, again == host, refs(host, MACH_PORT_RIGHT_SEND) - before);
	mach_port_deallocate(task, again);
	printf("; given up once, %d more\n", refs(host, MACH_PORT_RIGHT_SEND) - before);
	mach_port_t special = 0;
	kern_return_t r = host_get_special_port(host, HOST_LOCAL_NODE, HOST_PORT, &special);
	printf("asked for as the host's special port: %d, the same name %d, held %d more times\n", r, special == host, refs(host, MACH_PORT_RIGHT_SEND) - before);
	if (special == host)
		mach_port_deallocate(task, special);

	// Each answers what is asked of its kind, and not what is another's.
	printf("what the host is asked of the host: %d; of the task: %d\n", asked(host), asked(task));
	printf("what the task holds asked of the task: %d; of the host: %d\n", task_info(task, MACH_TASK_BASIC_INFO, (task_info_t)&basic, &count), task_info(host, MACH_TASK_BASIC_INFO, (task_info_t)&basic, &count));

	// A clock, which the host gives.
	clock_serv_t clock = 0, clockAgain = 0;
	mach_timespec_t now;
	printf("a clock from the host: %d", host_get_clock_service(host, SYSTEM_CLOCK, &clock));
	before = refs(clock, MACH_PORT_RIGHT_SEND);
	printf(", a right of kind %s; again: %d", kind(clock), host_get_clock_service(host, SYSTEM_CLOCK, &clockAgain));
	printf(", the same name %d, held %d more times\n", clockAgain == clock, refs(clock, MACH_PORT_RIGHT_SEND) - before);
	printf("the time asked of the clock: %d; of the task: %d; of the host: %d; what the host is asked of the clock: %d\n", clock_get_time(clock, &now), clock_get_time(task, &now), clock_get_time(host, &now),
	       asked(clock));
	printf("a clock asked of the task: %d\n", host_get_clock_service(task, SYSTEM_CLOCK, &clockAgain));

	// A thread's.
	mach_port_t thread = mach_thread_self();
	before = refs(thread, MACH_PORT_RIGHT_SEND);
	again = mach_thread_self();
	printf("the thread's port: a right of kind %s; asked for again it is the same name %d, held %d more times; what the host is asked of it: %d\n", kind(thread), again == thread,
	       refs(thread, MACH_PORT_RIGHT_SEND) - before, asked(thread));
	mach_port_deallocate(task, again);

	// What is no right to send.
	mach_port_t own = 0, gone = 0;
	mach_port_allocate(task, MACH_PORT_RIGHT_RECEIVE, &own);
	mach_port_allocate(task, MACH_PORT_RIGHT_RECEIVE, &gone);
	mach_port_destroy(task, gone);
	printf("what the host is asked of a name never given: %#x; of none: %#x; of a dead one: %#x; of a port that is gone: %#x; of one the process can only receive from: %#x\n", asked(0x7ffff03),
	       asked(MACH_PORT_NULL), asked(MACH_PORT_DEAD), asked(gone), asked(own));
	printf("how often a name never given is held: %d; one that is gone: %d; a port the process can only receive from, to send: %d, to receive: %d\n", refs(0x7ffff03, MACH_PORT_RIGHT_SEND),
	       refs(gone, MACH_PORT_RIGHT_SEND), refs(own, MACH_PORT_RIGHT_SEND), refs(own, MACH_PORT_RIGHT_RECEIVE));
	mach_port_insert_right(task, own, own, MACH_MSG_TYPE_MAKE_SEND);
	printf("with a right to send made for it: to send %d, to receive %d, of kind %s\n", refs(own, MACH_PORT_RIGHT_SEND), refs(own, MACH_PORT_RIGHT_RECEIVE), kind(own));
	mach_port_destroy(task, own);
	return 0;
}
