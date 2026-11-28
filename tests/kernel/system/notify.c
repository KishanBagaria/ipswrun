// Notifications between processes, kept by the system's notifyd: a name is registered for, has
// a state that is set and read, and is posted to those who wait for it. Under ipswrun the
// daemon is the release's own, started when it is first asked for (--on-demand). It prints the
// same natively on a Mac as under ipswrun.
#include <dispatch/dispatch.h>
#include <mach/mach.h>
#include <notify.h>
#include <stdint.h>
#include <stdio.h>
#include <unistd.h>

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char name[96];
	snprintf(name, sizeof name, "run.ipsw.example.notify.%d", getpid());

	// A name's state, and whether it has been posted since it was last asked about.
	int token = -1, other = -1, check = -1;
	uint64_t state = 99;
	printf("register to check: %u", notify_register_check(name, &token));
	printf("; a token: %d; valid: %d\n", token >= 0, notify_is_valid_token(token));
	printf("check: %u", notify_check(token, &check));
	printf(", posted since: %d", check);
	printf("; again: %u", notify_check(token, &check));
	printf(", %d\n", check);
	printf("its state: %u", notify_get_state(token, &state));
	printf(", %llu; set to 42: %u", state, notify_set_state(token, 42));
	printf("; read through a second registration: %u", notify_register_check(name, &other));
	printf(", %u", notify_get_state(other, &state));
	printf(", %llu\n", state);
	printf("post: %u", notify_post(name));
	usleep(100000); // the daemon is told of it, and says so, in its own time
	printf("; check: %u", notify_check(token, &check));
	printf(", posted since: %d", check);
	printf("; again: %u", notify_check(token, &check));
	printf(", %d\n", check);

	// One who waits is told: a block on a queue, and a message to a port.
	dispatch_semaphore_t told = dispatch_semaphore_create(0);
	dispatch_queue_t queue = dispatch_queue_create("notify", NULL);
	__block int waiting = -1;
	__block int times = 0;
	printf("register a block: %u\n", notify_register_dispatch(name, &waiting, queue, ^(int t) {
		       uint64_t s = 0;
		       notify_get_state(t, &s);
		       printf("  told, by its own token: %d; the state is %llu\n", t == waiting, s);
		       times++;
		       dispatch_semaphore_signal(told);
	       }));
	notify_set_state(token, 43);
	printf("post: %u\n", notify_post(name));
	long late = dispatch_semaphore_wait(told, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC));
	printf("the block ran: %d, %d times\n", late == 0, times);
	mach_port_t port = MACH_PORT_NULL;
	int ported = -1;
	printf("register a port: %u", notify_register_mach_port(name, &port, 0, &ported));
	printf("; a port: %d\n", MACH_PORT_VALID(port));
	printf("post: %u\n", notify_post(name));
	struct {
		mach_msg_header_t header;
		char rest[64];
	} message = {};
	kern_return_t kr = mach_msg(&message.header, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, sizeof message, port, 5000, MACH_PORT_NULL);
	printf("a message came: %#x, saying its token: %d\n", kr, message.header.msgh_id == ported);
	dispatch_semaphore_wait(told, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC));

	// Suspended, a registration is told once when it is resumed, however often it was posted.
	printf("suspend: %u", notify_suspend(waiting));
	notify_post(name);
	notify_post(name);
	late = dispatch_semaphore_wait(told, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC / 5));
	printf("; told while suspended: %d; resume: %u\n", late == 0, notify_resume(waiting));
	late = dispatch_semaphore_wait(told, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC));
	usleep(100000);
	printf("told after: %d, in all %d times\n", late == 0, times);

	// Cancelled, a token is no token, and is told nothing.
	printf("cancel: %u, %u, %u and %u", notify_cancel(waiting), notify_cancel(ported), notify_cancel(other), notify_cancel(token));
	printf("; valid: %d; its state: %u; cancel again: %u\n", notify_is_valid_token(token), notify_get_state(token, &state), notify_cancel(token));
	printf("post to nobody: %u\n", notify_post(name));
	return 0;
}
