// Sockets of the local domain with a name in the file system, as a server and its clients have
// them: one is bound to a path and listens, others connect to the path and are accepted, and
// the two ends then talk as the ends of a pair do. What each call says when the path is not
// there, is taken, is not listened on or has been removed, what the file is to stat and to
// open, and what the ends are named. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
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

static struct sockaddr_un at(const char *path) {
	struct sockaddr_un a = {.sun_family = AF_UNIX};
	strlcpy(a.sun_path, path, sizeof a.sun_path);
	a.sun_len = (unsigned char)SUN_LEN(&a);
	return a;
}

// named says what a socket's own name or its peer's is: how long, of what family, and whether
// it is the path.
static const char *named(int fd, int peer, const char *path) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	struct sockaddr_un a;
	memset(&a, 0x55, sizeof a);
	socklen_t n = sizeof a;
	int r = peer ? getpeername(fd, (struct sockaddr *)&a, &n) : getsockname(fd, (struct sockaddr *)&a, &n);
	if (r)
		snprintf(t, 96, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 96, "%d bytes (it says %d), family %d, the path %d", (int)n, a.sun_len, a.sun_family, n > 2 && !strcmp(a.sun_path, path));
	return t;
}

static int readable(int fd) {
	struct pollfd p = {fd, POLLIN, 0};
	return poll(&p, 1, 0) == 1 && (p.revents & POLLIN) != 0;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGPIPE, SIG_IGN); // a send to an end that is closed says so, and does not end the program
	char path[104], gone[104];
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(path, sizeof path, "%s/ls.%d", tmp, getpid());
	snprintf(gone, sizeof gone, "%s/ls.%d.none", tmp, getpid());
	struct sockaddr_un here = at(path), nowhere = at(gone);

	int server = socket(AF_UNIX, SOCK_STREAM, 0), client = socket(AF_UNIX, SOCK_STREAM, 0);
	printf("two sockets made: %d %d; one's name before it has one: %s\n", server >= 0, client >= 0, named(server, 0, path));
	char none[8];
	printf("sent on before it is connected: %s", said(send(client, "x", 1, 0)));
	printf("; received from: %s", said(recv(client, none, sizeof none, 0)));
	printf("; its peer's name: %s\n", named(client, 1, path));
	printf("connected to a path that is not there: %s", said(connect(client, (struct sockaddr *)&nowhere, sizeof nowhere)));
	printf("; listened on with no name: %s", said(listen(client, 5)));
	printf("; accepted from, not listening: %s\n", said(accept(client, NULL, NULL)));

	printf("bound to a path: %s", said(bind(server, (struct sockaddr *)&here, sizeof here)));
	struct stat st;
	int statted = lstat(path, &st);
	printf("; the file: %s, a socket %d, mode %o", said(statted), S_ISSOCK(st.st_mode), st.st_mode & 07777);
	printf("; opened as a file: %s\n", said(open(path, O_RDWR)));
	printf("bound again: %s", said(bind(server, (struct sockaddr *)&here, sizeof here)));
	int other = socket(AF_UNIX, SOCK_STREAM, 0);
	printf("; another bound to the same path: %s", said(bind(other, (struct sockaddr *)&here, sizeof here)));
	printf("; its name now: %s\n", named(server, 0, path));
	printf("connected to before it listens: %s\n", said(connect(client, (struct sockaddr *)&here, sizeof here)));

	printf("listening: %s; someone waiting %d", said(listen(server, 5)), readable(server));
	fcntl(server, F_SETFL, O_NONBLOCK);
	printf("; accepted with nobody there and not waiting: %s\n", said(accept(server, NULL, NULL)));
	printf("a client connected: %s; someone waiting %d", said(connect(client, (struct sockaddr *)&here, sizeof here)), readable(server));
	printf("; connected again: %s\n", said(connect(client, (struct sockaddr *)&here, sizeof here)));
	struct sockaddr_un from;
	memset(&from, 0x55, sizeof from);
	socklen_t fromSize = sizeof from;
	int served = accept(server, (struct sockaddr *)&from, &fromSize);
	printf("accepted: %d, from an address of %d bytes, family %d; someone waiting %d\n", served >= 0, (int)fromSize, from.sun_family, readable(server));
	printf("the accepted end's name: %s; its peer's: %s\n", named(served, 0, path), named(served, 1, path));
	printf("the client's name: %s; its peer's: %s\n", named(client, 0, path), named(client, 1, path));

	char got[64] = "";
	printf("the client sends 5: %s", said(send(client, "hello", 5, 0)));
	printf("; the server receives: %s", said(recv(served, got, sizeof got, 0)));
	printf(" \"%.5s\"; and answers 3: %s", got, said(send(served, "yes", 3, 0)));
	printf("; the client receives: %s", said(recv(client, got, sizeof got, 0)));
	printf(" \"%.3s\"\n", got);
	close(client);
	printf("the client closed: the server receives %s", said(recv(served, got, sizeof got, 0)));
	printf(", and sending to it: %s\n", said(send(served, "x", 1, 0)));
	close(served);

	// A client that is another process, waited for.
	fcntl(server, F_SETFL, 0);
	pid_t child = fork();
	if (child == 0) {
		int c = socket(AF_UNIX, SOCK_STREAM, 0);
		if (connect(c, (struct sockaddr *)&here, sizeof here) || send(c, "child", 5, 0) != 5)
			_exit(1);
		char back[8];
		_exit(recv(c, back, sizeof back, 0) == 2 ? 0 : 2);
	}
	served = accept(server, NULL, NULL);
	ssize_t n = recv(served, got, sizeof got, 0);
	printf("another process connects and is accepted: %d; it sent %zd \"%.5s\"", served >= 0, n, got);
	send(served, "ok", 2, 0);
	int status = 0;
	waitpid(child, &status, 0);
	printf("; it ended with %d\n", WEXITSTATUS(status));
	close(served);

	// More than may wait, and a name asked for with too little room for it.
	int few = socket(AF_UNIX, SOCK_STREAM, 0), connected = 0;
	char fewPath[104];
	snprintf(fewPath, sizeof fewPath, "%s/ls.%d.few", tmp, getpid());
	struct sockaddr_un fewAt = at(fewPath);
	bind(few, (struct sockaddr *)&fewAt, sizeof fewAt);
	listen(few, 2);
	int last = 0;
	for (int i = 0; i < 6; i++) {
		int c = socket(AF_UNIX, SOCK_STREAM, 0);
		if (connect(c, (struct sockaddr *)&fewAt, sizeof fewAt) == 0)
			connected++;
		else
			last = errno;
	}
	printf("of six that connect to one that lets two wait: %d connected, the rest told %s\n", connected, strerror(last));
	struct sockaddr_un small;
	memset(&small, 0x55, sizeof small);
	socklen_t room = 8;
	int asked = getsockname(few, (struct sockaddr *)&small, &room);
	printf("its name asked for with room for 8 bytes: %s, said to be %d long, the ninth byte written %d\n", said(asked), (int)room, ((unsigned char *)&small)[8] != 0x55);
	struct sockaddr_un zero = at(fewPath);
	zero.sun_family = 0;
	int z = socket(AF_UNIX, SOCK_STREAM, 0);
	printf("an address that names no family, connected to: %s", said(connect(z, (struct sockaddr *)&zero, sizeof zero)));
	printf("; an address with nowhere to read it, bound to: %s, connected to: %s\n", said(bind(z, NULL, 16)), said(connect(z, NULL, 16)));
	printf("made anew with a name that is a socket's: %s\n", said(open(fewPath, O_CREAT | O_EXCL | O_RDWR, 0644)));
	unlink(fewPath);

	printf("the path removed: %s", said(unlink(path)));
	client = socket(AF_UNIX, SOCK_STREAM, 0);
	printf("; connected to then: %s", said(connect(client, (struct sockaddr *)&here, sizeof here)));
	printf("; bound to again by another: %s\n", said(bind(other, (struct sockaddr *)&here, sizeof here)));
	close(server);
	printf("the first closed, the path now the other's, which does not listen: connected to %s\n", said(connect(client, (struct sockaddr *)&here, sizeof here)));
	close(other);
	printf("that one closed too, its file still there %d: connected to %s\n", lstat(path, &st) == 0, said(connect(client, (struct sockaddr *)&here, sizeof here)));
	printf("tidied: %s\n", said(unlink(path)));
	return 0;
}
