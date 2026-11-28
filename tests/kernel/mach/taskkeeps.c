// What a task has the kernel keep for it: its special ports, which it and what it starts are
// handed when they ask; the stretches of its code a thread is put back to the start of
// (restartable ranges), which a task registers once; and whether memory it has no use for now
// may be emptied (purgeable memory). It prints the same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <mach/task_special_ports.h>
#include <stdint.h>
#include <stdio.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

typedef struct {
	uint64_t location;
	uint16_t length, recovery_offs;
	uint32_t flags;
} range_t;
extern kern_return_t task_restartable_ranges_register(task_t, range_t *, mach_msg_type_number_t);
extern kern_return_t task_restartable_ranges_synchronize(task_t);
extern kern_return_t vm_purgable_control(vm_map_t, vm_address_t, int, int *);

int main(void) {
	task_t me = mach_task_self();
	static const int asked[] = {0, 1, 2, 3, 4, 7, 8, 11, 12};
	printf("special ports asked for:");
	for (unsigned i = 0; i < sizeof asked / sizeof *asked; i++) {
		mach_port_t port = 77;
		kern_return_t r = task_get_special_port(me, asked[i], &port);
		printf(" %d: %#x%s;", asked[i], r, r ? "" : port ? ", a port" : ", none");
	}
	printf("\n");

	mach_port_t mine = 0, first = 0, got = 0, only = 0;
	mach_port_allocate(me, MACH_PORT_RIGHT_RECEIVE, &mine);
	mach_port_insert_right(me, mine, mine, MACH_MSG_TYPE_MAKE_SEND);
	mach_port_allocate(me, MACH_PORT_RIGHT_RECEIVE, &only);
	task_get_special_port(me, TASK_BOOTSTRAP_PORT, &first);
	kern_return_t r1 = task_set_special_port(me, TASK_BOOTSTRAP_PORT, mine);
	task_get_special_port(me, TASK_BOOTSTRAP_PORT, &got);
	printf("where launchd is found, set to a port of one's own: %#x; asked for then, it is that port: %d", r1, got == mine);
	fflush(stdout);
	if (fork() == 0) {
		mach_port_t its = 0;
		kern_return_t r = task_get_special_port(mach_task_self(), TASK_BOOTSTRAP_PORT, &its);
		mach_port_type_t type = 0;
		mach_port_type(mach_task_self(), its, &type);
		printf("; a copy of the process is given: %#x, a right to send %d", r, (type & MACH_PORT_TYPE_SEND) != 0);
		fflush(stdout);
		_exit(0);
	}
	wait(NULL);
	r1 = task_set_special_port(me, TASK_BOOTSTRAP_PORT, MACH_PORT_NULL);
	got = 77;
	kern_return_t r2 = task_get_special_port(me, TASK_BOOTSTRAP_PORT, &got);
	printf("; set to none: %#x, and asked for: %#x, %s", r1, r2, got ? "a port" : "none");
	r1 = task_set_special_port(me, TASK_BOOTSTRAP_PORT, first);
	got = 0;
	task_get_special_port(me, TASK_BOOTSTRAP_PORT, &got);
	printf("; set back: %#x, the first again %d\n", r1, got == first);
	printf("set to one's own, each of the others:");
	for (int which = 0; which <= 12; which++)
		if (which != TASK_BOOTSTRAP_PORT)
			printf(" %d: %#x;", which, task_set_special_port(me, which, mine));
	got = 0;
	task_get_special_port(me, 10, &got);
	printf(" the tenth asked for is that port: %d\n", got == mine);
	r1 = task_set_special_port(me, TASK_BOOTSTRAP_PORT, 0x12345);
	r2 = task_set_special_port(me, TASK_BOOTSTRAP_PORT, only);
	printf("set to a name that is no right: %#x; to a port one may only receive from: %#x\n", r1, r2);

	range_t one = {(uint64_t)(uintptr_t)main, 16, 8, 0}, none = one, long_one = {(uint64_t)(uintptr_t)main, 5000, 8, 0}, far_back = {(uint64_t)(uintptr_t)main, 16, 5000, 0},
		flagged = {(uint64_t)(uintptr_t)main, 16, 8, 1}, two[2] = {{(uint64_t)(uintptr_t)main, 64, 8, 0}, {(uint64_t)(uintptr_t)main + 32, 16, 8, 0}};
	printf("ranges registered by a program that is running (the system's libraries have theirs): %#x; none: %#x; one longer than a page: %#x; one put back further than a page: %#x; one with a flag: %#x; two that run into each other: %#x\n",
	       task_restartable_ranges_register(me, &one, 1), task_restartable_ranges_register(me, &none, 0), task_restartable_ranges_register(me, &long_one, 1), task_restartable_ranges_register(me, &far_back, 1),
	       task_restartable_ranges_register(me, &flagged, 1), task_restartable_ranges_register(me, two, 2));
	printf("threads put back out of them: %#x; by a name that is no task's: %#x\n", task_restartable_ranges_synchronize(me), task_restartable_ranges_synchronize(mach_host_self()));

	char *plain = mmap(NULL, 16384, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
	int state = 77;
	r1 = vm_purgable_control(me, (vm_address_t)plain, 1, &state);
	printf("whether plain memory may be emptied: %#x, and nothing said %d", r1, state == 77);
	state = 1;
	printf("; set that it may: %#x", vm_purgable_control(me, (vm_address_t)plain, 0, &state));
	munmap(plain, 16384);
	state = 77;
	r1 = vm_purgable_control(me, (vm_address_t)plain, 1, &state);
	printf("; asked of memory that is not there: %#x; a thing to do that there is not: %#x\n", r1, vm_purgable_control(me, (vm_address_t)&state, 99, &state));
	vm_address_t made = 0;
	r1 = vm_allocate(me, &made, 16384, VM_FLAGS_ANYWHERE | VM_FLAGS_PURGABLE);
	state = 77;
	r2 = vm_purgable_control(me, made, 1, &state);
	printf("memory made to be emptied: %#x; whether it may be: %#x, %d", r1, r2, state);
	state = 1;
	r1 = vm_purgable_control(me, made, 0, &state);
	printf("; set that it may: %#x, and it had been %d", r1, state);
	state = 0;
	r1 = vm_purgable_control(me, made, 0, &state);
	printf("; set that it may not: %#x, and it had been set that it may or was emptied %d", r1, state == 1 || state == 2);
	state = 77;
	vm_purgable_control(me, made, 1, &state);
	printf("; and then %d\n", state);
	return 0;
}
