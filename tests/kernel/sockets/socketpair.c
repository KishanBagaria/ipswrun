// A pair of connected sockets: what is sent into one is received from the other, both ways, a
// receiver waits for a sender and a sender for room, and each is told when the other has gone or
// has said it will send no more. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/ioctl.h>
#include <sys/select.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/uio.h>
#include <sys/un.h>
#include <unistd.h>

static char big[100000], sink[100000];

// said is what a call answered: its result, and the error if it failed.
static const char *said(ssize_t r) {
	static char text[8][64];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 64, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 64, "%zd", r);
	return t;
}

static int option(int fd, int name) {
	int value = -1;
	socklen_t size = sizeof value;
	return getsockopt(fd, SOL_SOCKET, name, &value, &size) ? -errno : value;
}

static void status(const char *what, int fd) {
	struct stat s;
	int waiting = -1;
	fstat(fd, &s);
	ioctl(fd, FIONREAD, &waiting);
	printf("%s: mode %o, %d links, size %lld, %lld blocks of %d, device %d, mine %d, flags %#x, %d to read (%d), %d sent and not read\n", what, s.st_mode,
	       s.st_nlink, s.st_size, s.st_blocks, s.st_blksize, s.st_dev, s.st_uid == getuid() && s.st_gid == getgid(), fcntl(fd, F_GETFL), waiting,
	       option(fd, SO_NREAD), option(fd, SO_NWRITE));
}

// ready asks select, without waiting, whether a descriptor can be read or written.
static int ready(int fd, int writing) {
	fd_set set;
	struct timeval now = {0, 0};
	FD_ZERO(&set);
	FD_SET(fd, &set);
	int n = select(fd + 1, writing ? NULL : &set, writing ? &set : NULL, NULL, &now);
	return n == 1 && FD_ISSET(fd, &set) ? 1 : n;
}

// polled asks poll the same, about everything.
static int polled(int fd) {
	struct pollfd one = {fd, POLLIN | POLLOUT | POLLPRI | POLLRDBAND | POLLWRBAND, 0};
	return poll(&one, 1, 0) < 0 ? -1 : one.revents;
}

// watched asks a kqueue what it has to say of reading and of writing a descriptor.
static void watched(const char *what, int fd) {
	int kq = kqueue();
	struct kevent change[2], got[2];
	EV_SET(&change[0], fd, EVFILT_READ, EV_ADD, 0, 0, NULL);
	EV_SET(&change[1], fd, EVFILT_WRITE, EV_ADD, 0, 0, NULL);
	struct timespec now = {0, 0};
	int n = kevent(kq, change, 2, got, 2, &now);
	printf("%s, to a kqueue: %d", what, n);
	for (int i = 0; i < n; i++)
		printf("; %s flags %#x fflags %#x data %ld", got[i].filter == EVFILT_READ ? "read" : "write", got[i].flags, got[i].fflags, got[i].data);
	printf("\n");
	close(kq);
}

struct job {
	int fd;
	size_t size;
	useconds_t after;
	ssize_t result;
	int error;
};

static void *writer(void *arg) {
	struct job *j = arg;
	if (j->after)
		usleep(j->after);
	j->result = write(j->fd, big, j->size);
	j->error = errno;
	return NULL;
}

static void *closer(void *arg) {
	struct job *j = arg;
	usleep(j->after);
	j->result = close(j->fd);
	return NULL;
}

static void *shutter(void *arg) {
	struct job *j = arg;
	usleep(j->after);
	j->result = shutdown(j->fd, (int)j->size);
	return NULL;
}

static void pair(int s[2]) {
	if (socketpair(AF_UNIX, SOCK_STREAM, 0, s)) {
		printf("socketpair: %s\n", strerror(errno));
		exit(1);
	}
}

static void nowait(int fd) { fcntl(fd, F_SETFL, O_NONBLOCK); }

static void handler(int sig) { write(1, "  SIGPIPE\n", 10); }

