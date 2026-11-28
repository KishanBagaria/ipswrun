// What a process hands on: to a copy of itself (fork), to a program it starts (posix_spawn), and
// to the program it becomes (execve). Each says what it has of what the first set: the mask for
// new files, a limit, a priority, an I/O policy, how it takes three signals, and a timer. It
// prints the same natively on a Mac as under ipswrun.
#include <mach-o/dyld.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

static void handler(int sig) {}

static void report(const char *who) {
	mode_t mask = umask(0);
	umask(mask);
	struct rlimit files;
	getrlimit(RLIMIT_NOFILE, &files);
	struct sigaction usr1, usr2;
	sigaction(SIGUSR1, NULL, &usr1);
	sigaction(SIGUSR2, NULL, &usr2);
	sigset_t blocked;
	sigprocmask(0, NULL, &blocked);
	struct itimerval timer;
	getitimer(ITIMER_REAL, &timer);
	printf("%s: umask %03o, files %llu, priority %d, disk policy %d, SIGUSR1 %s, SIGUSR2 %s, SIGTERM %s, a timer %s\n", who, mask, files.rlim_cur, getpriority(PRIO_PROCESS, 0),
	       getiopolicy_np(IOPOL_TYPE_DISK, IOPOL_SCOPE_PROCESS), usr1.sa_handler == SIG_IGN ? "ignored" : usr1.sa_handler == SIG_DFL ? "as it comes" : "handled",
	       usr2.sa_handler == SIG_IGN ? "ignored" : usr2.sa_handler == SIG_DFL ? "as it comes" : "handled", sigismember(&blocked, SIGTERM) ? "blocked" : "let in",
	       timer.it_value.tv_sec || timer.it_value.tv_usec ? "set" : "not set");
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc > 1) {
		report(argv[1]);
		return 0;
	}
	char self[1024];
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);

	umask(027);
	struct rlimit files = {200, 300};
	setrlimit(RLIMIT_NOFILE, &files);
	setpriority(PRIO_PROCESS, 0, 5);
	setiopolicy_np(IOPOL_TYPE_DISK, IOPOL_SCOPE_PROCESS, IOPOL_UTILITY);
	signal(SIGUSR1, SIG_IGN);
	signal(SIGUSR2, handler);
	sigset_t term;
	sigemptyset(&term);
	sigaddset(&term, SIGTERM);
	sigprocmask(SIG_BLOCK, &term, NULL);
	struct itimerval timer = {.it_value = {3600, 0}};
	setitimer(ITIMER_REAL, &timer, NULL);
	report("after it set them");

	pid_t pid = fork();
	if (pid == 0) {
		report("a copy of it");
		_exit(0);
	}
	waitpid(pid, NULL, 0);

	char *started[] = {self, "a program it starts", NULL};
	if (posix_spawn(&pid, self, NULL, NULL, started, environ) == 0)
		waitpid(pid, NULL, 0);

	if ((pid = fork()) == 0) {
		char *became[] = {self, "a program a copy of it becomes", NULL};
		execve(self, became, environ);
		_exit(1);
	}
	waitpid(pid, NULL, 0);

	char *becomes[] = {self, "the program it becomes", NULL};
	execve(self, becomes, environ);
	return 1;
}
