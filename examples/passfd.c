// Messages over a socket in parts and with descriptors in them (sendmsg, recvmsg, SCM_RIGHTS),
// as a program hands another an open file: what is sent from several places arrives as one,
// a descriptor sent arrives as a new one for the same open file, and what happens to those
// sent when the receiver has no room for them, asks with recv, or has room for some. Over the
// two of a pair, and to another process. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/uio.h>
#include <sys/wait.h>
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

// open descriptors there are, of the first 64.
static int count(void) {
	int n = 0;
	for (int fd = 0; fd < 64; fd++)
		if (fcntl(fd, F_GETFD) != -1)
			n++;
	return n;
}

// sent sends some bytes with descriptors beside them.
static long sent(int to, const char *bytes, const int *fds, int nfds) {
	char control[CMSG_SPACE(4 * sizeof(int))];
	memset(control, 0, sizeof control);
	struct iovec part = {(void *)bytes, strlen(bytes)};
	struct msghdr m = {.msg_iov = &part, .msg_iovlen = 1};
	if (nfds) {
		m.msg_control = control;
		m.msg_controllen = CMSG_SPACE(nfds * sizeof(int));
		struct cmsghdr *c = CMSG_FIRSTHDR(&m);
		c->cmsg_level = SOL_SOCKET;
		c->cmsg_type = SCM_RIGHTS;
		c->cmsg_len = CMSG_LEN(nfds * sizeof(int));
		memcpy(CMSG_DATA(c), fds, nfds * sizeof(int));
	}
	return sendmsg(to, &m, 0);
}