int main(void) {
	int s[2];
	char buf[64] = "";
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGPIPE, SIG_IGN);
	for (size_t i = 0; i < sizeof big; i++)
		big[i] = 'a' + i % 26;
	printf("socketpair: %s, its descriptors one after the other: %d\n", said(socketpair(AF_UNIX, SOCK_STREAM, 0, s)), s[1] == s[0] + 1);
	status("one", s[0]);
	printf("its type %d, room to send %d and to receive %d, least to send %d and to receive %d, error %d, no signal %d, close on exec %d\n", option(s[0], SO_TYPE),
	       option(s[0], SO_SNDBUF), option(s[0], SO_RCVBUF), option(s[0], SO_SNDLOWAT), option(s[0], SO_RCVLOWAT), option(s[0], SO_ERROR), option(s[0], SO_NOSIGPIPE),
	       fcntl(s[0], F_GETFD));
	struct sockaddr_un name;
	socklen_t size = sizeof name;
	memset(&name, 0xAA, sizeof name);
	printf("its name: %s", said(getsockname(s[0], (struct sockaddr *)&name, &size)));
	printf(", %d bytes: length %d, family %d, path \"%s\"", size, name.sun_len, name.sun_family, name.sun_path);
	size = sizeof name;
	memset(&name, 0xAA, sizeof name);
	printf("; the other's: %s", said(getpeername(s[0], (struct sockaddr *)&name, &size)));
	printf(", %d bytes: length %d, family %d, path \"%s\"\n", size, name.sun_len, name.sun_family, name.sun_path);

	// Both ways, by write and read and by send and recv.
	printf("write 5 to one: %s", said(write(s[0], "hello", 5)));
	printf("; send 3 to the other: %s\n", said(send(s[1], "abc", 3, 0)));
	status("one, sent 3 and written 5", s[0]);
	status("the other", s[1]);
	printf("can be read: %d and %d, written: %d and %d, to poll: %#x and %#x\n", ready(s[0], 0), ready(s[1], 0), ready(s[0], 1), ready(s[1], 1), polled(s[0]), polled(s[1]));
	watched("the other", s[1]);
	ssize_t n = recv(s[1], buf, 10, MSG_PEEK);
	printf("a look at what the other has: %s \"%.*s\"", said(n), n > 0 ? (int)n : 0, buf);
	n = read(s[1], buf, 2);
	printf("; read 2: %s \"%.*s\"", said(n), n > 0 ? (int)n : 0, buf);
	n = recv(s[1], buf, 10, 0);
	printf("; recv 10: %s \"%.*s\"", said(n), n > 0 ? (int)n : 0, buf);
	n = read(s[0], buf, 10);
	printf("; one reads: %s \"%.*s\"\n", said(n), n > 0 ? (int)n : 0, buf);
	printf("read none of nothing: %s; lseek: %s; pread: %s; pwrite: %s\n", said(read(s[0], buf, 0)), said(lseek(s[0], 0, SEEK_SET)), said(pread(s[0], buf, 1, 0)),
	       said(pwrite(s[0], buf, 1, 0)));
	struct iovec parts[2] = {{"two ", 4}, {"parts", 5}}, into[2] = {{buf, 3}, {buf + 10, 20}};
	printf("writev of two parts: %s", said(writev(s[0], parts, 2)));
	n = readv(s[1], into, 2);
	printf("; readv into two: %s \"%.3s\" \"%.*s\"\n", said(n), buf, n > 3 ? (int)n - 3 : 0, buf + 10);
	printf("recv with nothing there, not to wait: %s", said(recv(s[1], buf, 10, MSG_DONTWAIT)));
	printf("; the descriptor still waits: flags %#x\n", fcntl(s[1], F_GETFL));

	// Without waiting: how much goes in, and when more does.
	nowait(s[0]);
	nowait(s[1]);
	printf("no waiting: flags %#x; read: %s\n", fcntl(s[0], F_GETFL), said(read(s[0], buf, 10)));
	printf("write 100000: %s", said(write(s[0], big, 100000)));
	printf("; one more: %s; can be written: %d, to poll: %#x and %#x\n", said(write(s[0], big, 1)), ready(s[0], 1), polled(s[0]), polled(s[1]));
	status("one, full", s[0]);
	status("the other, full", s[1]);
	watched("one, full", s[0]);
	watched("the other, full", s[1]);
	printf("the other way is its own: write 10: %s", said(write(s[1], big, 10)));
	printf(", read: %s\n", said(read(s[0], sink, 100)));
	static const int takes[] = {100, 1947, 1, 1000, 96, 3000};
	for (size_t i = 0; i < sizeof takes / sizeof *takes; i++) {
		printf("read %d: %s", takes[i], said(read(s[1], sink, takes[i])));
		printf("; can be written: %d, to poll: %#x", ready(s[0], 1), polled(s[0]));
		watched("", s[0]);
		printf("  write 3000: %s", said(write(s[0], big, 3000)));
		printf("; write 50: %s\n", said(write(s[0], big, 50)));
	}
	printf("read the rest: %s", said(read(s[1], sink, sizeof sink)));
	printf("; can be read: %d, written: %d\n", ready(s[1], 0), ready(s[0], 1));
	static const int chunks[] = {1, 7, 100, 3000, 3100, 5000, 8192, 8193};
	for (size_t i = 0; i < sizeof chunks / sizeof *chunks; i++) {
		long total = 0, calls = 0;
		while ((n = write(s[0], big, chunks[i])) > 0)
			total += n, calls++;
		printf("writes of %d until one is refused: %ld in %ld; ", chunks[i], total, calls);
		total = 0;
		while ((n = read(s[1], sink, sizeof sink)) > 0)
			total += n;
		printf("read back %ld\n", total);
	}
	fcntl(s[0], F_SETFL, 0);
	fcntl(s[1], F_SETFL, 0);

	// What a receiver is given is what was sent, in order, however it was cut up.
	write(s[0], big, 26);
	write(s[0], big + 26, 26);
	n = read(s[1], sink, 40);
	printf("two writes of 26, read 40: %s \"%.*s\"", said(n), n > 0 ? (int)n : 0, sink);
	n = read(s[1], sink, 40);
	printf("; read 40: %s \"%.*s\"\n", said(n), n > 0 ? (int)n : 0, sink);

	// A receiver waits for a sender, and a sender for room.
	pthread_t one, two;
	struct job w = {.fd = s[0], .size = 5, .after = 20000};
	pthread_create(&two, NULL, writer, &w);
	printf("a reader that waited: %s", said(read(s[1], sink, 16)));
	pthread_join(two, NULL);
	printf("; its writer: %s\n", said(w.result));
	w = (struct job){.fd = s[0], .size = 5, .after = 20000};
	pthread_create(&two, NULL, writer, &w);
	struct job w2 = {.fd = s[0], .size = 5, .after = 40000};
	pthread_create(&one, NULL, writer, &w2);
	printf("a recv of all of 8, sent in two halves: %s\n", said(recv(s[1], sink, 8, MSG_WAITALL)));
	pthread_join(two, NULL);
	pthread_join(one, NULL);
	printf("what is left: %s\n", said(read(s[1], sink, 16)));
	struct job all = {.fd = s[0], .size = sizeof big};
	pthread_create(&one, NULL, writer, &all);
	usleep(20000);
	status("a writer of 100000 waits; the other", s[1]);
	printf("can be written: %d\n", ready(s[0], 1));
	size_t total = 0;
	int same = 1;
	while (total < sizeof big) {
		n = read(s[1], sink, 3000);
		if (n <= 0)
			break;
		same = same && !memcmp(sink, big + total, n);
		total += n;
	}
	pthread_join(one, NULL);
	printf("read in all: %zu, as it was written: %d; the writer: %s\n", total, same, said(all.result));

	// select, poll and a kqueue wait for a sender.
	fd_set set;
	FD_ZERO(&set);
	FD_SET(s[1], &set);
	struct timeval soon = {0, 20000};
	printf("select with nothing sent, for a while: %s", said(select(s[1] + 1, &set, NULL, NULL, &soon)));
	w = (struct job){.fd = s[0], .size = 3, .after = 20000};
	pthread_create(&two, NULL, writer, &w);
	FD_ZERO(&set);
	FD_SET(s[1], &set);
	printf("; with a writer on its way: %s", said(select(s[1] + 1, &set, NULL, NULL, NULL)));
	printf(", in the set: %d\n", FD_ISSET(s[1], &set) != 0);
	pthread_join(two, NULL);
	read(s[1], sink, 16);
	struct pollfd asked = {s[1], POLLIN, 0};
	printf("poll with nothing sent, for a while: %s", said(poll(&asked, 1, 20)));
	w = (struct job){.fd = s[0], .size = 3, .after = 20000};
	pthread_create(&two, NULL, writer, &w);
	printf("; with a writer on its way: %s", said(poll(&asked, 1, -1)));
	printf(", it says %#x\n", asked.revents);
	pthread_join(two, NULL);
	read(s[1], sink, 16);
	int kq = kqueue();
	struct kevent change, got;
	EV_SET(&change, s[1], EVFILT_READ, EV_ADD, 0, 0, NULL);
	struct timespec little = {0, 20000000};
	printf("a kqueue with nothing sent, for a while: %s", said(kevent(kq, &change, 1, &got, 1, &little)));
	w = (struct job){.fd = s[0], .size = 3, .after = 20000};
	pthread_create(&two, NULL, writer, &w);
	printf("; with a writer on its way: %s", said(kevent(kq, NULL, 0, &got, 1, NULL)));
	printf(", flags %#x data %ld\n", got.flags, got.data);
	pthread_join(two, NULL);
	read(s[1], sink, 16);
	close(kq);

	// One says it will send no more: the other reads to the end, and can still answer.
	write(s[0], "last", 4);
	printf("one will send no more: %s", said(shutdown(s[0], SHUT_WR)));
	printf("; it writes: %s", said(write(s[0], "x", 1)));
	printf("; to poll: %#x and %#x; can be read: %d, written: %d\n", polled(s[0]), polled(s[1]), ready(s[1], 0), ready(s[0], 1));
	watched("one, sending no more", s[0]);
	watched("the other", s[1]);
	n = read(s[1], buf, 16);
	printf("the other reads: %s \"%.*s\", then %s and %s", said(n), n > 0 ? (int)n : 0, buf, said(read(s[1], buf, 16)), said(read(s[1], buf, 16)));
	printf("; it answers: %s", said(write(s[1], "answer", 6)));
	n = read(s[0], buf, 16);
	printf(", which one reads: %s \"%.*s\"\n", said(n), n > 0 ? (int)n : 0, buf);
	printf("the other will receive no more: %s", said(shutdown(s[1], SHUT_RD)));
	printf("; it reads: %s; to poll: %#x and %#x\n", said(read(s[1], buf, 16)), polled(s[0]), polled(s[1]));
	printf("again: %s; neither way: %s; a way there is not: %s; error %d\n", said(shutdown(s[0], SHUT_WR)), said(shutdown(s[1], SHUT_RDWR)), said(shutdown(s[1], 3)),
	       option(s[1], SO_ERROR));
	close(s[0]);
	close(s[1]);

	// One goes: the other reads what was sent and then the end, and cannot send.
	pair(s);
	write(s[0], "left behind", 11);
	printf("one closes: %s; to poll: %#x; can be read: %d, written: %d\n", said(close(s[0])), polled(s[1]), ready(s[1], 0), ready(s[1], 1));
	status("the other, alone", s[1]);
	watched("the other, alone", s[1]);
	n = read(s[1], buf, 32);
	printf("it reads: %s \"%.*s\", then %s", said(n), n > 0 ? (int)n : 0, buf, said(read(s[1], buf, 16)));
	printf("; it writes: %s, and sends: %s; error %d\n", said(write(s[1], "x", 1)), said(send(s[1], "x", 1, 0)), option(s[1], SO_ERROR));
	size = sizeof name;
	printf("the other's name: %s; it will send no more: %s\n", said(getpeername(s[1], (struct sockaddr *)&name, &size)), said(shutdown(s[1], SHUT_WR)));
	close(s[1]);

	// The signal for sending to nobody, and the ways not to have it.
	signal(SIGPIPE, handler);
	pair(s);
	close(s[0]);
	printf("write to nobody, with a handler: %s\n", said(write(s[1], "x", 1)));
	int on = 1;
	printf("no signal for this socket, asked too late: %s", said(setsockopt(s[1], SOL_SOCKET, SO_NOSIGPIPE, &on, sizeof on)));
	printf(", is %d, to fcntl %d; write: %s\n", option(s[1], SO_NOSIGPIPE), fcntl(s[1], F_GETNOSIGPIPE), said(write(s[1], "x", 1)));
	close(s[1]);
	pair(s);
	printf("asked in time: %s", said(setsockopt(s[1], SOL_SOCKET, SO_NOSIGPIPE, &on, sizeof on)));
	printf(", is %d, to fcntl %d", option(s[1], SO_NOSIGPIPE), fcntl(s[1], F_GETNOSIGPIPE));
	close(s[0]);
	printf("; write: %s\n", said(write(s[1], "x", 1)));
	close(s[1]);
	pair(s);
	printf("asked of fcntl: %s", said(fcntl(s[1], F_SETNOSIGPIPE, 1)));
	printf(", is %d, to fcntl %d", option(s[1], SO_NOSIGPIPE), fcntl(s[1], F_GETNOSIGPIPE));
	close(s[0]);
	printf("; write: %s\n", said(write(s[1], "x", 1)));
	close(s[1]);
	pair(s);
	close(s[0]);
	printf("no signal for this send: %s\n", said(send(s[1], "x", 1, MSG_NOSIGNAL)));
	close(s[1]);
	signal(SIGPIPE, SIG_IGN);

	// Each is told when the other goes, or stops, while it waits.
	pair(s);
	struct job end = {.fd = s[0], .after = 20000};
	pthread_create(&one, NULL, closer, &end);
	printf("a reader whose sender goes while it waits: %s\n", said(read(s[1], buf, 16)));
	pthread_join(one, NULL);
	close(s[1]);
	pair(s);
	end = (struct job){.fd = s[1], .after = 20000};
	pthread_create(&one, NULL, closer, &end);
	errno = 0;
	printf("a writer of 100000 whose receiver goes while it waits: %s\n", said(write(s[0], big, sizeof big)));
	pthread_join(one, NULL);
	close(s[0]);
	pair(s);
	end = (struct job){.fd = s[0], .size = SHUT_WR, .after = 20000};
	pthread_create(&one, NULL, shutter, &end);
	printf("a reader whose sender stops while it waits: %s\n", said(read(s[1], buf, 16)));
	pthread_join(one, NULL);
	close(s[0]);
	close(s[1]);
	pair(s);
	w = (struct job){.fd = s[0], .size = 5, .after = 20000};
	end = (struct job){.fd = s[0], .after = 40000};
	pthread_create(&two, NULL, writer, &w);
	pthread_create(&one, NULL, closer, &end);
	printf("a recv of all of 8, whose sender goes after 5: %s\n", said(recv(s[1], buf, 8, MSG_WAITALL)));
	pthread_join(two, NULL);
	pthread_join(one, NULL);
	close(s[1]);

	// A second descriptor keeps a socket open, and a copy of the process has it too.
	pair(s);
	int again = dup(s[0]);
	close(s[0]);
	printf("one, twice: closing the first, can the other be read: %d; write through the second: %s", ready(s[1], 0), said(write(again, "ab", 2)));
	close(again);
	printf("; closing it too: %d\n", ready(s[1], 0));
	close(s[1]);
	pair(s);
	pid_t child = fork();
	if (child == 0) {
		close(s[0]);
		n = read(s[1], buf, 16);
		write(s[1], buf, n);
		write(s[1], " and back", 9);
		_exit(0);
	}
	close(s[1]);
	write(s[0], "there", 5);
	total = 0;
	while ((n = read(s[0], buf + total, sizeof buf - total)) > 0)
		total += n;
	printf("with a copy of the process at the other: \"%.*s\", then %s\n", (int)total, buf, said(n));
	close(s[0]);

	// What there is no pair of.
	printf("a pair of datagram sockets is another matter, and one of the Internet's there is not: %s\n", said(socketpair(AF_INET, SOCK_STREAM, 0, s)));
	return 0;
}
