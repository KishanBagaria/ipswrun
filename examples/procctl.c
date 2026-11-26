// What a process tells the kernel of itself through proc_info: a name for the thread that asks
// (pthread_setname_np), and that it keeps track of whether it has work in hand, so that a
// daemon with none may be ended when memory is short (proc_track_dirty and the calls beside
// it). It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

extern int __proc_info(int call, int pid, int flavor, uint64_t arg, void *buffer, int size);

enum { setControl = 5, dirtyControl = 8, threadName = 2, track = 1, set = 2, get = 3, clear = 4 };
enum { tracked = 1, idleExit = 2, defer = 4, launching = 8, deferAlways = 0x10 }; // what is asked; what is said back is 1, 2, 4 for having work, and 8

static const char *said(int r) {
	static char text[8][48];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 48, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 48, "%d", r);
	return t;
}

// dirty asks one thing of this process's keeping track, and says what the kernel answers.
static const char *dirty(int what, uint64_t with) { return said(__proc_info(dirtyControl, getpid(), what, with, NULL, 0)); }

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char name[80], back[80] = "";
	memset(name, 'n', sizeof name);
	printf("a thread named with 6 bytes: %s; with 63: %s; with 64: %s; with none: %s; of another process: %s\n", said(__proc_info(setControl, getpid(), threadName, 0, "worker", 6)),
	       said(__proc_info(setControl, getpid(), threadName, 0, name, 63)), said(__proc_info(setControl, getpid(), threadName, 0, name, 64)), said(__proc_info(setControl, getpid(), threadName, 0, name, 0)),
	       said(__proc_info(setControl, 1, threadName, 0, "worker", 6)));
	int named = pthread_setname_np("as the library names it");
	pthread_getname_np(pthread_self(), back, sizeof back);
	printf("named by the library: %d, and it reads back \"%s\"\n", named, back);

	// Each in turn: what is asked changes what the next is told.
	const char *before = dirty(get, 0), *early = dirty(set, 1), *cleared = dirty(clear, launching);
	printf("before it keeps track: it says %s; saying it has work: %s; giving up that it is starting: %s\n", before, early, cleared);
	const char *nothing = dirty(track, 0), *still = dirty(get, 0);
	printf("asked to keep track of nothing: %s, and it says %s\n", nothing, still);
	printf("to be ended when idle without keeping track: %s; starting, without: %s; put off, without being ended when idle: %s; put off both ways: %s\n", dirty(track, idleExit), dirty(track, launching),
	       dirty(track, tracked | defer), dirty(track, tracked | idleExit | defer | deferAlways));
	const char *tracking = dirty(track, tracked), *clean = dirty(get, 0), *working = dirty(set, 1), *busy = dirty(get, 0), *twice = dirty(set, 1), *done = dirty(set, 0), *idle = dirty(get, 0),
	           *again = dirty(set, 0);
	printf("keeping track: %s, and it says %s; with work: %s, and it says %s; said again: %s; with none: %s, and it says %s; said again: %s\n", tracking, clean, working, busy, twice, done, idle, again);
	const char *more = dirty(track, tracked | idleExit | launching), *all = dirty(get, 0);
	printf("to be ended when idle, and starting: %s, and it says %s\n", more, all);
	const char *none = dirty(clear, 0), *wrong = dirty(clear, tracked), *started = dirty(clear, launching), *after = dirty(get, 0);
	printf("giving up nothing: %s; that it keeps track: %s; that it is starting: %s, and it says %s\n", none, wrong, started, after);
	const char *put = dirty(track, tracked | idleExit | defer), *while_ = dirty(get, 0), *other = dirty(track, tracked | idleExit | deferAlways), *given = dirty(clear, defer), *then = dirty(get, 0);
	printf("to be put off once: %s, and it says %s; and then every time: %s; giving up being put off: %s, and it says %s\n", put, while_, other, given, then);
	printf("of the kernel, pid 0: what it is %s, asked to keep track %s\n", said(__proc_info(dirtyControl, 0, get, 0, NULL, 0)), said(__proc_info(dirtyControl, 0, track, tracked, NULL, 0)));
	printf("of no process: %s; launchd asked to keep track, by a user: %s\n", said(__proc_info(dirtyControl, 99999, get, 0, NULL, 0)), said(__proc_info(dirtyControl, 1, track, tracked, NULL, 0)));
	return 0;
}
