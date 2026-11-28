// A kqueue watching a process for the program it becomes (NOTE_EXEC) as well as for its end, as
// a daemon watches each program that talks to it: what it is told when the process starts
// another program, that it goes on watching after, and what it is told at the end. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <sys/event.h>
#include <sys/wait.h>
#include <unistd.h>

// next waits for the next event and says what it is.
static void next(int queue, const char *when, pid_t watched) {
	struct kevent e;
	memset(&e, 0x55, sizeof e);
	struct timespec limit = {5, 0};
	int n = kevent(queue, NULL, 0, &e, 1, &limit);
	if (n != 1) {
		printf("%s: no event (%d)\n", when, n);
		return;
	}
	printf("%s: of the watched process %d, filter %d, flags %#x, what happened %#x, with it %ld, its mark %ld\n", when, e.ident == (uintptr_t)watched, e.filter, e.flags, e.fflags, (long)e.data,
	       (long)e.udata);
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	int go[2], queue = kqueue();
	pipe(go);
	pid_t pid = fork();
	if (pid == 0) {
		char byte;
		close(go[1]);
		read(go[0], &byte, 1); // until it is watched
		execl("/bin/sleep", "sleep", "30", (char *)NULL);
		_exit(99);
	}
	close(go[0]);
	struct kevent watch;
	EV_SET(&watch, pid, EVFILT_PROC, EV_ADD, NOTE_EXIT | NOTE_EXITSTATUS | NOTE_EXEC, 0, (void *)42);
	printf("watching for the end and for a new program: %d\n", kevent(queue, &watch, 1, NULL, 0, NULL));
	struct timespec none = {0, 0};
	struct kevent e;
	printf("before anything: %d events\n", kevent(queue, NULL, 0, &e, 1, &none));
	write(go[1], "x", 1); // it starts another program, which sleeps
	next(queue, "it became another program", pid);
	printf("after that, with nothing more: %d events\n", kevent(queue, NULL, 0, &e, 1, &none));
	kill(pid, SIGTERM); // which is ended, by a signal it does not handle
	next(queue, "it ended", pid);
	printf("after that: %d events\n", kevent(queue, NULL, 0, &e, 1, &none));
	int status = 0;
	waitpid(pid, &status, 0);
	printf("as wait has it: %#x\n", status);
	EV_SET(&watch, pid, EVFILT_PROC, EV_ADD, NOTE_EXEC, 0, NULL);
	struct kevent refused;
	int n = kevent(queue, &watch, 1, &refused, 1, &none);
	printf("watching it for a new program once it is gone: %d, error %d\n", n, n == 1 && (refused.flags & EV_ERROR) ? (int)refused.data : 0);
	return 0;
}