// got receives into room for so many descriptors, and says what came: how many bytes, how
// much beside them, what kind, how many descriptors, and what the message was marked with.
static int came[4];
static const char *got(int from, int room) {
	static char text[4][160];
	static int next;
	char *t = text[next++ % 4];
	char bytes[64] = "", control[CMSG_SPACE(4 * sizeof(int))];
	memset(control, 0x55, sizeof control);
	struct iovec part = {bytes, sizeof bytes - 1};
	struct msghdr m = {.msg_iov = &part, .msg_iovlen = 1, .msg_control = room >= 0 ? control : NULL, .msg_controllen = room > 0 ? CMSG_SPACE(room * sizeof(int)) : room == 0 ? sizeof(struct cmsghdr) : 0};
	m.msg_flags = 0x5555;
	came[0] = came[1] = came[2] = came[3] = -1;
	ssize_t n = recvmsg(from, &m, 0);
	if (n < 0) {
		snprintf(t, 160, "-1 (%s)", strerror(errno));
		return t;
	}
	int fds = 0, level = -1, type = -1, length = -1;
	struct cmsghdr *c = m.msg_controllen >= sizeof *c ? CMSG_FIRSTHDR(&m) : NULL;
	if (c) {
		level = c->cmsg_level, type = c->cmsg_type, length = (int)c->cmsg_len;
		fds = (int)((c->cmsg_len - CMSG_LEN(0)) / sizeof(int));
		if (fds > 0 && fds <= 4)
			memcpy(came, CMSG_DATA(c), fds * sizeof(int));
	}
	snprintf(t, 160, "%zd \"%s\", %d beside (level %d kind %d length %d), %d descriptors, marked %#x", n, bytes, (int)m.msg_controllen, level, type, length, fds, m.msg_flags);
	return t;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGPIPE, SIG_IGN);
	int s[2], ends[2], more[2];
	socketpair(AF_UNIX, SOCK_STREAM, 0, s);
	pipe(ends);
	pipe(more);
	int before = count();

	// In parts, and with nothing beside.
	struct iovec parts[3] = {{"one ", 4}, {"", 0}, {"two", 3}};
	struct msghdr m = {.msg_iov = parts, .msg_iovlen = 3};
	printf("sent from three places: %s", said(sendmsg(s[0], &m, 0)));
	printf("; received: %s\n", got(s[1], 2));
	char a[4] = "", b[8] = "";
	struct iovec into[2] = {{a, 3}, {b, 7}};
	struct msghdr r = {.msg_iov = into, .msg_iovlen = 2};
	send(s[0], "abcdefg", 7, 0);
	ssize_t n = recvmsg(s[1], &r, 0);
	printf("received into two places: %zd \"%s\" \"%s\", marked %#x\n", n, a, b, r.msg_flags);

	// One descriptor.
	printf("a pipe's writing end sent beside one byte: %s", said(sent(s[0], "x", &ends[1], 1)));
	printf("; open descriptors %d more than before\n", count() - before);
	int lowest = dup(0);
	close(lowest);
	printf("received: %s", got(s[1], 1));
	int passed = came[0];
	printf("; it is a new one %d, the lowest free %d, open descriptors %d more\n", passed != ends[1] && passed >= 0, passed == lowest, count() - before);
	char c = 0;
	printf("written through it: %s", said(write(passed, "p", 1)));
	printf("; read from the pipe: %s '%c'; closed on a call of exec: %d\n", said(read(ends[0], &c, 1)), c, fcntl(passed, F_GETFD));
	close(passed);

	// Two in one message, with room for both, for one, and for none.
	int two[2] = {ends[1], more[1]};
	sent(s[0], "both", two, 2);
	printf("two sent, received with room for both: %s; open %d more\n", got(s[1], 2), count() - before);
	close(came[0]);
	close(came[1]);
	sent(s[0], "some", two, 2);
	printf("with room for one: %s; open %d more\n", got(s[1], 1), count() - before);
	if (came[0] >= 0)
		close(came[0]);
	sent(s[0], "none", two, 2);
	printf("with room for a heading and no more: %s; open %d more\n", got(s[1], 0), count() - before);
	sent(s[0], "nil", two, 2);
	printf("with nowhere for it: %s; open %d more\n", got(s[1], -1), count() - before);
	sent(s[0], "recv", two, 2);
	char plain[16] = "";
	printf("received by recv: %s", said(recv(s[1], plain, sizeof plain - 1, 0)));
	printf(" \"%s\"; open %d more\n", plain, count() - before);

	// Two messages, each with a descriptor, received with room for all the bytes.
	sent(s[0], "ab", &ends[1], 1);
	sent(s[0], "cd", &more[1], 1);
	printf("two messages with a descriptor each, the first received: %s\n", got(s[1], 4));
	if (came[0] >= 0)
		close(came[0]);
	printf("the second: %s\n", got(s[1], 4));
	if (came[0] >= 0)
		close(came[0]);
	send(s[0], "plain", 5, 0);
	sent(s[0], "with", &ends[1], 1);
	printf("bytes sent alone and then some with a descriptor, received: %s\n", got(s[1], 4));
	if (came[0] >= 0)
		close(came[0]);
	printf("and then: %s\n", got(s[1], 4));
	if (came[0] >= 0)
		close(came[0]);

	sent(s[0], "first", &ends[1], 1);
	send(s[0], "after", 5, 0);
	printf("some with a descriptor and then bytes alone, received: %s\n", got(s[1], 4));
	if (came[0] >= 0)
		close(came[0]);
	fcntl(s[1], F_SETFL, O_NONBLOCK);
	printf("and then: %s\n", got(s[1], 4));
	fcntl(s[1], F_SETFL, 0);
	sent(s[0], "longer", &ends[1], 1);
	char part[4] = "";
	printf("of one with a descriptor, the first three bytes by recv: %s", said(recv(s[1], part, 3, 0)));
	printf(" \"%s\"; the rest: %s; open %d more\n", part, got(s[1], 4), count() - before);

	// What is refused.
	int bad = 99;
	printf("a descriptor that is not open sent: %s", said(sent(s[0], "z", &bad, 1)));
	char control[CMSG_SPACE(sizeof(int))];
	memset(control, 0, sizeof control);
	struct iovec one = {"q", 1};
	struct msghdr odd = {.msg_iov = &one, .msg_iovlen = 1, .msg_control = control, .msg_controllen = sizeof control};
	struct cmsghdr *h = CMSG_FIRSTHDR(&odd);
	h->cmsg_level = SOL_SOCKET, h->cmsg_type = 99, h->cmsg_len = CMSG_LEN(sizeof(int));
	printf("; something of a kind there is not: %s", said(sendmsg(s[0], &odd, 0)));
	h->cmsg_type = SCM_RIGHTS, h->cmsg_level = 99;
	printf("; of a level there is not: %s", said(sendmsg(s[0], &odd, 0)));
	struct msghdr nothing = {0};
	printf("; a message of nothing: %s\n", said(sendmsg(s[0], &nothing, 0)));
	odd.msg_controllen = 0;
	printf("something beside said to be of no length: %s", said(sendmsg(s[0], &odd, 0)));
	char twice[2 * CMSG_SPACE(sizeof(int))];
	memset(twice, 0, sizeof twice);
	struct msghdr pair = {.msg_iov = &one, .msg_iovlen = 1, .msg_control = twice, .msg_controllen = sizeof twice};
	for (struct cmsghdr *each = CMSG_FIRSTHDR(&pair); each; each = CMSG_NXTHDR(&pair, each)) {
		each->cmsg_level = SOL_SOCKET, each->cmsg_type = SCM_RIGHTS, each->cmsg_len = CMSG_LEN(sizeof(int));
		memcpy(CMSG_DATA(each), &ends[1], sizeof(int));
	}
	printf("; two lots of descriptors in one message: %s", said(sendmsg(s[0], &pair, 0)));
	struct msghdr empty = {.msg_control = control, .msg_controllen = sizeof control};
	h->cmsg_level = SOL_SOCKET, h->cmsg_type = SCM_RIGHTS;
	memcpy(CMSG_DATA(h), &ends[1], sizeof(int));
	printf("; descriptors with no place for bytes: %s; received into no place: %s\n", said(sendmsg(s[0], &empty, 0)), said(recvmsg(s[1], &nothing, 0)));
	fcntl(s[1], F_SETFL, O_NONBLOCK);
	printf("what is left to receive: %s\n", got(s[1], 4));
	fcntl(s[1], F_SETFL, 0);

	// What a receive says in the message besides, and how much room a descriptor takes.
	send(s[0], "flags", 5, 0);
	char few[8] = "";
	struct iovec small = {few, 7};
	struct msghdr asked = {.msg_iov = &small, .msg_iovlen = 1, .msg_controllen = 77, .msg_namelen = 55, .msg_flags = 0x5555};
	n = recvmsg(s[1], &asked, MSG_DONTWAIT);
	printf("received without waiting, with nowhere for what is beside or for a name: %zd, marked %#x, beside said to be %d, the name %d\n", n, asked.msg_flags, (int)asked.msg_controllen, (int)asked.msg_namelen);
	static char large[8192];
	memset(large, 'L', sizeof large);
	struct iovec all = {large, sizeof large};
	memset(control, 0, sizeof control);
	struct msghdr big = {.msg_iov = &all, .msg_iovlen = 1, .msg_control = control, .msg_controllen = sizeof control};
	h = CMSG_FIRSTHDR(&big);
	h->cmsg_level = SOL_SOCKET, h->cmsg_type = SCM_RIGHTS, h->cmsg_len = CMSG_LEN(sizeof(int));
	memcpy(CMSG_DATA(h), &ends[1], sizeof(int));
	fcntl(s[0], F_SETFL, O_NONBLOCK);
	printf("all a socket holds sent with a descriptor, not waiting: %s", said(sendmsg(s[0], &big, 0)));
	printf("; one byte more: %s\n", said(send(s[0], "m", 1, 0)));
	fcntl(s[0], F_SETFL, 0);
	static char drain[16384];
	long drained = recv(s[1], drain, 0, 0);
	printf("received into no room: %s; open %d more", said(drained), count() - before);
	drained = 0;
	fcntl(s[1], F_SETFL, O_NONBLOCK);
	for (ssize_t k; (k = recv(s[1], drain, sizeof drain, 0)) > 0;)
		drained += k;
	fcntl(s[1], F_SETFL, 0);
	printf("; then all of it: %ld; open %d more\n", drained, count() - before);

	// To another process.
	pid_t child = fork();
	if (child == 0) {
		got(s[1], 1);
		_exit(came[0] >= 0 && write(came[0], "c", 1) == 1 ? 0 : 1);
	}
	sent(s[0], "child", &ends[1], 1);
	int status = 0;
	waitpid(child, &status, 0);
	c = 0;
	printf("another process is sent the pipe's end and writes to it: it ended with %d, and the pipe has %s '%c'\n", WEXITSTATUS(status), said(read(ends[0], &c, 1)), c);

	// The sender's own closed after sending, and the socket closed with one in it.
	int spare[2];
	pipe(spare);
	sent(s[0], "kept", &spare[1], 1);
	close(spare[1]);
	printf("sent, then the sender's own closed: received %s", got(s[1], 1));
	printf("; written through it: %s\n", said(write(came[0], "k", 1)));
	close(came[0]);
	fcntl(spare[0], F_SETFL, O_NONBLOCK);
	read(spare[0], &c, 1);
	pipe(more);
	sent(s[0], "lost", &more[1], 1);
	close(more[1]);
	close(s[0]);
	close(s[1]);
	fcntl(more[0], F_SETFL, O_NONBLOCK);
	printf("one sent and never received, the sockets closed: the pipe's reader is told %s\n", said(read(more[0], &c, 1)));
	return 0;
}
