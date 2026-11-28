// A kqueue of the program's own: events it makes itself, timers, a pipe that can be read or
// written, and a child ending, each asked for and waited for with kevent. It prints the same
// natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <unistd.h>
#include <mach-o/dyld.h>

extern char **environ;
static int kq;
static const struct timespec at_once = {0, 0};

// change makes one change to the kqueue, with no room for an answer, and says what kevent said.
static const char *change(uintptr_t ident, int filter, int flags, unsigned fflags, long data, void *udata) {
	static char text[4][48];
	static int next;
	char *t = text[next++ % 4];
	struct kevent c;
	EV_SET(&c, ident, filter, flags, fflags, data, udata);
	int n = kevent(kq, &c, 1, NULL, 0, NULL);
	if (n < 0)
		snprintf(t, 48, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 48, "%d", n);
	return t;
}

// events prints what the kqueue has, waiting as long as it is told to.
static int events(const char *what, const struct timespec *timeout) {
	struct kevent e[4];
	memset(e, 0, sizeof e);
	int n = kevent(kq, NULL, 0, e, 4, timeout);
	printf("%s: %d", what, n);
	if (n < 0)
		printf(" (%s)", strerror(errno));
	for (int i = 0; i < n; i++)
		printf("; ident %lu filter %d flags %#x fflags %#x data %ld udata %#lx", (unsigned long)e[i].ident, e[i].filter, e[i].flags, e[i].fflags, (long)e[i].data,
		       (unsigned long)e[i].udata);
	printf("\n");
	return n;
}

static double now(void) {
	struct timeval t;
	gettimeofday(&t, NULL);
	return t.tv_sec + t.tv_usec / 1e6;
}

static void *trigger(void *arg) {
	usleep(20000);
	struct kevent c;
	EV_SET(&c, 9, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL);
	kevent(kq, &c, 1, NULL, 0, NULL);
	return NULL;
}

