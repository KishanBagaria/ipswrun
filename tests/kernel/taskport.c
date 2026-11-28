// The port a process's own task may be read by (task_read_for_pid of itself), as ps asks for
// each process: a second name for the task, by which it can be asked about and not changed.
// And what kind of right each name a process has is (mach_port_type), and which names are a
// task's (pid_for_task). It prints the same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <stdio.h>
#include <unistd.h>

extern kern_return_t task_read_for_pid(mach_port_name_t, int, mach_port_name_t *);

static void named(const char *what, mach_port_name_t name) {
	mach_port_type_t type = 0;
	int pid = -5;
	kern_return_t typed = mach_port_type(mach_task_self(), name, &type), asked = pid_for_task(name, &pid);
	printf("%s: ", what);
	if (typed)
		printf("no name (%d)", typed);
	else
		printf("%s%s%s%s", type & MACH_PORT_TYPE_SEND ? "a right to send " : "", type & MACH_PORT_TYPE_RECEIVE ? "a right to receive " : "", type & MACH_PORT_TYPE_PORT_SET ? "a set of ports " : "",
		       type & ~(MACH_PORT_TYPE_SEND | MACH_PORT_TYPE_RECEIVE | MACH_PORT_TYPE_PORT_SET) ? "and more " : "");
	printf("; as a task: %d, its pid %s\n", asked, pid == getpid() ? "this process's" : pid == -1 ? "-1" : "another");
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	mach_port_name_t read = 0, again = 0, received = 0, set = 0;
	printf("task_read_for_pid of this process: %d", task_read_for_pid(mach_task_self(), getpid(), &read));
	printf("; a name of its own %d; asked again: %d, the same name %d\n", read != 0 && read != mach_task_self(), task_read_for_pid(mach_task_self(), getpid(), &again), again == read);
	named("the task's own port", mach_task_self());
	named("the port it may be read by", read);
	named("the host's", mach_host_self());
	named("this thread's", mach_thread_self());
	named("the bootstrap port", bootstrap_port);
	mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &received);
	named("a port it made", received);
	mach_port_insert_right(mach_task_self(), received, received, MACH_MSG_TYPE_MAKE_SEND);
	named("with a right to send to it too", received);
	mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_PORT_SET, &set);
	named("a set of ports", set);
	named("a name it has not", 0x7654321);
	named("no name at all", 0);

	audit_token_t token = {{0}};
	mach_msg_type_number_t count = TASK_AUDIT_TOKEN_COUNT;
	printf("the task asked who it is by the port it may be read by: %d, this process %d\n", task_info(read, TASK_AUDIT_TOKEN, (task_info_t)&token, &count), token.val[5] == (unsigned)getpid());
	// It was given twice, so it is given up twice; asked for again, it is another name.
	printf("given up: %d, and still %s", mach_port_deallocate(mach_task_self(), read), "");
	named("there", read);
	printf("given up again: %d", mach_port_deallocate(mach_task_self(), read));
	printf("; asked for anew: %d, another name than before %d\n", task_read_for_pid(mach_task_self(), getpid(), &again), again != read && again != 0);
	return 0;
}
