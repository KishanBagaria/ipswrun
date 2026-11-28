// The least room a socket wants before it will be written to (SO_SNDLOWAT), which rsync sets on
// the pair of sockets it talks to its other half through: what it is at first, what the kernel
// makes of what is asked, and what it then says of whether the socket can be written and takes
// of what is sent without waiting. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

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

static int least(int fd) {
	int v = -7;
	socklen_t n = sizeof v;
	return getsockopt(fd, SOL_SOCKET, SO_SNDLOWAT, &v, &n) ? -1 : v;
}

static const char *set(int fd, int v) { return said(setsockopt(fd, SOL_SOCKET, SO_SNDLOWAT, &v, sizeof v)); }

static int writable(int fd) {
	struct pollfd p = {fd, POLLOUT, 0};
	return poll(&p, 1, 0) == 1 && (p.revents & POLLOUT) != 0;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	static char bytes[16384];
	int s[2], ends[2];
	socketpair(AF_UNIX, SOCK_STREAM, 0, s);
	pipe(ends);
	printf("at first: %d\n", least(s[0]));
	printf("set to 32768: %s", set(s[0], 32768));
	printf(", and it is then %d; to 1: %s", least(s[0]), set(s[0], 1));
	printf(", then %d; to 5000: %s", least(s[0]), set(s[0], 5000));
	printf(", then %d; the other's meanwhile %d\n", least(s[0]), least(s[1]));
	printf("to none: %s", set(s[0], 0));
	printf("; to less than none: %s", set(s[0], -5));
	short half = 100;
	printf("; with half the room for the number: %s", said(setsockopt(s[0], SOL_SOCKET, SO_SNDLOWAT, &half, sizeof half)));
	printf(", then %d; of a pipe: %s\n", least(s[0]), set(ends[1], 100));

	// With something sent and not yet received.
	fcntl(s[0], F_SETFL, O_NONBLOCK);
	printf("100 bytes sent: %s", said(send(s[0], bytes, 100, 0)));
	printf("; the sender's set to 32768: %s", set(s[0], 32768));
	printf(", then %d; the receiver's set to 32768: %s", least(s[0]), set(s[1], 32768));
	printf(", then %d\n", least(s[1]));

	// Whether it can be written, and what a send that does not wait takes.
	printf("wanting all its room, with 100 bytes waiting: it can be written %d; 10 bytes more sent: %s", writable(s[0]), said(send(s[0], bytes, 10, 0)));
	printf("; 9000 more: %s\n", said(send(s[0], bytes, 9000, 0)));
	char got[16384];
	printf("all received: %s", said(recv(s[1], got, sizeof got, 0)));
	printf("; it can be written %d\n", writable(s[0]));
	set(s[0], 4000);
	printf("wanting 4000: 4000 bytes sent: %s", said(send(s[0], bytes, 4000, 0)));
	printf(", it can be written %d; 500 more: %s", writable(s[0]), said(send(s[0], bytes, 500, 0)));
	printf(", it can be written %d; 4000 more: %s\n", writable(s[0]), said(send(s[0], bytes, 4000, 0)));
	set(s[0], 1);
	printf("wanting 1: it can be written %d; 4000 more: %s\n", writable(s[0]), said(send(s[0], bytes, 4000, 0)));
	return 0;
}
