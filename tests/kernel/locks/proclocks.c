// A process's own locks on parts of a file (fcntl's F_SETLK, F_SETLKW and F_GETLK), which most
// programs that share a file take, and waiting for a lock that another holds: whose a lock is,
// what another process is told of it, what a copy of the process has of them, how one is split
// and joined, what gives them up, and what a wait is told when it could never end. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <unistd.h>

static char path[512];
static pid_t parent;

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

static int lock(int fd, int how, int kind, long from, long length) {
	struct flock l = {.l_start = from, .l_len = length, .l_type = (short)kind, .l_whence = SEEK_SET};
	return fcntl(fd, how, &l);
}

// against says what stands against a lock: none, or its kind, where it is, and whose.
static const char *against(int fd, int how, int kind, long from, long length) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	struct flock l = {.l_start = from, .l_len = length, .l_type = (short)kind, .l_whence = SEEK_SET, .l_pid = 77};
	if (fcntl(fd, how, &l))
		snprintf(t, 96, "-1 (%s)", strerror(errno));
	else if (l.l_type == F_UNLCK)
		snprintf(t, 96, "nothing");
	else
		snprintf(t, 96, "%s lock of %lld from %lld, %s", l.l_type == F_WRLCK ? "a write" : "a read", (long long)l.l_len, (long long)l.l_start,
		         l.l_pid == parent ? "the first process's" : l.l_pid == getpid() ? "its own" : l.l_pid == -1 ? "no process's" : "another's");
	return t;
}

// other runs a function in a copy of the process and prints what it wrote.
static void other(void (*run)(int out)) {
	int ends[2];
	pipe(ends);
	pid_t child = fork();
	if (child == 0) {
		close(ends[0]);
		run(ends[1]);
		_exit(0);
	}
	close(ends[1]);
	char text[512];
	ssize_t n;
	while ((n = read(ends[0], text, sizeof text)) > 0)
		fwrite(text, 1, n, stdout);
	close(ends[0]);
	waitpid(child, NULL, 0);
}

static void say(int out, const char *format, ...) {
	char text[512];
	va_list args;
	va_start(args, format);
	int n = vsnprintf(text, sizeof text, format, args);
	va_end(args);
	write(out, text, n);
}

static void looks(int out) {
	int fd = open(path, O_RDWR);
	say(out, "a copy made after: against a write of the first ten it finds %s", against(fd, F_GETLK, F_WRLCK, 0, 10));
	say(out, "; against a read %s", against(fd, F_GETLK, F_RDLCK, 0, 10));
	say(out, "; a read lock there: %s", said(lock(fd, F_SETLK, F_RDLCK, 0, 10)));
	say(out, "; a write lock of 20 to 30: %s, which it then finds %s\n", said(lock(fd, F_SETLK, F_WRLCK, 20, 10)), against(fd, F_GETLK, F_WRLCK, 20, 10));
}

static void split(int out) {
	int fd = open(path, O_RDWR);
	say(out, "another, after 3 and 4 are unlocked: a lock of them %s", said(lock(fd, F_SETLK, F_WRLCK, 3, 2)));
	say(out, "; against the first ten it finds %s", against(fd, F_GETLK, F_WRLCK, 0, 10));
	say(out, "; against 5 on %s\n", against(fd, F_GETLK, F_WRLCK, 5, 100));
}

static void joined(int out) {
	int fd = open(path, O_RDWR);
	say(out, "another, of two read locks side by side: against a write of all it finds %s", against(fd, F_GETLK, F_WRLCK, 0, 0));
	say(out, "; a read lock of all: %s; a write lock: %s\n", said(lock(fd, F_SETLK, F_RDLCK, 0, 0)), said(lock(fd, F_SETLK, F_WRLCK, 0, 0)));
}

static void tries(int out) {
	int fd = open(path, O_RDWR);
	say(out, "another then: a write lock of the first ten %s\n", said(lock(fd, F_SETLK, F_WRLCK, 0, 10)));
}

static void waits(int out) {
	int fd = open(path, O_RDWR);
	say(out, "another waits for it: %s\n", said(lock(fd, F_SETLKW, F_WRLCK, 0, 10)));
}

static void holds(int out) {
	int fd = open(path, O_RDWR);
	lock(fd, F_SETLK, F_WRLCK, 100, 10);
	say(out, "h");
	usleep(300000);
	_exit(0); // with the lock held
}

// order has a second process lock lower in the file than the first has, and a third ask what
// stands against a lock of both parts.
static void order(int out) {
	int fd = open(path, O_RDWR);
	lock(fd, F_SETLK, F_WRLCK, 800, 10);
	if (fork() == 0) {
		int mine = open(path, O_RDWR);
		say(out, "the first holding 900 on and a second 800 on, a third finds against both %s\n", against(mine, F_GETLK, F_WRLCK, 790, 200));
		_exit(0);
	}
	wait(NULL);
}

