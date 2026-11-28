// Faults and timers as signals: memory that is not there or may not be touched, a trap, and a
// timer that runs out, each with a handler that is told of it and without one. The child is this
// same program, given a role to play. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <mach-o/dyld.h>
#include <setjmp.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;
static char self[1024];
static sigjmp_buf back;
static volatile char *page;
static volatile int alarms;

static void fault(int sig, siginfo_t *info, void *context) {
	ucontext_t *uc = context;
	printf("  handler: signal %d, code %d, at the page: %d, and the context says the same: %d\n", sig, info->si_code, (char *)info->si_addr == page,
	       (char *)uc->uc_mcontext->__es.__far == page);
	siglongjmp(back, 1);
}

static void trap(int sig, siginfo_t *info, void *context) {
	ucontext_t *uc = context;
	printf("  handler: signal %d, code %d, at the instruction that trapped: %d\n", sig, info->si_code, (unsigned long long)info->si_addr == uc->uc_mcontext->__ss.__pc);
	siglongjmp(back, 1);
}

// mend makes the page writable, for the write that faulted to be made again.
static void mend(int sig, siginfo_t *info, void *context) {
	printf("  handler: signal %d, code %d; the page is made writable\n", sig, info->si_code);
	mprotect((void *)page, 16384, PROT_READ | PROT_WRITE);
}

static void rang(int sig) { alarms++; }

static int child(const char *role) {
	if (!strcmp(role, "null"))
		return *(volatile int *)8;
	if (!strcmp(role, "readonly")) {
		char *p = mmap(NULL, 16384, PROT_READ, MAP_ANON | MAP_PRIVATE, -1, 0);
		p[0] = 1;
		return 2;
	}
	if (!strcmp(role, "trap"))
		__builtin_trap();
	if (!strcmp(role, "alarm")) {
		struct itimerval soon = {.it_value = {0, 30000}};
		setitimer(ITIMER_REAL, &soon, NULL);
		pause();
		return 3;
	}
	return 99;
}

static const char *ended(const char *role) {
	static char text[64];
	char *argv[] = {self, (char *)role, NULL};
	pid_t pid = 0;
	int status = -1;
	posix_spawn(&pid, self, NULL, NULL, argv, environ);
	waitpid(pid, &status, 0);
	if (WIFSIGNALED(status))
		snprintf(text, sizeof text, "ended by signal %d", WTERMSIG(status));
	else
		snprintf(text, sizeof text, "exited %d", WEXITSTATUS(status));
	return text;
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc > 1)
		return child(argv[1]);
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);

	// With nothing said of them, each ends the process.
	printf("a read of memory that is not there: %s\n", ended("null"));
	printf("a write to memory that may only be read: %s\n", ended("readonly"));
	printf("a trap: %s\n", ended("trap"));
	printf("a timer that runs out: %s\n", ended("alarm"));

	// With a handler, which is told where, and goes back to before it.
	struct sigaction action = {.sa_sigaction = fault, .sa_flags = SA_SIGINFO};
	sigaction(SIGSEGV, &action, NULL);
	sigaction(SIGBUS, &action, NULL);
	page = (char *)0x7000;
	if (!sigsetjmp(back, 1))
		printf("read %d\n", page[0]);
	else
		printf("back from a read of memory that is not there\n");
	page = mmap(NULL, 16384, PROT_READ, MAP_ANON | MAP_PRIVATE, -1, 0);
	if (!sigsetjmp(back, 1)) {
		page[0] = 1;
		printf("wrote\n");
	} else
		printf("back from a write to memory that may only be read; it reads %d\n", page[0]);
	mprotect((void *)page, 16384, PROT_NONE);
	if (!sigsetjmp(back, 1))
		printf("read %d\n", page[0]);
	else
		printf("back from a read of memory that may not be touched\n");
	munmap((void *)page, 16384);
	if (!sigsetjmp(back, 1))
		printf("read %d\n", page[0]);
	else
		printf("back from a read of memory that was unmapped\n");
	action.sa_sigaction = trap;
	sigaction(SIGTRAP, &action, NULL);
	if (!sigsetjmp(back, 1))
		__builtin_trap();
	else
		printf("back from a trap\n");

	// A handler that mends what faulted returns to it, and it is made again.
	page = mmap(NULL, 16384, PROT_READ, MAP_ANON | MAP_PRIVATE, -1, 0);
	action.sa_sigaction = mend;
	sigaction(SIGBUS, &action, NULL);
	sigaction(SIGSEGV, &action, NULL);
	page[5] = 42;
	printf("the write that faulted was made: %d\n", page[5]);
	signal(SIGBUS, SIG_DFL);
	signal(SIGSEGV, SIG_DFL);

	// Timers.
	signal(SIGALRM, rang);
	struct itimerval once = {.it_value = {0, 30000}}, old = {{9, 9}, {9, 9}}, none = {{0, 0}, {0, 0}};
	int rc = setitimer(ITIMER_REAL, &once, &old);
	printf("a timer for once, in 30 ms: %d; none was running: %d\n", rc, old.it_value.tv_sec == 0 && old.it_value.tv_usec == 0);
	struct itimerval left;
	getitimer(ITIMER_REAL, &left);
	printf("it has some of that left: %d, and no interval: %d\n", left.it_value.tv_sec == 0 && left.it_value.tv_usec > 0 && left.it_value.tv_usec <= 30000,
	       left.it_interval.tv_sec == 0 && left.it_interval.tv_usec == 0);
	rc = pause();
	printf("pause: %d (%s); it rang %d time\n", rc, strerror(errno), alarms);
	getitimer(ITIMER_REAL, &left);
	printf("afterwards it is not running: %d\n", left.it_value.tv_sec == 0 && left.it_value.tv_usec == 0);
	struct itimerval often = {.it_interval = {0, 10000}, .it_value = {0, 10000}};
	alarms = 0;
	setitimer(ITIMER_REAL, &often, NULL);
	while (alarms < 3)
		pause();
	setitimer(ITIMER_REAL, &none, &old);
	printf("every 10 ms: it rang 3 times, and was stopped with an interval of %d\n", (int)old.it_interval.tv_usec);
	alarms = 0;
	usleep(30000);
	printf("stopped, it rings no more: %d\n", alarms == 0);
	unsigned first = alarm(1), second = alarm(2), last = alarm(0);
	printf("alarm for 1 second: %u; alarm again, with that one left: %u; and stopped: %u\n", first, second, last);
	struct itimerval bad = {.it_value = {0, 2000000}};
	printf("a timer of 2000000 microseconds: %d (%s)\n", setitimer(ITIMER_REAL, &bad, NULL), strerror(errno));
	printf("a timer there is not: %d (%s)\n", setitimer(7, &once, NULL), strerror(errno));
	return 0;
}
