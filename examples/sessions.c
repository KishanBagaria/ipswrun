// A process leaving for a session or a group of its own (setsid, setpgid), as one that makes
// itself a daemon does, and what it keeps of where it came from: a parent that has ended, and a
// signal that waits. It prints the same natively on a Mac as under ipswrun.
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

// waiting is a copy of this process that waits to be ended.
static pid_t waiting(void) {
	pid_t pid = fork();
	if (pid == 0)
		for (;;)
			pause();
	return pid;
}

static void done(pid_t pid) {
	kill(pid, SIGKILL);
	waitpid(pid, NULL, 0);
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char self[1024];
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	if (argc == 1) {
		// What follows is done by this program started again as the first of a group, so that
		// it is the same whatever started this one.
		int status = 0;
		waitpid(started(self, "first", 1), &status, 0);
		return WEXITSTATUS(status);
	}
	if (!strcmp(argv[1], "wait")) {
		for (;;)
			pause();
	}
	if (!strcmp(argv[1], "pending")) { // what a process became, with a signal waiting
		sigset_t waits;
		sigpending(&waits);
		printf("the program a process becomes with SIGUSR1 blocked and waiting: it still waits %d\n", sigismember(&waits, SIGUSR1));
		return 0;
	}
	pid_t me = getpid(), session = getsid(0);

	// A session of its own: not for the first of a group, which this is.
	printf("setsid, of the first of a group: %s\n", said(setsid()));
	pid_t pid = fork();
	if (pid == 0) {
		pid_t made = setsid();
		printf("of a copy of it: its own pid %d; its session is then its own %d, and its group %d; again: %s\n", made == getpid(), getsid(0) == getpid(), getpgrp() == getpid(), said(setsid()));
		printf("its parent's group, for one that leads a session: %s\n", said(setpgid(0, me)));
		_exit(0);
	}
	waitpid(pid, NULL, 0);

	// A group of its own, or another of its session.
	pid = fork();
	if (pid == 0) {
		printf("setpgid, a copy into a group of its own: %s; its group is its own %d, its session still its parent's %d", said(setpgid(0, 0)), getpgrp() == getpid(), getsid(0) == session);
		printf("; back into its parent's: %s, its group its parent's %d\n", said(setpgid(0, me)), getpgrp() == me);
		_exit(0);
	}
	waitpid(pid, NULL, 0);
	pid_t copy = waiting(), other = waiting();
	printf("by its parent, a copy into a group of its own: %s, its group its own %d", said(setpgid(copy, copy)), getpgid(copy) == copy);
	printf("; another into that group: %s, its group that %d", said(setpgid(other, copy)), getpgid(other) == copy);
	printf("; into a group there is not: %s; into one below zero: %s\n", said(setpgid(other, 99999)), said(setpgid(other, -5)));
	done(copy);
	done(other);
	pid_t program = started(self, "wait", 0);
	usleep(100000);
	printf("of a program it started: %s; of launchd: %s; of no process: %s\n", said(setpgid(program, program)), said(setpgid(1, 1)), said(setpgid(99999, 99999)));
	done(program);

	// One that has left a group it was the first of, with others still in it, has that group's
	// number for its pid, and gets no session of its own.
	pid = fork();
	if (pid == 0) {
		setpgid(0, 0);
		pid_t stays = waiting(), apart = waiting();
		setpgid(apart, apart);
		printf("the first of a group, into another: %s", said(setpgid(0, apart)));
		printf("; setsid, with one still in the group it left: %s", said(setsid()));
		kill(stays, SIGKILL);
		usleep(100000);
		printf("; when that one has ended and is not waited for: %s", said(setsid()));
		waitpid(stays, NULL, 0);
		printf("; with none: its own pid %d\n", setsid() == getpid());
		done(apart);
		_exit(0);
	}
	waitpid(pid, NULL, 0);

	// A process whose parent has ended is launchd's.
	int out[2];
	pipe(out);
	pid = fork();
	if (pid == 0) {
		if (fork() == 0) {
			pid_t before = getppid();
			usleep(300000); // its parent has ended by now
			pid_t after = getppid();
			write(out[1], &before, sizeof before);
			write(out[1], &after, sizeof after);
			_exit(0);
		}
		usleep(100000); // its child has asked who its parent is by now
		_exit(0);
	}
	pid_t before = 0, after = 0;
	read(out[0], &before, sizeof before);
	read(out[0], &after, sizeof after);
	waitpid(pid, NULL, 0);
	printf("a child's child: its parent was the child %d; once the child has ended, its parent is %d\n", before == pid, after);

	// A signal that is blocked and waits goes with a process to the program it becomes.
	pid = fork();
	if (pid == 0) {
		sigset_t usr1;
		sigemptyset(&usr1);
		sigaddset(&usr1, SIGUSR1);
		sigprocmask(SIG_BLOCK, &usr1, NULL);
		kill(getpid(), SIGUSR1);
		char *becomes[] = {self, "pending", NULL};
		execve(self, becomes, environ);
		_exit(1);
	}
	waitpid(pid, NULL, 0);
	return 0;
}
