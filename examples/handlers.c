// Signal handlers: when one is run, what it is told, what is blocked while it runs, where it
// runs, and what the call it interrupted then answers. It prints the same natively on a Mac as
// under ipswrun.
#include <errno.h>
#include <mach-o/dyld.h>
#include <pthread.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;
static char self[1024];
static volatile int depth, ran;

static int blocked(int sig) {
	sigset_t now;
	sigprocmask(SIG_BLOCK, NULL, &now);
	return sigismember(&now, sig);
}

static int pending(int sig) {
	sigset_t set;
	sigpending(&set);
	return sigismember(&set, sig);
}

static void plain(int sig) {
	ran++;
	printf("  handler: signal %d; itself blocked: %d, SIGUSR2 blocked: %d\n", sig, blocked(sig), blocked(SIGUSR2));
}

static void told(int sig, siginfo_t *info, void *context) {
	ucontext_t *uc = context;
	ran++;
	printf("  handler: signal %d, told of %d, code %#x, from this process: %d, from this user: %d, status %d; the mask it interrupted has SIGUSR2: %d; on another stack: %d\n", sig,
	       info->si_signo, info->si_code, info->si_pid == getpid(), info->si_uid == getuid(), info->si_status, sigismember(&uc->uc_sigmask, SIGUSR2), uc->uc_onstack);
}

static void nesting(int sig) {
	int level = ++depth;
	printf("  handler in: signal %d, level %d\n", sig, level);
	static int first = 1;
	if (sig == SIGUSR1 && first) {
		first = 0;
		raise(SIGUSR2); // not blocked: runs inside this one
		raise(SIGUSR1); // blocked while this runs: waits for it to return
		printf("  handler: SIGUSR1 is waiting: %d\n", pending(SIGUSR1));
	}
	printf("  handler out: signal %d, level %d\n", sig, level);
	depth--;
}

static char *stack_low, *stack_high;
static void where(int sig, siginfo_t *info, void *context) {
	ucontext_t *uc = context;
	char here;
	stack_t now;
	sigaltstack(NULL, &now);
	printf("  handler: on the stack it was given: %d; said to be on it: %d; the stack in its context is that one: %d, with flags %#x\n", &here >= stack_low && &here < stack_high,
	       (now.ss_flags & SS_ONSTACK) != 0, uc->uc_stack.ss_sp == stack_low, uc->uc_stack.ss_flags);
}

static void child_ended(int sig, siginfo_t *info, void *context) {
	printf("  handler: signal %d, code %d, status %d, of a process that is not this one: %d\n", sig, info->si_code, info->si_status, info->si_pid != getpid() && info->si_pid > 0);
	ran++;
}

static void quiet(int sig) { ran++; }

static void *later(void *arg) {
	usleep(30000);
	pthread_kill(*(pthread_t *)arg, SIGUSR1);
	return NULL;
}

