// The descriptors a process has open, as the kernel lists them for it (proc_pidinfo's
// PROC_PIDLISTFDS, by which ssh finds what to close before it starts): each with its number and
// the kind of thing it is of, and, asked with no room, how much room the whole table of them
// would take. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/event.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <unistd.h>

extern int proc_pidinfo(int pid, int flavor, uint64_t arg, void *buffer, int size);

struct listed {
	int fd;
	unsigned kind;
};

// places is how many descriptors the kernel says there would be room to list.
static int places(void) { return proc_pidinfo(getpid(), 1, 0, NULL, 0) / (int)sizeof(struct listed); }

// list prints those above the three every process starts with, into so much room.
static void list(const char *what, int room) {
	struct listed l[64];
	memset(l, 0x55, sizeof l);
	errno = 0;
	int got = proc_pidinfo(getpid(), 1, 0, l, room);
	printf("%s: %d bytes", what, got);
	if (got <= 0)
		printf(" (%s)", strerror(errno));
	for (int i = 0; i * (int)sizeof *l < got; i++)
		if (l[i].fd > 2)
			printf(", %d of kind %u", l[i].fd, l[i].kind);
	printf("; the three it started with first %d; nothing written after %d\n", got >= 24 && l[0].fd == 0 && l[1].fd == 1 && l[2].fd == 2, got < 0 || ((unsigned *)l)[got / 4] == 0x55555555);
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	int file = open("/usr/lib/dyld", O_RDONLY), dir = open("/", O_RDONLY), queue = kqueue(), ends[2], pair[2];
	pipe(ends);
	socketpair(AF_UNIX, SOCK_STREAM, 0, pair);
	int shared = shm_open("/ipswrun.openfds", O_CREAT | O_RDWR, 0600);
	shm_unlink("/ipswrun.openfds");
	int high = dup2(file, 40);
	printf("a file is %d, a directory %d, a kqueue %d, a pipe %d and %d, a pair of sockets %d and %d, shared memory %d, and the file again %d\n", file, dir, queue, ends[0], ends[1], pair[0], pair[1], shared, high);
	list("all of them", sizeof(struct listed) * 64);
	list("into room for five", 5 * sizeof(struct listed));
	list("into room for two and a half", 20);
	list("into room for less than one", 7);
	close(dir);
	list("with the directory closed", sizeof(struct listed) * 64);

	// How much room the table would take: more than the descriptors there are, by a table that
	// grows as higher numbers are opened and does not shrink. (How large it is as a program
	// starts is the machine's own doing: what was opened before it ran.)
	int before = places();
	dup2(file, 200);
	int grown = places();
	close(200);
	close(40);
	printf("room would be had for more than the highest that is open %d; with descriptor 200 open, for more than before %d, a table of 256 and 20 more %d; with it closed again, as many still %d\n", before > 40,
	       grown > before, grown == 276, places() == grown);
	errno = 0;
	int of = proc_pidinfo(1, 1, 0, NULL, 0);
	printf("of launchd's: %d (%s)\n", of, strerror(errno));
	return 0;
}
