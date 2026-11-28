// Datagram sockets of the local domain, as a program that logs or one that is told things in
// whole messages has them: each send is one message that is received whole or cut short, to
// the other of a pair or to a socket bound to a path. What comes when the room is too little,
// when there is nothing, when the other has gone; whom a message is said to be from; how many
// a socket holds; and what each call says of a path that is not there or that nobody has. It
// prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
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

static int option(int fd, int which) {
	int v = -7;
	socklen_t n = sizeof v;
	return getsockopt(fd, SOL_SOCKET, which, &v, &n) ? -1 : v;
}

static int readable(int fd) {
	struct pollfd p = {fd, POLLIN, 0};
	return poll(&p, 1, 0) == 1 && (p.revents & POLLIN) != 0;
}

static int writable(int fd) {
	struct pollfd p = {fd, POLLOUT, 0};
	return poll(&p, 1, 0) == 1 && (p.revents & POLLOUT) != 0;
}

// held is how many messages of a size a socket takes before it will take no more, sent without
// waiting; what it then says is left in told. The other end then receives them all.
static int told;
static int held(int from, int to, int size) {
	static char bytes[4096], sink[4096];
	int n = 0;
	fcntl(from, F_SETFL, O_NONBLOCK);
	fcntl(to, F_SETFL, O_NONBLOCK);
	while (n < 10000 && send(from, bytes, size, 0) == size)
		n++;
	told = errno;
	while (recv(to, sink, sizeof sink, 0) >= 0)
		;
	fcntl(from, F_SETFL, 0);
	fcntl(to, F_SETFL, 0);
	return n;
}

