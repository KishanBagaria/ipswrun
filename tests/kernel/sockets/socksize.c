// A socket's two sizes (SO_SNDBUF, SO_RCVBUF) and what they do to a pair of stream sockets:
// how much goes in before the other has taken any is the sender's size for sending, whatever
// the other's for taking. And how an option is written into more room than it is, and less.
// It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

static int get(int s, int option) {
	int value = 0;
	socklen_t length = 4;
	getsockopt(s, SOL_SOCKET, option, &value, &length);
	return value;
}

// asked reads an option into so much room.
static void asked(const char *what, int s, int option, socklen_t room) {
	unsigned char buf[300];
	memset(buf, 0xAA, sizeof buf);
	socklen_t length = room;
	if (getsockopt(s, SOL_SOCKET, option, buf, &length)) {
		printf("%s into %u bytes: %s\n", what, room, strerror(errno));
		return;
	}
	int value = 0;
	memcpy(&value, buf, length < 4 ? length : 4);
	printf("%s into %u bytes: %u written, value %d, the byte after %s\n", what, room, length, value, buf[length] == 0xAA ? "untouched" : "written");
}

// filled makes a pair, sets the sender's size for sending and the other's for taking (0 to
// leave one as it is made), and writes to it so much at a time until no more goes in.
static void filled(const char *what, int send, int take, int chunk) {
	int pair[2];
	socketpair(AF_UNIX, SOCK_STREAM, 0, pair);
	const char *a = "-", *b = "-";
	if (send)
		a = setsockopt(pair[0], SOL_SOCKET, SO_SNDBUF, &send, 4) ? strerror(errno) : "set";
	if (take)
		b = setsockopt(pair[1], SOL_SOCKET, SO_RCVBUF, &take, 4) ? strerror(errno) : "set";
	fcntl(pair[0], F_SETFL, O_NONBLOCK);
	static char buf[1 << 17];
	long total = 0;
	ssize_t n;
	while ((n = write(pair[0], buf, chunk)) > 0)
		total += n;
	printf("%s (send %s, take %s): the sender says %d and %d, the taker %d and %d; %ld bytes go in by %d at a time, then %s\n", what, a, b, get(pair[0], SO_SNDBUF), get(pair[0], SO_RCVBUF), get(pair[1], SO_SNDBUF),
	       get(pair[1], SO_RCVBUF), total, chunk, strerror(errno));
	close(pair[0]);
	close(pair[1]);
}

int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	int pair[2], datagrams[2];
	socketpair(AF_UNIX, SOCK_STREAM, 0, pair);
	asked("a stream socket's kind", pair[0], SO_TYPE, 4);
	asked("its kind", pair[0], SO_TYPE, 256);
	asked("its kind", pair[0], SO_TYPE, 8);
	asked("its kind", pair[0], SO_TYPE, 2);
	asked("its kind", pair[0], SO_TYPE, 0);
	asked("what it sends at once", pair[0], SO_SNDBUF, 256);
	asked("what it takes", pair[0], SO_RCVBUF, 4);
	asked("an error", pair[0], SO_ERROR, 256);
	socketpair(AF_UNIX, SOCK_DGRAM, 0, datagrams);
	asked("a datagram socket's kind", datagrams[0], SO_TYPE, 256);
	asked("what it sends at once", datagrams[0], SO_SNDBUF, 4);
	asked("what it takes", datagrams[0], SO_RCVBUF, 4);

	filled("as made", 0, 0, 1);
	filled("as made", 0, 0, 1000);
	filled("as made", 0, 0, 100000);
	filled("the sender's 4096", 4096, 0, 1);
	filled("the sender's 4096", 4096, 0, 1000);
	filled("the taker's 4096", 0, 4096, 1);
	filled("both 4096", 4096, 4096, 1);
	filled("the sender's 65536", 65536, 0, 1);
	filled("the taker's 65536", 0, 65536, 1);
	filled("both 65536", 65536, 65536, 1000);
	filled("the sender's 1", 1, 0, 1);
	filled("the sender's below nothing", (int)0x80000000, 0, 1);
	return 0;
}
