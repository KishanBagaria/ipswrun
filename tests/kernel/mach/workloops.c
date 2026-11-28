// A workloop made by name with how its thread is to be scheduled (kqueue_workloop_ctl), as a
// program that plays sound makes one for each of its queues: what the kernel takes for a
// priority, a policy and a share of a processor, and that a workloop is made once and destroyed
// once. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/event.h>

extern int __kqueue_workloop_ctl(uintptr_t command, uint64_t options, void *parameters, size_t size);

struct event { // struct kevent_qos_s, which no SDK has
	uint64_t ident;
	int16_t filter;
	uint16_t flags;
	int32_t qos;
	uint64_t udata;
	uint32_t fflags, xflags;
	int64_t data;
	uint64_t ext[4];
};
extern int kevent_id(uint64_t id, const struct event *changes, int nchanges, struct event *events, int nevents, void *out, size_t *available, unsigned int flags);

enum { create = 1, destroy = 2, hasPriority = 1, hasPolicy = 2, hasShare = 4 };

struct parameters { // struct kqueue_workloop_params, which no SDK has
	int version, flags;
	uint64_t id;
	int priority, policy, percent, refill, more[2];
};

static uint64_t next = 0x51000;

// asked is what a command says of a workloop with such parameters, each a new one unless named.
static const char *asked(int command, uint64_t id, int flags, int priority, int policy, int percent, int refill, int size) {
	static char text[8][48];
	static int at;
	char *t = text[at++ % 8];
	struct parameters p = {size, flags, id ? id : next++, priority, policy, percent, refill, {0, 0}};
	if (__kqueue_workloop_ctl(command, 0, &p, size) == 0)
		return "0";
	snprintf(t, 48, "-1 (%s)", strerror(errno));
	return t;
}

// watched asks a workloop for an event of the program's own, or to forget it: by asking, a
// workloop that is not there is made, and it is there for as long as it holds the event.
static int watched(uint64_t id, int adding) {
	struct event e = {.ident = 7, .filter = EVFILT_USER, .flags = adding ? EV_ADD : EV_DELETE, .qos = 0x800}; // a default quality of service
	return kevent_id(id, &e, 1, NULL, 0, NULL, NULL, 0x403); // a workloop's, answering only what is refused, at once
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	// One after another: each is a thing done, and the next is asked of what that left.
	const char *made = asked(create, 0x50000, 3, 61, 2, 0, 0, 36), *again = asked(create, 0x50000, 3, 61, 2, 0, 0, 36);
	const char *gone = asked(destroy, 0x50000, 0, 0, 0, 0, 0, 36), *twice = asked(destroy, 0x50000, 0, 0, 0, 0, 0, 36);
	printf("made, with a priority and a policy as a player of sound gives: %s; made again: %s; destroyed: %s; destroyed again: %s; one never made destroyed: %s\n", made, again, gone, twice,
	       asked(destroy, 0x50001, 0, 0, 0, 0, 0, 36));

	printf("a priority alone:");
	int last = -2;
	for (int priority = -1; priority <= 130; priority++) {
		int refused = strcmp(asked(create, 0, hasPriority, priority, 0, 0, 0, 36), "0") != 0;
		if (refused != last)
			printf(" from %d %s;", priority, refused ? "refused" : "taken");
		last = refused;
	}
	printf("\na policy alone:");
	for (int policy = -1; policy <= 6; policy++)
		printf(" %d: %s;", policy, asked(create, 0, hasPolicy, 0, policy, 0, 0, 36));
	printf("\na share of a processor, each second:");
	for (int i = 0, percent[] = {-1, 0, 1, 50, 100, 101}; i < 6; i++)
		printf(" %d%%: %s;", percent[i], asked(create, 0, hasShare, 0, 0, percent[i], 1000, 36));
	printf(" half of each %d and %d and %d thousandths of one: %s, %s, %s\n", -1, 0, 1, asked(create, 0, hasShare, 0, 0, 50, -1, 36), asked(create, 0, hasShare, 0, 0, 50, 0, 36), asked(create, 0, hasShare, 0, 0, 50, 1, 36));
	printf("saying which of them it has:");
	for (int flags = 0; flags <= 9; flags++)
		printf(" %d: %s;", flags, asked(create, 0, flags, 31, 2, 50, 1000, 36));
	printf("\nparameters of so many bytes, saying so:");
	for (int size = 16; size <= 44; size += 4)
		printf(" %d: %s;", size, asked(create, 0, 3, 31, 2, 0, 0, size));
	struct parameters wrong = {32, 3, next++, 31, 2, 0, 0, {0, 0}};
	int mismatch = __kqueue_workloop_ctl(create, 0, &wrong, 36);
	printf(" of 36 saying 32: %d (%s)\n", mismatch, strerror(errno));
	printf("of 20 bytes, with a priority and no room for a policy: %s; a share over 16777215 thousandths of a second: %s, over one more: %s\n", asked(create, 0, hasPriority, 31, 0, 0, 0, 20),
	       asked(create, 0, hasShare, 0, 0, 50, 0xffffff, 36), asked(create, 0, hasShare, 0, 0, 50, 0x1000000, 36));

	// One that an event made is there, and is not one to destroy; one made by name lasts while it holds an event.
	const uint64_t byEvent = 0x60000, byName = 0x60001;
	int added = watched(byEvent, 1);
	const char *taken = asked(create, byEvent, 3, 31, 2, 0, 0, 36), *kept = asked(destroy, byEvent, 0, 0, 0, 0, 0, 36);
	int deleted = watched(byEvent, 0);
	printf("an event asked of a workloop that is not there: %d; that workloop made by name: %s; destroyed: %s; its event forgotten: %d, and it is then made by name: %s\n", added, taken, kept, deleted,
	       asked(create, byEvent, 3, 31, 2, 0, 0, 36));
	asked(create, byName, 3, 31, 2, 0, 0, 36);
	added = watched(byName, 1);
	const char *first = asked(destroy, byName, 0, 0, 0, 0, 0, 36), *second = asked(destroy, byName, 0, 0, 0, 0, 0, 36), *remade = asked(create, byName, 3, 31, 2, 0, 0, 36);
	deleted = watched(byName, 0);
	const char *after = asked(destroy, byName, 0, 0, 0, 0, 0, 36);
	printf("one made by name, holding an event (%d): destroyed %s, again %s, made again %s; its event forgotten (%d): destroyed %s, made %s\n", added, first, second, remade, deleted, after,
	       asked(create, byName, 3, 31, 2, 0, 0, 36));
	struct parameters all = {36, 0, ~0ull, 0, 0, 0, 0, {0, 0}}, zero = {36, 0, 0, 0, 0, 0, 0, {0, 0}};
	int ones = __kqueue_workloop_ctl(destroy, 0, &all, 36);
	printf("destroying one named all ones: %d (%s)", ones, strerror(errno));
	int nought = __kqueue_workloop_ctl(destroy, 0, &zero, 36);
	printf("; named 0: %d (%s)\n", nought, strerror(errno));

	struct parameters nameless = {36, 3, 0, 31, 2, 0, 0, {0, 0}};
	int unnamed = __kqueue_workloop_ctl(create, 0, &nameless, 36);
	printf("named 0: %d (%s)", unnamed, strerror(errno));
	int none = __kqueue_workloop_ctl(create, 0, NULL, 36);
	printf("; with no parameters: %d (%s); a command there is not: %s\n", none, strerror(errno), asked(3, 0, 3, 31, 2, 0, 0, 36));
	return 0;
}