static struct sockaddr_un at(const char *path) {
	struct sockaddr_un a = {.sun_family = AF_UNIX};
	strlcpy(a.sun_path, path, sizeof a.sun_path);
	a.sun_len = (unsigned char)SUN_LEN(&a);
	return a;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGPIPE, SIG_IGN);
	int s[2];
	printf("a pair: %s", said(socketpair(AF_UNIX, SOCK_DGRAM, 0, s)));
	printf("; its kind %d, room to send %d, to receive %d\n", option(s[0], SO_TYPE), option(s[0], SO_SNDBUF), option(s[0], SO_RCVBUF));

	// Each send is a message of its own.
	char got[64] = "";
	printf("three sent, of 5, of none and of 3: %s", said(send(s[0], "first", 5, 0)));
	printf(" %s", said(send(s[0], "", 0, 0)));
	printf(" %s; something to read %d\n", said(send(s[0], "end", 3, 0)), readable(s[1]));
	printf("received with room for all: %s", said(recv(s[1], got, sizeof got, 0)));
	printf(" \"%.5s\"; then %s", got, said(recv(s[1], got, sizeof got, 0)));
	printf("; then %s", said(recv(s[1], got, sizeof got, 0)));
	printf(" \"%.3s\"; something to read %d\n", got, readable(s[1]));
	send(s[0], "abcdefgh", 8, 0);
	send(s[0], "next", 4, 0);
	memset(got, 0, sizeof got);
	struct iovec part = {got, 3};
	struct msghdr m = {.msg_iov = &part, .msg_iovlen = 1};
	ssize_t n = recvmsg(s[1], &m, 0);
	printf("one of 8 received with room for 3: %zd \"%s\", marked %#x", n, got, m.msg_flags);
	memset(got, 0, sizeof got);
	printf("; the next: %s \"%s\"\n", said(recv(s[1], got, sizeof got, 0)), got);
	send(s[0], "peeked", 6, 0);
	int toRead = -1;
	printf("with one of 6 waiting, said to be there to read: %s, %d\n", said(ioctl(s[1], FIONREAD, &toRead)), toRead);
	memset(got, 0, sizeof got);
	printf("looked at: %s", said(recv(s[1], got, sizeof got, MSG_PEEK)));
	printf(" \"%s\"; and received: %s\n", got, said(recv(s[1], got, sizeof got, 0)));
	fcntl(s[1], F_SETFL, O_NONBLOCK);
	printf("with nothing there and not waiting: %s; it can be written %d\n", said(recv(s[1], got, sizeof got, 0)), writable(s[0]));
	fcntl(s[1], F_SETFL, 0);

	// How large a message may be, and how many a socket holds.
	static char large[8192];
	printf("a message of 2048: %s", said(send(s[0], large, 2048, 0)));
	printf("; it can then be written %d", writable(s[0]));
	printf("; of 2049: %s", said(send(s[0], large, 2049, 0)));
	n = recv(s[1], large, sizeof large, 0);
	printf("; received %zd\n", n);
	// Of sizes for which the room in bytes decides it: for smaller ones it is how the kernel
	// keeps each message, which is not the same on every release.
	int sizes[] = {400, 1000, 2048};
	printf("how many it holds:");
	for (unsigned i = 0; i < sizeof sizes / sizeof *sizes; i++) {
		int count = held(s[0], s[1], sizes[i]);
		printf(" of %d bytes %d (%s)%s", sizes[i], count, strerror(told), i + 1 < sizeof sizes / sizeof *sizes ? "," : "\n");
	}

	// The other gone.
	close(s[0]);
	printf("the other closed: received %s", said(recv(s[1], got, sizeof got, 0)));
	printf("; sent %s; something to read %d\n", said(send(s[1], "x", 1, 0)), readable(s[1]));
	close(s[1]);

	// Bound to a path, and sent to by one that is not.
	char path[104], gone[104];
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(path, sizeof path, "%s/ld.%d", tmp, getpid());
	snprintf(gone, sizeof gone, "%s/ld.%d.none", tmp, getpid());
	struct sockaddr_un here = at(path), nowhere = at(gone);
	int server = socket(AF_UNIX, SOCK_DGRAM, 0), client = socket(AF_UNIX, SOCK_DGRAM, 0);
	printf("two made: %d %d; sent on with nobody named: %s", server >= 0, client >= 0, said(send(client, "x", 1, 0)));
	printf("; bound to a path: %s", said(bind(server, (struct sockaddr *)&here, sizeof here)));
	struct stat st;
	lstat(path, &st);
	printf("; the file a socket %d; listened on: %s\n", S_ISSOCK(st.st_mode), said(listen(server, 5)));
	printf("sent to the path: %s", said(sendto(client, "hello", 5, 0, (struct sockaddr *)&here, sizeof here)));
	printf("; to a path that is not there: %s\n", said(sendto(client, "x", 1, 0, (struct sockaddr *)&nowhere, sizeof nowhere)));
	struct sockaddr_un from;
	memset(&from, 0x55, sizeof from);
	socklen_t fromSize = sizeof from;
	memset(got, 0, sizeof got);
	n = recvfrom(server, got, sizeof got, 0, (struct sockaddr *)&from, &fromSize);
	printf("received: %zd \"%s\", from an address of %d bytes (it says %d), family %d\n", n, got, (int)fromSize, from.sun_len, from.sun_family);

	// A sender with a name of its own, and one that connects.
	char mine[104];
	snprintf(mine, sizeof mine, "%s/ld.%d.c", tmp, getpid());
	struct sockaddr_un me = at(mine);
	bind(client, (struct sockaddr *)&me, sizeof me);
	sendto(client, "named", 5, 0, (struct sockaddr *)&here, sizeof here);
	memset(&from, 0x55, sizeof from);
	fromSize = sizeof from;
	n = recvfrom(server, got, sizeof got, 0, (struct sockaddr *)&from, &fromSize);
	printf("from one with a name: %zd, an address of %d bytes (it says %d), its path %d", n, (int)fromSize, from.sun_len, !strcmp(from.sun_path, mine));
	printf("; answered to that address: %s", said(sendto(server, "back", 4, 0, (struct sockaddr *)&from, fromSize)));
	memset(got, 0, sizeof got);
	printf("; received %s \"%s\"\n", said(recv(client, got, sizeof got, 0)), got);
	sendto(client, "short", 5, 0, (struct sockaddr *)&here, sizeof here);
	memset(&from, 0x55, sizeof from);
	fromSize = 16;
	n = recvfrom(server, got, sizeof got, 0, (struct sockaddr *)&from, &fromSize);
	printf("from it again with room for 16 bytes of address: %zd, said to be %d, the 17th byte written %d", n, (int)fromSize, ((unsigned char *)&from)[16] != 0x55);
	sendto(client, "nolen", 5, 0, (struct sockaddr *)&here, sizeof here);
	memset(&from, 0x55, sizeof from);
	n = recvfrom(server, got, sizeof got, 0, (struct sockaddr *)&from, NULL);
	printf("; with nowhere to say how long: %s, anything written %d; accepted from: %s\n", said(n), ((unsigned char *)&from)[0] != 0x55, said(accept(server, NULL, NULL)));
	printf("connected to the path: %s", said(connect(client, (struct sockaddr *)&here, sizeof here)));
	printf("; sent: %s", said(send(client, "conn", 4, 0)));
	printf("; sent to an address all the same: %s", said(sendto(client, "x", 1, 0, (struct sockaddr *)&here, sizeof here)));
	printf("; connected to a path that is not there: %s\n", said(connect(client, (struct sockaddr *)&nowhere, sizeof nowhere)));
	recv(server, got, sizeof got, 0);
	int stream = socket(AF_UNIX, SOCK_STREAM, 0);
	printf("a stream socket connected to it: %s", said(connect(stream, (struct sockaddr *)&here, sizeof here)));
	close(server);
	printf("; the one bound closed: sent to by the one connected %s", said(send(client, "x", 1, 0)));
	int third = socket(AF_UNIX, SOCK_DGRAM, 0);
	printf(", by another to its path %s\n", said(sendto(third, "x", 1, 0, (struct sockaddr *)&here, sizeof here)));
	printf("tidied: %s %s\n", said(unlink(path)), said(unlink(mine)));
	return 0;
}
