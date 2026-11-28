// The numbers of the processes there are, as pgrep and the daemon Activity Monitor asks have
// them from the kernel (proc_listpids): all of them, those of a parent, of a group and of a
// user, how much room the kernel says they need, and what it says with too little. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

// What libproc.h has, which an iPhone's SDK does not ship.
int proc_listpids(uint32_t type, uint32_t typeinfo, void *buffer, int buffersize);
enum { PROC_ALL_PIDS = 1, PROC_PGRP_ONLY = 2, PROC_UID_ONLY = 4, PROC_RUID_ONLY = 5, PROC_PPID_ONLY = 6 };

static const char *said(long r) {
	static char text[8][64];
	static int next;
	char *t = text[next++ % 8];
	if (r <= 0 && errno)
		snprintf(t, 64, "%ld (%s)", r, strerror(errno));
	else
		snprintf(t, 64, "%ld", r);
	return t;
}

static pid_t list[16384];

// listed asks for the processes of a kind and says how many came.
static int listed(uint32_t kind, uint32_t of) {
	memset(list, 0, sizeof list);
	errno = 0;
	int bytes = proc_listpids(kind, of, list, sizeof list);
	return bytes < 0 ? -1 : bytes / (int)sizeof *list;
}

static int at(int count, pid_t pid) {
	for (int i = 0; i < count; i++)
		if (list[i] == pid)
			return i;
	return -1;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	int ready[2], done[2];
	pipe(ready);
	pipe(done);
	pid_t child = fork();
	if (child == 0) {
		char c;
		setpgid(0, 0);
		write(ready[1], "r", 1);
		read(done[0], &c, 1);
		_exit(0);
	}
	char c;
	read(ready[0], &c, 1);
	pid_t self = getpid();

	// The room the kernel says they need is for some more than it then lists: asked until it
	// is the same twice, as one may come or go between the two questions.
	int all = 0, more = -1, before = -2;
	for (int tries = 0; tries < 20 && more != before; tries++) {
		before = more;
		int room = proc_listpids(PROC_ALL_PIDS, 0, NULL, 0);
		all = listed(PROC_ALL_PIDS, 0);
		more = room / (int)sizeof *list - all;
	}
	int me = at(all, self), it = at(all, child);
	printf("all: this one among them %d, its child %d, launchd %d; the child, begun later, comes first %d; room is said for %d more\n", me >= 0, it >= 0, at(all, 1) >= 0, it >= 0 && it < me, more);
	int n = listed(PROC_PPID_ONLY, self);
	printf("of this parent: %d, the child %d\n", n, at(n, child) >= 0);
	n = listed(PROC_PGRP_ONLY, child);
	printf("of the child's group: %d, the child %d, this one %d\n", n, at(n, child) >= 0, at(n, self) >= 0);
	n = listed(PROC_UID_ONLY, getuid());
	printf("of this user: this one %d, the child %d, launchd %d\n", at(n, self) >= 0, at(n, child) >= 0, at(n, 1) >= 0);
	n = listed(PROC_RUID_ONLY, getuid());
	printf("of this user as it really is: this one %d, the child %d\n", at(n, self) >= 0, at(n, child) >= 0);
	printf("of a parent there is not: %d; of a kind there is not: %d\n", listed(PROC_PPID_ONLY, 99999999), listed(99, 0));
	errno = 0;
	int two = proc_listpids(PROC_ALL_PIDS, 0, list, 2);
	printf("with room for half a number: %s", said(two));
	errno = 0;
	int one = proc_listpids(PROC_ALL_PIDS, 0, list, sizeof *list);
	printf("; for one: %s, and it is the newest %d\n", said(one), list[0] != 0);

	write(done[1], "d", 1);
	int status;
	waitpid(child, &status, 0);
	n = listed(PROC_PPID_ONLY, self);
	printf("the child ended and waited for: of this parent %d\n", n);
	return 0;
}