int main(int argc, char **argv) {
	if (argc > 1)
		return atoi(argv[1]);
	setvbuf(stdout, NULL, _IOLBF, 0);
	kq = kqueue();
	printf("kqueue: a descriptor: %d\n", kq >= 0);
	events("an empty kqueue, not waited for", &at_once);
	struct timespec short_wait = {0, 30000000};
	double began = now();
	events("an empty kqueue, waited for 30 ms", &short_wait);
	printf("it took at least that: %d\n", now() - began >= 0.029);

	// An event the program makes happen itself.
	printf("add one of the user's, cleared when it is taken: %s\n", change(1, EVFILT_USER, EV_ADD | EV_CLEAR, 0, 0, (void *)0x11));
	events("before it is made to happen", &at_once);
	printf("make it happen: %s\n", change(1, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL));
	events("after", &at_once);
	events("again", &at_once);
	printf("add one that is not cleared: %s; make it happen: %s\n", change(2, EVFILT_USER, EV_ADD, 0, 0, (void *)0x22), change(2, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL));
	events("taken", &at_once);
	events("taken again", &at_once);
	printf("disable it: %s\n", change(2, EVFILT_USER, EV_DISABLE, 0, 0, NULL));
	events("disabled", &at_once);
	printf("enable it: %s\n", change(2, EVFILT_USER, EV_ENABLE, 0, 0, NULL));
	events("enabled", &at_once);
	printf("delete it: %s; delete it again: %s\n", change(2, EVFILT_USER, EV_DELETE, 0, 0, NULL), change(2, EVFILT_USER, EV_DELETE, 0, 0, NULL));
	events("deleted", &at_once);
	printf("make one happen that is not there: %s\n", change(7, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL));

	// The flags of a user's event are set, masked and added to as they are made to happen.
	printf("add with flags 0x5: %s; or 0x2 in: %s\n", change(3, EVFILT_USER, EV_ADD | EV_CLEAR, NOTE_FFCOPY | 0x5, 0, NULL),
	       change(3, EVFILT_USER, 0, NOTE_TRIGGER | NOTE_FFOR | 0x2, 0, NULL));
	events("its flags", &at_once);
	printf("and with 0x6: %s\n", change(3, EVFILT_USER, 0, NOTE_TRIGGER | NOTE_FFAND | 0x6, 0, NULL));
	events("its flags", &at_once);
	printf("copy 0x30: %s\n", change(3, EVFILT_USER, 0, NOTE_TRIGGER | NOTE_FFCOPY | 0x30, 0, NULL));
	events("its flags", &at_once);
	change(3, EVFILT_USER, EV_DELETE, 0, 0, NULL);

	// One that is taken once, and one that is disabled when it is taken.
	printf("add one for once: %s, make it happen: %s\n", change(4, EVFILT_USER, EV_ADD | EV_ONESHOT, 0, 0, NULL), change(4, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL));
	events("once", &at_once);
	printf("make it happen again: %s\n", change(4, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL));
	printf("add one that is disabled when taken: %s, make it happen: %s\n", change(5, EVFILT_USER, EV_ADD | EV_DISPATCH | EV_CLEAR, 0, 0, NULL),
	       change(5, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL));
	events("taken", &at_once);
	printf("make it happen again: %s\n", change(5, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL));
	events("while disabled", &at_once);
	printf("enable it: %s\n", change(5, EVFILT_USER, EV_ENABLE, 0, 0, NULL));
	events("enabled", &at_once);
	change(5, EVFILT_USER, EV_DELETE, 0, 0, NULL);

	// What a change is answered with, where there is room for an answer.
	struct kevent c[2], e[2];
	memset(e, 0, sizeof e);
	EV_SET(&c[0], 6, EVFILT_USER, EV_ADD | EV_RECEIPT, 0, 0, (void *)0x66);
	EV_SET(&c[1], 8, EVFILT_USER, EV_DELETE, 0, 0, NULL);
	int n = kevent(kq, c, 2, e, 2, &at_once);
	printf("a receipt asked for, and a change that fails, with room: %d", n);
	for (int i = 0; i < n; i++)
		printf("; ident %lu flags %#x data %ld udata %#lx", (unsigned long)e[i].ident, e[i].flags, (long)e[i].data, (unsigned long)e[i].udata);
	printf("\n");
	change(6, EVFILT_USER, EV_DELETE, 0, 0, NULL);
	change(1, EVFILT_USER, EV_DELETE, 0, 0, NULL);

	// A thread waiting is woken by another making an event happen.
	change(9, EVFILT_USER, EV_ADD | EV_CLEAR, 0, 0, NULL);
	pthread_t thread;
	pthread_create(&thread, NULL, trigger, NULL);
	events("waited for, with another thread to make it happen", NULL);
	pthread_join(thread, NULL);
	change(9, EVFILT_USER, EV_DELETE, 0, 0, NULL);

	// Timers.
	printf("a timer for once, in 20 ms: %s\n", change(1, EVFILT_TIMER, EV_ADD | EV_ONESHOT, 0, 20, (void *)0x71));
	events("before it is due", &at_once);
	events("waited for", NULL);
	struct timespec longer = {0, 60000000};
	events("and after, for 60 ms", &longer);
	printf("a timer every 10 ms: %s\n", change(2, EVFILT_TIMER, EV_ADD, 0, 10, NULL));
	struct kevent t;
	memset(&t, 0, sizeof t);
	usleep(35000);
	n = kevent(kq, NULL, 0, &t, 1, NULL);
	printf("after 35 ms: %d; ident %lu filter %d flags %#x fflags %#x, due more than once: %d\n", n, (unsigned long)t.ident, t.filter, t.flags, t.fflags, t.data >= 2);
	n = kevent(kq, NULL, 0, &t, 1, NULL);
	printf("waited for again: %d, due at least once: %d\n", n, t.data >= 1);
	printf("delete it: %s\n", change(2, EVFILT_TIMER, EV_DELETE, 0, 0, NULL));
	printf("a timer in microseconds, 15000 of them, for once: %s\n", change(3, EVFILT_TIMER, EV_ADD | EV_ONESHOT, NOTE_USECONDS, 15000, NULL));
	began = now();
	events("waited for", NULL);
	printf("it took at least 15 ms: %d\n", now() - began >= 0.014);

	// A pipe: when it can be read, and when written.
	int p[2];
	pipe(p);
	printf("watch the reading end: %s\n", change(p[0], EVFILT_READ, EV_ADD, 0, 0, (void *)0x81));
	events("an empty pipe", &at_once);
	write(p[1], "hello", 5);
	events("with 5 in it", &at_once);
	events("and still", &at_once);
	char buf[16];
	read(p[0], buf, 2);
	events("with 3 left", &at_once);
	read(p[0], buf, 3);
	events("emptied", &at_once);
	printf("only what is new: %s\n", change(p[0], EVFILT_READ, EV_ADD | EV_CLEAR, 0, 0, (void *)0x82));
	write(p[1], "abc", 3);
	events("3 written", &at_once);
	events("nothing since", &at_once);
	write(p[1], "de", 2);
	events("2 more", &at_once);
	printf("watch the writing end: %s\n", change(p[1], EVFILT_WRITE, EV_ADD, 0, 0, (void *)0x83));
	events("both ends", &at_once);
	close(p[1]);
	events("the writer gone", &at_once);
	read(p[0], buf, sizeof buf);
	events("and what it left read", &at_once);
	close(p[0]);
	events("the pipe closed", &at_once);

	// A child ending.
	char self[1024];
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	char *child[] = {self, "3", NULL};
	pid_t pid = 0;
	posix_spawn(&pid, self, NULL, NULL, child, environ);
	struct kevent proc, ended;
	memset(&ended, 0, sizeof ended);
	EV_SET(&proc, pid, EVFILT_PROC, EV_ADD, NOTE_EXIT | NOTE_EXITSTATUS, 0, (void *)0x91);
	n = kevent(kq, &proc, 1, &ended, 1, NULL);
	int status = 0;
	waitpid(pid, &status, 0);
	if (n == 1 && ended.flags & EV_ERROR)
		printf("a child ending: it had ended before it could be watched (%s), exited %d\n", strerror((int)ended.data), WEXITSTATUS(status));
	else
		printf("a child ending: %d; it is the child: %d, filter %d flags %#x fflags %#x data %#lx udata %#lx\n", n, ended.ident == (uintptr_t)pid, ended.filter, ended.flags,
		       ended.fflags, (long)ended.data, (unsigned long)ended.udata);

	// kevent64 carries two words more. A kqueue is of the one kind or the other, whichever it
	// was first asked with.
	struct kevent64_s c64, e64;
	memset(&e64, 0, sizeof e64);
	EV_SET64(&c64, 1, EVFILT_USER, EV_ADD | EV_ONESHOT, NOTE_TRIGGER | 0x7, 0, 0x64, 0xAAAA, 0xBBBB);
	n = kevent64(kq, &c64, 1, &e64, 1, 0, &at_once);
	printf("kevent64 of a kqueue that kevent was asked of: %d (%s)\n", n, strerror(errno));
	int kq64 = kqueue();
	n = kevent64(kq64, &c64, 1, &e64, 1, 0, &at_once);
	printf("kevent64 of one of its own: %d; ident %llu filter %d flags %#x fflags %#x data %lld udata %#llx, and its two words %#llx %#llx\n", n, e64.ident, e64.filter, e64.flags,
	       e64.fflags, e64.data, e64.udata, e64.ext[0], e64.ext[1]);
	n = kevent(kq64, NULL, 0, e, 1, &at_once);
	printf("kevent of that one: %d (%s)\n", n, strerror(errno));
	close(kq64);
	printf("closing the kqueue: %d; asking it afterwards: %d (%s)\n", close(kq), kevent(kq, NULL, 0, e, 1, &at_once), strerror(errno));
	return 0;
}
