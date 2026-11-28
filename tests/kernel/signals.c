// Signals that end a process, and signals that do nothing: what a parent learns of a child that
// aborts, is sent a signal, or sends itself one it ignores. The child is this same program, given
// a role to play. It prints the same natively on a Mac as under ipswrun.
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

static int child(char **argv) {
	const char *role = argv[1];
	int sig = argv[2] ? atoi(argv[2]) : 0;
	if (!strcmp(role, "abort"))
		abort();
	if (!strcmp(role, "raise")) { // a signal to itself, which it survives or does not
		int rc = raise(sig);
		printf("child: sent itself %d, and goes on: raise %d\n", sig, rc);
		return 5;
	}
	if (!strcmp(role, "ignore")) {
		signal(sig, SIG_IGN);
		int rc = kill(getpid(), sig);
		printf("child: ignores %d, sent itself one, and goes on: kill %d\n", sig, rc);
		return 6;
	}
	if (!strcmp(role, "thread")) { // to one of its threads, which ends them all
		pthread_kill(pthread_self(), sig);
		printf("child: went on\n");
		return 7;
	}
	if (!strcmp(role, "pipe")) { // a write with nobody to read it
		int p[2];
		pipe(p);
		close(p[0]);
		ssize_t n = write(p[1], "x", 1);
		printf("child: wrote %zd\n", n);
		return 8;
	}
	if (!strcmp(role, "sleep")) {
		if (sig)
			signal(sig, SIG_IGN);
		usleep(200000);
		return 9;
	}
	return 99;
}

static pid_t start(const char *role, const char *arg) {
	char *argv[] = {self, (char *)role, (char *)arg, NULL};
	pid_t pid = 0;
	posix_spawn(&pid, self, NULL, NULL, argv, environ);
	return pid;
}

// ended waits for a child and says how it ended.
static const char *ended(pid_t pid) {
	static char text[96];
	int status = -1;
	if (waitpid(pid, &status, 0) != pid)
		snprintf(text, sizeof text, "waitpid: %s", strerror(errno));
	else if (WIFEXITED(status))
		snprintf(text, sizeof text, "exited %d", WEXITSTATUS(status));
	else if (WIFSIGNALED(status))
		snprintf(text, sizeof text, "ended by signal %d%s (status %#x)", WTERMSIG(status), WCOREDUMP(status) ? ", with a core" : "", status);
	else
		snprintf(text, sizeof text, "status %#x", status);
	return text;
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc > 1)
		return child(argv);
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);

	printf("a child that aborts: %s\n", ended(start("abort", NULL)));
	printf("one that sends itself SIGTERM: %s\n", ended(start("raise", "15")));
	printf("SIGUSR1: %s\n", ended(start("raise", "30")));
	printf("SIGKILL: %s\n", ended(start("raise", "9")));
	printf("SIGCHLD, which does nothing unless asked for: %s\n", ended(start("raise", "20")));
	printf("SIGWINCH: %s\n", ended(start("raise", "28")));
	printf("SIGURG: %s\n", ended(start("raise", "16")));
	printf("one that ignores SIGTERM and sends itself one: %s\n", ended(start("ignore", "15")));
	printf("one that would ignore SIGKILL: %s\n", ended(start("ignore", "9")));
	printf("SIGINT to one of its threads: %s\n", ended(start("thread", "2")));
	printf("a write with nobody to read it: %s\n", ended(start("pipe", NULL)));

	// From its parent.
	pid_t pid = start("sleep", NULL);
	printf("a signal that asks nothing, of a child: %d", kill(pid, 0));
	printf("; SIGTERM: %d; it %s\n", kill(pid, SIGTERM), ended(pid));
	printf("a signal to it once it has been waited for: %d (%s)\n", kill(pid, 0), strerror(errno));
	pid = start("sleep", "15");
	usleep(50000); // for it to say so
	printf("SIGTERM to a child that ignores it: %d; it %s\n", kill(pid, SIGTERM), ended(pid));
	pid = start("sleep", "15");
	usleep(50000);
	printf("SIGKILL to it: %d; it %s\n", kill(pid, SIGKILL), ended(pid));
	pid = start("exit", NULL);
	usleep(100000); // ended, and not yet waited for
	printf("a signal to one that has ended and not been waited for: %d; it %s\n", kill(pid, SIGTERM), ended(pid));

	// A child inherits what its parent ignores.
	signal(SIGTERM, SIG_IGN);
	printf("with SIGTERM ignored by its parent, one that sends itself SIGTERM: %s\n", ended(start("raise", "15")));
	signal(SIGTERM, SIG_DFL);

	// To itself, and to what is not there or not its own.
	printf("a signal that asks nothing, of itself: %d\n", kill(getpid(), 0));
	printf("of no process: %d (%s)\n", kill(99999, 0), strerror(errno));
	printf("of launchd: %d (%s)\n", kill(1, 0), strerror(errno));
	printf("a signal there is not: %d (%s)\n", kill(getpid(), 99), strerror(errno));
	signal(SIGUSR2, SIG_IGN);
	printf("one it ignores, to itself: %d, to its own thread: %d\n", kill(getpid(), SIGUSR2), pthread_kill(pthread_self(), SIGUSR2));
	printf("one that does nothing, to itself: %d\n", raise(SIGCHLD));
	printf("and it goes on\n");
	return 0;
}