static void *write_later(void *arg) {
	usleep(60000);
	write(*(int *)arg, "late", 4);
	return NULL;
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc > 1 && !strcmp(argv[1], "exit"))
		return 4;
	if (argc > 1 && !strcmp(argv[1], "tell")) { // a signal to its parent
		usleep(30000);
		return kill(getppid(), SIGUSR1) ? 1 : 0;
	}
	if (argc > 1 && !strcmp(argv[1], "once")) { // a handler for once, and the signal twice
		struct sigaction once = {.sa_handler = quiet, .sa_flags = SA_RESETHAND};
		sigaction(SIGUSR1, &once, NULL);
		raise(SIGUSR1);
		struct sigaction now;
		sigaction(SIGUSR1, NULL, &now);
		printf("child: the handler ran %d time, and is the default again: %d\n", ran, now.sa_handler == SIG_DFL);
		raise(SIGUSR1);
		return 3;
	}
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	pthread_t me = pthread_self();

	// A handler runs before the call that sent the signal returns.
	signal(SIGUSR1, plain);
	printf("raise: %d; the handler has run: %d\n", raise(SIGUSR1), ran);
	printf("after it: SIGUSR1 blocked: %d\n", blocked(SIGUSR1));
	double kept = 1.5;
	struct sigaction action = {.sa_sigaction = told, .sa_flags = SA_SIGINFO};
	sigemptyset(&action.sa_mask);
	sigaddset(&action.sa_mask, SIGUSR2);
	sigaction(SIGUSR1, &action, NULL);
	printf("kill of itself: %d\n", kill(getpid(), SIGUSR1));
	printf("a number kept across it: %g\n", kept * 2);
	signal(SIGUSR1, plain);
	sigaction(SIGUSR1, NULL, &action);
	action.sa_flags = SA_NODEFER;
	sigaddset(&action.sa_mask, SIGUSR2);
	sigaction(SIGUSR1, &action, NULL);
	printf("with its own signal not blocked, and SIGUSR2 blocked: %d\n", raise(SIGUSR1));

	// One inside another, and one that waits for the first to return.
	signal(SIGUSR1, nesting);
	signal(SIGUSR2, nesting);
	raise(SIGUSR1);
	printf("both done, at level %d\n", depth);

	// Blocked, a signal waits until it is not.
	signal(SIGUSR1, plain);
	sigset_t set, old;
	sigemptyset(&set);
	sigaddset(&set, SIGUSR1);
	sigprocmask(SIG_BLOCK, &set, &old);
	ran = 0;
	printf("blocked and raised: %d; the handler has run: %d; it is waiting: %d\n", raise(SIGUSR1), ran, pending(SIGUSR1));
	printf("unblocking:\n");
	sigprocmask(SIG_SETMASK, &old, NULL);
	printf("the handler has run: %d; waiting: %d\n", ran, pending(SIGUSR1));

	// On a stack of its own.
	static char stack[65536];
	stack_low = stack;
	stack_high = stack + sizeof stack;
	stack_t alt = {.ss_sp = stack, .ss_size = sizeof stack};
	sigaltstack(&alt, NULL);
	struct sigaction on_stack = {.sa_sigaction = where, .sa_flags = SA_SIGINFO | SA_ONSTACK};
	sigaction(SIGUSR1, &on_stack, NULL);
	raise(SIGUSR1);
	stack_t after;
	sigaltstack(NULL, &after);
	printf("after it, said to be on that stack: %d\n", (after.ss_flags & SS_ONSTACK) != 0);
	on_stack.sa_flags = SA_SIGINFO;
	sigaction(SIGUSR1, &on_stack, NULL);
	raise(SIGUSR1);

	// A call that waits is interrupted, unless the handler says to start it again.
	int p[2];
	char buf[8];
	pipe(p);
	struct sigaction interrupt = {.sa_handler = quiet};
	sigaction(SIGUSR1, &interrupt, NULL);
	pthread_t thread, writer;
	pthread_create(&thread, NULL, later, &me);
	ssize_t n = read(p[0], buf, sizeof buf);
	printf("a read that a signal interrupts: %zd (%s)\n", n, n < 0 ? strerror(errno) : "");
	pthread_join(thread, NULL);
	interrupt.sa_flags = SA_RESTART;
	sigaction(SIGUSR1, &interrupt, NULL);
	ran = 0;
	pthread_create(&thread, NULL, later, &me);
	pthread_create(&writer, NULL, write_later, &p[1]);
	n = read(p[0], buf, sizeof buf);
	printf("one that is started again: %zd \"%.*s\", after the handler ran: %d\n", n, n > 0 ? (int)n : 0, buf, ran);
	pthread_join(thread, NULL);
	pthread_join(writer, NULL);
	interrupt.sa_flags = 0;
	sigaction(SIGUSR1, &interrupt, NULL);
	pthread_create(&thread, NULL, later, &me);
	int rc = usleep(500000);
	printf("a sleep that a signal interrupts: %d (%s)\n", rc, rc < 0 ? strerror(errno) : "");
	pthread_join(thread, NULL);

	// From another process, to one that waits for a signal.
	action = (struct sigaction){.sa_sigaction = told, .sa_flags = SA_SIGINFO};
	sigaction(SIGUSR1, &action, NULL);
	char *tell[] = {self, "tell", NULL};
	pid_t pid = 0;
	posix_spawn(&pid, self, NULL, NULL, tell, environ);
	sigemptyset(&set);
	rc = sigsuspend(&set);
	printf("sigsuspend: %d (%s)\n", rc, strerror(errno));
	int status = 0;
	waitpid(pid, &status, 0);

	// A child ending.
	struct sigaction chld = {.sa_sigaction = child_ended, .sa_flags = SA_SIGINFO};
	sigaction(SIGCHLD, &chld, NULL);
	char *ending[] = {self, "exit", NULL};
	ran = 0;
	posix_spawn(&pid, self, NULL, NULL, ending, environ);
	while (!ran)
		usleep(10000);
	pid_t got = waitpid(pid, &status, 0);
	printf("it is still there to wait for: %d, exited %d\n", got == pid, WEXITSTATUS(status));
	signal(SIGCHLD, SIG_DFL);

	// A handler for once.
	char *once[] = {self, "once", NULL};
	posix_spawn(&pid, self, NULL, NULL, once, environ);
	waitpid(pid, &status, 0);
	printf("the child was ended by signal %d\n", WIFSIGNALED(status) ? WTERMSIG(status) : 0);
	return 0;
}