static void rings(int signal) { (void)signal; }

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	parent = getpid();
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(path, sizeof path, "%s/proclocks.%d", tmp, getpid());
	int fd = open(path, O_CREAT | O_RDWR, 0644), again = open(path, O_RDWR), reads = open(path, O_RDONLY);
	ftruncate(fd, 1000);

	printf("a write lock of the first ten: %s", said(lock(fd, F_SETLK, F_WRLCK, 0, 10)));
	printf("; the same by another open of the file: %s", said(lock(again, F_SETLK, F_WRLCK, 0, 10)));
	printf("; against it the process finds %s\n", against(again, F_GETLK, F_WRLCK, 0, 10));
	printf("a write lock by an open for reading: %s; a read lock: %s; one of a length below none: %s\n", said(lock(reads, F_SETLK, F_WRLCK, 200, 1)), said(lock(reads, F_SETLK, F_RDLCK, 200, 1)),
	       said(lock(fd, F_SETLK, F_WRLCK, 300, -5)));
	other(looks);

	lock(fd, F_SETLK, F_UNLCK, 3, 2);
	other(split);
	lock(fd, F_SETLK, F_UNLCK, 0, 0);
	lock(fd, F_SETLK, F_RDLCK, 0, 10);
	lock(fd, F_SETLK, F_RDLCK, 10, 10);
	other(joined);

	// What gives them up: any descriptor of the file closed.
	lock(fd, F_SETLK, F_UNLCK, 0, 0);
	lock(fd, F_SETLK, F_WRLCK, 0, 10);
	other(tries);
	close(open(path, O_RDONLY));
	printf("after the first process opens the file once more and closes that:\n");
	other(tries);

	// An open file's lock and a process's, in one process.
	printf("an open file's write lock of 50 to 60: %s", said(lock(again, 90, F_WRLCK, 50, 10))); // F_OFD_SETLK
	printf("; the process's own there, by another open: %s", said(lock(fd, F_SETLK, F_WRLCK, 50, 10)));
	printf(", which finds against it %s", against(fd, F_GETLK, F_WRLCK, 50, 10));
	printf("; by the same open: %s\n", said(lock(again, F_SETLK, F_WRLCK, 50, 10)));
	lock(again, 90, F_UNLCK, 0, 0);

	// Waiting for one: until it is given up, and when it never could be.
	lock(fd, F_SETLK, F_WRLCK, 0, 10);
	int ends[2];
	pipe(ends);
	pid_t child = fork();
	if (child == 0) {
		int mine = open(path, O_RDWR);
		long r = lock(mine, F_SETLKW, F_WRLCK, 0, 10);
		char text[96];
		write(ends[1], text, snprintf(text, sizeof text, "another waits for the first ten: %s\n", said(r)));
		_exit(0);
	}
	usleep(200000);
	printf("the first process gives them up a while later: %s\n", said(lock(fd, F_SETLK, F_UNLCK, 0, 10)));
	char text[256];
	ssize_t n = read(ends[0], text, sizeof text);
	fwrite(text, 1, n > 0 ? n : 0, stdout);
	waitpid(child, NULL, 0);

	lock(fd, F_SETLK, F_WRLCK, 0, 10);
	pipe(ends);
	child = fork();
	if (child == 0) {
		int mine = open(path, O_RDWR);
		lock(mine, F_SETLK, F_WRLCK, 500, 10);
		write(ends[1], "r", 1);
		long r = lock(mine, F_SETLKW, F_WRLCK, 0, 10);
		char line[96];
		write(ends[1], line, snprintf(line, sizeof line, "the other, which waited for the first's, is then given it: %s\n", said(r)));
		_exit(0);
	}
	read(ends[0], text, 1);
	usleep(200000);
	printf("each holding what the other waits for: the second to wait is told %s\n", said(lock(fd, F_SETLKW, F_WRLCK, 500, 10)));
	lock(fd, F_SETLK, F_UNLCK, 0, 10);
	n = read(ends[0], text, sizeof text);
	fwrite(text, 1, n > 0 ? n : 0, stdout);
	waitpid(child, NULL, 0);

	// A wait a signal ends, and a process that ends with a lock.
	pipe(ends);
	child = fork();
	if (child == 0)
		holds(ends[1]);
	read(ends[0], text, 1);
	struct sigaction act = {.sa_handler = rings};
	sigaction(SIGALRM, &act, NULL);
	struct itimerval soon = {.it_value = {0, 100000}};
	setitimer(ITIMER_REAL, &soon, NULL);
	printf("a wait a signal comes during: %s", said(lock(fd, F_SETLKW, F_WRLCK, 100, 10)));
	waitpid(child, NULL, 0);
	printf("; once the one that held it has ended: %s\n", said(lock(fd, F_SETLK, F_WRLCK, 100, 10)));

	// Waiting for an open file's lock.
	pipe(ends);
	child = fork();
	if (child == 0) {
		int mine = open(path, O_RDWR);
		lock(mine, 90, F_WRLCK, 700, 10);
		write(ends[1], "r", 1);
		usleep(200000);
		lock(mine, 90, F_UNLCK, 700, 10);
		usleep(100000);
		_exit(0);
	}
	read(ends[0], text, 1);
	int third = open(path, O_RDWR);
	printf("an open file's lock that another open file holds: not waiting %s", said(lock(third, 90, F_WRLCK, 700, 10)));
	printf(", waiting %s\n", said(lock(third, 91, F_WRLCK, 700, 10))); // F_OFD_SETLKW
	waitpid(child, NULL, 0);
	lock(fd, F_SETLK, F_WRLCK, 900, 10);
	other(order);
	printf("a lock from the last byte there can be, of 2: %s\n", said(lock(fd, F_SETLK, F_WRLCK, 0x7fffffffffffffffL, 2)));
	printf("tidied: %s\n", said(unlink(path)));
	return 0;
}
