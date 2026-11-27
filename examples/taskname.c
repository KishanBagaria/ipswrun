// A task named and no more (task_name_for_pid), as logd and the tools that list processes have
// one: a right to a process's task that says who it is and little else. Of the process itself,
// of its child, of launchd and of no process, and what can be asked through it. It prints the
// same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <stdio.h>
#include <unistd.h>
#include <signal.h>
#include <sys/wait.h>
extern kern_return_t task_name_for_pid(mach_port_name_t target, int pid, mach_port_name_t *t);
int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	mach_port_name_t n = 77, again = 77;
	kern_return_t r = task_name_for_pid(mach_task_self(), getpid(), &n);
	kern_return_t r2 = task_name_for_pid(mach_task_self(), getpid(), &again);
	mach_port_type_t type = 0; mach_port_type(mach_task_self(), n, &type);
	printf("of itself: %#x, a name %d, the same name again %d (%#x), a right of type %#x, the task's own name %d\n", r, n != 0 && n != 77, again == n, r2, type, n == mach_task_self());
	int pid = 0; kern_return_t back = pid_for_task(n, &pid);
	printf("the pid it is of: %#x, this one's %d\n", back, pid == getpid());
	struct task_basic_info_64 info; mach_msg_type_number_t count = TASK_BASIC_INFO_64_COUNT;
	printf("task_info through it: %#x\n", task_info(n, TASK_BASIC_INFO_64, (task_info_t)&info, &count));
	n = 77; r = task_name_for_pid(mach_task_self(), 99999999, &n);
	printf("of no process: %#x, name %d\n", r, n);
	n = 77; r = task_name_for_pid(mach_task_self(), 1, &n);
	printf("of launchd: %#x, a name %d\n", r, n != 0);
	n = 77; r = task_name_for_pid(0x12345, getpid(), &n);
	printf("for a task that is none: %#x, name %d\n", r, n);
	pid_t child = fork();
	if (child == 0) { pause(); _exit(0); }
	n = 77; r = task_name_for_pid(mach_task_self(), child, &n);
	printf("of its child: %#x, a name %d\n", r, n != 0 && n != 77);
	kill(child, 9); int st; waitpid(child, &st, 0);
	return 0;
}
