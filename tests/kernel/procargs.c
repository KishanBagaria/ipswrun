// What a process was started with, as the kernel keeps it for whoever asks (the sysctl
// kern.procargs2, which ps reads a command line from), and the port by which a process's task
// may be read (task_read_for_pid), which a user is given for no process but its own. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <mach/mach.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/sysctl.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;
extern kern_return_t task_read_for_pid(mach_port_name_t, int, mach_port_name_t *);

static void started(const char *what, pid_t pid, size_t room, int buffer) {
	static char block[1 << 16];
	int mib[3] = {CTL_KERN, KERN_PROCARGS2, pid};
	size_t size = room;
	errno = 0;
	if (sysctl(mib, 3, buffer ? block : NULL, &size, NULL, 0)) {
		printf("%s: -1 (%s)\n", what, strerror(errno));
		return;
	}
	if (!buffer) {
		printf("%s: 0, room for it %d\n", what, size > 8);
		return;
	}
	int count;
	memcpy(&count, block, sizeof count);
	char *at = block + sizeof count, *end = block + size;
	size_t path = strlen(at);
	int zeros = 0;
	for (at += path; at < end && !*at; at++)
		zeros++;
	printf("%s: %d arguments; the program's path is a whole one %d, with zeros after it to a multiple of 8 %d;", what, count, block[4] == '/', zeros > 0 && (path + zeros) % 8 == 0);
	for (int i = 0; i < count && at < end; i++, at += strlen(at) + 1)
		printf(" [%s]", i == 0 ? "the program" : at);
	int variables = 0;
	for (; at < end && *at; at += strlen(at) + 1)
		variables += !strncmp(at, "FIRST=", 6) || !strncmp(at, "SECOND=", 7);
	printf("; then the environment, the two it was given among it %d, and a zero %d\n", variables == 2, at < end && !*at);
}

static void readable(const char *what, pid_t pid, mach_port_name_t task) {
	mach_port_name_t port = 0x5555;
	errno = 0;
	if (task_read_for_pid(task, pid, &port))
		printf("%s: -1 (%s), the port %s\n", what, strerror(errno), port == 0 ? "0" : port == 0x5555 ? "left as it was" : "another");
	else
		printf("%s: 0\n", what);
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc > 1 && !strcmp(argv[1], "wait"))
		for (;;)
			pause();
	setenv("FIRST", "1", 1);
	setenv("SECOND", "two words", 1);
	pid_t child = fork();
	if (child == 0) {
		char *with[] = {argv[0], "wait", "two words", "", NULL};
		execve(argv[0], with, environ);
		_exit(1);
	}
	usleep(200000); // it is the program by now
	started("a child started with three arguments", child, 1 << 16, 1);
	started("asked how much room it takes", child, 0, 0);
	started("into four bytes", child, 4, 1);
	started("launchd, which is root's", 1, 1 << 16, 1);
	started("no process", 99999, 1 << 16, 1);
	readable("the port to read the child's task by", child, mach_task_self());
	readable("launchd's", 1, mach_task_self());
	readable("no process's", 99999, mach_task_self());
	readable("asked with a task that is not the caller's", child, 0x9999);
	kill(child, SIGKILL);
	usleep(100000);
	started("the child, ended and not waited for", child, 1 << 16, 1);
	waitpid(child, NULL, 0);
	return 0;
}
