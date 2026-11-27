// The socket on which the kernel tells a program of what happens to the machine (a network
// interface coming up, a volume mounted), which configd opens as it starts: that a program is
// given one, what it may say of which events it wants and is told back, and that with nothing
// happening nothing is there to read. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/event.h>
#include <sys/filio.h>
#include <sys/ioctl.h>
#include <sys/socket.h>
#include <unistd.h>

// What <sys/kern_event.h> has, which an iPhone's SDK does not give.
struct request {
	uint32_t vendor, class, subclass;
};
enum { forSystem = 32, eventProtocol = 1 };
#define getID _IOR('e', 1, uint32_t)
#define setFilter _IOW('e', 2, struct request)
#define getFilter _IOR('e', 3, struct request)

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

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	int s = socket(forSystem, SOCK_RAW, eventProtocol);
	printf("a socket for the kernel's events: %s\n", said(s < 0 ? s : 0));
	int kind = -1;
	socklen_t n = sizeof kind;
	printf("its kind: %s", said(getsockopt(s, SOL_SOCKET, SO_TYPE, &kind, &n)));
	printf(", %d\n", kind);

	struct request filter = {0x55, 0x55, 0x55};
	printf("which events it wants, at first: %s", said(ioctl(s, getFilter, &filter)));
	printf(", vendor %u class %u subclass %u\n", filter.vendor, filter.class, filter.subclass);
	struct request apple = {1, 1, 0}; // Apple's, of the network
	printf("set to Apple's network events: %s", said(ioctl(s, setFilter, &apple)));
	memset(&filter, 0x55, sizeof filter);
	printf("; then: %s", said(ioctl(s, getFilter, &filter)));
	printf(", vendor %u class %u subclass %u\n", filter.vendor, filter.class, filter.subclass);
	printf("a request there is not: %s\n", said(ioctl(s, _IOR('e', 77, uint32_t), &kind)));

	// With nothing happening that it asked for (a class there is none of), nothing is there.
	struct request nothing = {1, 0x7fff, 0x7fff};
	ioctl(s, setFilter, &nothing);
	char buf[256];
	struct pollfd p = {s, POLLIN, 0};
	printf("with nothing happening: poll says %d", poll(&p, 1, 0));
	int on = 1, off = 0;
	printf("; told by ioctl not to wait: %s", said(ioctl(s, FIONBIO, &on)));
	printf(", and its flags then say so %d", (fcntl(s, F_GETFL) & O_NONBLOCK) != 0);
	printf("; to wait again: %s", said(ioctl(s, FIONBIO, &off)));
	printf(", %d\n", (fcntl(s, F_GETFL) & O_NONBLOCK) != 0);
	fcntl(s, F_SETFL, O_NONBLOCK);
	printf("a read that does not wait: %s", said(read(s, buf, sizeof buf)));
	printf("; a receive: %s\n", said(recv(s, buf, sizeof buf, 0)));
	int queue = kqueue();
	struct kevent watch, got;
	EV_SET(&watch, s, EVFILT_READ, EV_ADD, 0, 0, NULL);
	struct timespec now = {0, 0};
	printf("a kqueue watching it: %s", said(kevent(queue, &watch, 1, NULL, 0, NULL)));
	printf(", events %d\n", kevent(queue, NULL, 0, &got, 1, &now));
	printf("sent to: %s\n", said(send(s, "x", 1, 0)));
	// How much it holds of what has not been read, which configd sets.
	int room = -1, sizes[] = {65536, 1, 0, -1};
	n = sizeof room;
	printf("room for what is not yet read, at first: %s", said(getsockopt(s, SOL_SOCKET, SO_RCVBUF, &room, &n)));
	printf(", %d;", room);
	for (unsigned i = 0; i < sizeof sizes / sizeof *sizes; i++) {
		printf(" set to %d: %s", sizes[i], said(setsockopt(s, SOL_SOCKET, SO_RCVBUF, &sizes[i], sizeof sizes[i])));
		room = -1, n = sizeof room;
		getsockopt(s, SOL_SOCKET, SO_RCVBUF, &room, &n);
		printf(", then %d;", room);
	}
	printf("\n");
	int other = socket(forSystem, SOCK_RAW, eventProtocol);
	printf("a second one: %s; of another kind (a stream): %s", said(other < 0 ? other : 0), said(socket(forSystem, SOCK_STREAM, eventProtocol)));
	printf("; of a protocol there is not: %s\n", said(socket(forSystem, SOCK_RAW, 77)));
	printf("closed: %s %s\n", said(close(other)), said(close(s)));
	return 0;
}
