// Signals to a group of processes: kill with no pid, or with a group's number below zero. A
// process is in its parent's group unless it is started into one of its own. It prints the same
// natively on a Mac as under ipswrun.
#include <errno.h>
#include <mach-o/dyld.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

static const char *said(long r) {
	static char text[8][64];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 64, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 64, "%ld", r);
	return t;
}

static const char *ended(pid_t pid) {
	static char texts[4][64];
	static int next;
	char *text = texts[next++ % 4];
	int status = 0;
	if (waitpid(pid, &status, 0) != pid)
		return strerror(errno);
	if (WIFSIGNALED(status))
		snprintf(text, 64, "ended by signal %d", WTERMSIG(status));
	else
		snprintf(text, 64, "exited with %d", WEXITSTATUS(status));
	return text;
}

static volatile sig_atomic_t told;

static void handler(int sig) { told++; }

// started starts this program again in a role, in a group of its own or its parent's.
static pid_t started(const char *self, const char *role, int own) {
	posix_spawnattr_t attr;
	posix_spawnattr_init(&attr);
	if (own) {
		posix_spawnattr_setflags(&attr, POSIX_SPAWN_SETPGROUP);
		posix_spawnattr_setpgroup(&attr, 0);
	}
	char *argv[] = {(char *)self, (char *)role, NULL};
	pid_t pid = 0;
	if (posix_spawn(&pid, self, NULL, &attr, argv, environ))
		pid = -1;
	posix_spawnattr_destroy(&attr);
	return pid;
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char self[1024];
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	if (argc == 1) {
		// What follows signals its own group, so it is done by this program started again as
		// the first of a new one: whatever started this one is in its group, and is left alone.
		int status = 0;
		waitpid(started(self, "first", 1), &status, 0);
		return WEXITSTATUS(status);
	}
	if (!strcmp(argv[1], "wait")) { // a child: it waits for a signal, which ends it
		for (;;)
			pause();
	}
	pid_t group = getpgrp();
	printf("started as the first of a group: %d\n", group == getpid());

	// Asked only whether there is anyone to signal (0).
	printf("its own group, by number: %s; as no pid: %s; a group there is not: %s; a signal there is not: %s\n", said(kill(-group, 0)), said(kill(0, 0)), said(kill(-99999, 0)),
	       said(kill(-group, 99)));
	pid_t inside = started(self, "wait", 0), apart = started(self, "wait", 1);
	printf("a child in its parent's group: its group is the parent's %d; as a group of its own: %s\n", getpgid(inside) == group, said(kill(-inside, 0)));
	printf("a child in a group of its own: its group is its own %d; as a group: %s\n", getpgid(apart) == apart, said(kill(-apart, 0)));

	// A signal to a group is one to each process in it.
	printf("SIGTERM to the child's group: %s; the child %s", said(kill(-apart, SIGTERM)), ended(apart));
	printf("; its group then: %s\n", said(kill(-apart, 0)));
	apart = started(self, "wait", 1);
	kill(apart, SIGKILL);
	usleep(100000);
	printf("a child that has ended and is not yet waited for, as a group: %s", said(kill(-apart, 0)));
	printf("; it %s\n", ended(apart));

	// One that has ended is there to be sent a signal until its parent has waited for it, or
	// has ended too.
	int out[2];
	pipe(out);
	pid_t child = fork(), its = 0;
	if (child == 0) {
		if ((its = fork()) == 0)
			_exit(0);
		write(out[1], &its, sizeof its);
		usleep(100000); // its child has ended by now, and is not waited for
		_exit(0);
	}
	read(out[0], &its, sizeof its);
	usleep(50000);
	printf("a child's child that has ended, while the child has not: %s", said(kill(its, 0)));
	printf("; the child %s", ended(child));
	usleep(100000);
	printf("; its child then: %s\n", said(kill(its, 0)));

	// To its own group, the process is told too. It and a copy of it have a handler, and the
	// signal blocked until they are ready for it; the child started in the group has neither.
	struct sigaction act = {.sa_handler = handler};
	sigaction(SIGUSR1, &act, NULL);
	sigset_t usr1, before;
	sigemptyset(&usr1);
	sigaddset(&usr1, SIGUSR1);
	sigprocmask(SIG_BLOCK, &usr1, &before);
	pid_t copy = fork();
	if (copy == 0) {
		sigsuspend(&before); // until the handler has run
		_exit(told == 1 ? 7 : 8);
	}
	printf("SIGUSR1 to its own group: %s", said(kill(0, SIGUSR1)));
	printf("; the process is told when it lets the signal in: %d before", told);
	sigprocmask(SIG_SETMASK, &before, NULL);
	printf(", %d after; the copy of it %s; the child started in the group %s\n", told, ended(copy), ended(inside));
	return 0;
}
