// Pipes: what goes in one end comes out of the other, a reader waits for a writer and a writer
// for room, and each is told when the other end has gone. It prints the same natively on a Mac as
// under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/select.h>
#include <sys/stat.h>
#include <unistd.h>

static char big[200000];

// said is what a call answered: its result, and the error if it failed.
static const char *said(ssize_t r) {
	static char text[4][64];
	static int next;
	char *t = text[next++ % 4];
	if (r < 0)
		snprintf(t, 64, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 64, "%zd", r);
	return t;
}

static void status(const char *what, int fd) {
	struct stat s;
	int waiting = -1;
	fstat(fd, &s);
	ioctl(fd, FIONREAD, &waiting);
	printf("%s: mode %o, %d links, size %lld, %lld blocks of %d, device %d, mine %d, born %ld, flags %#x, %d to read\n", what, s.st_mode, s.st_nlink, s.st_size,
	       s.st_blocks, s.st_blksize, s.st_dev, s.st_uid == getuid() && s.st_gid == getgid(), s.st_birthtime, fcntl(fd, F_GETFL), waiting);
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

struct job {
	int fd;
	size_t size;
	useconds_t after;
	ssize_t result;
	int error;
	char data[16];
};

static void *reader(void *arg) {
	struct job *j = arg;
	j->result = read(j->fd, j->data, j->size);
	j->error = errno;
	return NULL;
}

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

int main(void) {
	int p[2], q[2];
	char buf[64] = "";
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGPIPE, SIG_IGN);
	printf("pipe: %s, its ends one after the other: %d\n", said(pipe(p)), p[1] == p[0] + 1);
	status("the reading end", p[0]);
	status("the writing end", p[1]);
	printf("write 5: %s\n", said(write(p[1], "hello", 5)));
	status("the reading end, with 5 in it", p[0]);
	status("the writing end, written to", p[1]);
	ssize_t n = read(p[0], buf, 3);
	printf("read 3: %s \"%.3s\"", said(n), buf);
	n = read(p[0], buf, 10);
	printf("; read 10: %s \"%.2s\"", said(n), buf);
	printf("; read none of nothing: %s\n", said(read(p[0], buf, 0)));
	printf("the wrong way round: the writing end can be read: %d; the reading end can be written: %d\n", ready(p[1], 0), ready(p[0], 1));
	printf("lseek: %s; pread: %s; pwrite: %s\n", said(lseek(p[0], 0, SEEK_SET)), said(pread(p[0], buf, 1, 0)), said(pwrite(p[1], buf, 1, 0)));
	printf("reading the writing end: %s; writing the reading end: %s\n", said(read(p[1], buf, 1)), said(write(p[0], buf, 1)));

	// Without waiting: nothing to read is an error, and a write takes what there is room for,
	// or nothing if it is small enough to be kept whole.
	printf("no waiting on the reading end: %s", said(fcntl(p[0], F_SETFL, O_NONBLOCK)));
	printf(", its flags %#x; read: %s\n", fcntl(p[0], F_GETFL), said(read(p[0], buf, 10)));
	int on = 1, off = 0;
	printf("no waiting on the writing end, by ioctl: %s", said(ioctl(p[1], FIONBIO, &on)));
	printf(", its flags %#x\n", fcntl(p[1], F_GETFL));
	printf("write 100000: %s", said(write(p[1], big, 100000)));
	printf("; one more: %s; can be written: %d, read: %d\n", said(write(p[1], big, 1)), ready(p[1], 1), ready(p[0], 0));
	char *sink = malloc(sizeof big);
	printf("read 100: %s", said(read(p[0], sink, 100)));
	printf("; can be written: %d; write 512: %s", ready(p[1], 1), said(write(p[1], big, 512)));
	printf("; write 600: %s\n", said(write(p[1], big, 600)));
	printf("read 600: %s", said(read(p[0], sink, 600)));
	printf("; can be written: %d; write 512: %s\n", ready(p[1], 1), said(write(p[1], big, 512)));
	printf("read the rest: %s", said(read(p[0], sink, sizeof big)));
	printf("; can be read: %d, written: %d\n", ready(p[0], 0), ready(p[1], 1));
	printf("waiting again: %s and %s", said(fcntl(p[0], F_SETFL, 0)), said(ioctl(p[1], FIONBIO, &off)));
	printf(", flags %#x and %#x\n", fcntl(p[0], F_GETFL), fcntl(p[1], F_GETFL));

	// A second descriptor for the writing end keeps it open.
	int again = dup(p[1]);
	printf("the writing end twice: closing one %s", said(close(p[1])));
	printf(", write through the other: %s", said(write(again, "ab", 2)));
	printf("; closing it: %s; can be read: %d\n", said(close(again)), ready(p[0], 0));
	printf("read what is left: %s", said(read(p[0], buf, 10)));
	printf(", then the end: %s, and again: %s; can be read: %d\n", said(read(p[0], buf, 10)), said(read(p[0], buf, 10)), ready(p[0], 0));
	status("the reading end, its writer gone", p[0]);
	close(p[0]);

	// Nobody to read: an error, as the signal is ignored. A descriptor can ask for no signal.
	pipe(p);
	close(p[0]);
	printf("write with no reader: %s; can be written: %d, and read: %d\n", said(write(p[1], "x", 1)), ready(p[1], 1), ready(p[1], 0));
	close(p[1]);
	signal(SIGPIPE, SIG_DFL);
	pipe(p);
	printf("no signal for this descriptor: was %d, set %s", fcntl(p[1], F_GETNOSIGPIPE), said(fcntl(p[1], F_SETNOSIGPIPE, 1)));
	printf(", is %d\n", fcntl(p[1], F_GETNOSIGPIPE));
	close(p[0]);
	printf("write with no reader, and no signal: %s\n", said(write(p[1], "x", 1)));
	close(p[1]);
	signal(SIGPIPE, SIG_IGN);

	// A reader waits for a writer, and a writer for room.
	pthread_t one, two;
	pipe(p);
	struct job r = {.fd = p[0], .size = 16}, w = {.fd = p[1], .size = 5, .after = 20000};
	pthread_create(&one, NULL, reader, &r);
	pthread_create(&two, NULL, writer, &w);
	pthread_join(one, NULL);
	pthread_join(two, NULL);
	printf("a reader that waited: %s; its writer: %s\n", said(r.result), said(w.result));
	struct job all = {.fd = p[1], .size = sizeof big};
	pthread_create(&one, NULL, writer, &all);
	usleep(20000);
	size_t total = 0;
	int waiting = 0;
	ioctl(p[0], FIONREAD, &waiting);
	printf("a writer of %zu waits with %d in the pipe; can be written: %d\n", sizeof big, waiting, ready(p[1], 1));
	while (total < sizeof big) {
		n = read(p[0], sink, 30000);
		if (n <= 0)
			break;
		total += n;
	}
	pthread_join(one, NULL);
	printf("read in all: %zu; the writer: %s\n", total, said(all.result));

	// Each is told when the other end goes while it waits.
	struct job end = {.fd = p[1], .after = 20000};
	r = (struct job){.fd = p[0], .size = 16};
	pthread_create(&one, NULL, closer, &end);
	printf("a reader whose writer goes while it waits: %s\n", said(read(p[0], buf, 16)));
	pthread_join(one, NULL);
	close(p[0]);
	pipe(q);
	end = (struct job){.fd = q[0], .after = 20000};
	pthread_create(&one, NULL, closer, &end);
	errno = 0;
	printf("a writer of %zu whose reader goes while it waits: %s\n", sizeof big, said(write(q[1], big, sizeof big)));
	pthread_join(one, NULL);
	close(q[1]);

	// select waits for either, as long as it is told to.
	pipe(p);
	fd_set set;
	FD_ZERO(&set);
	FD_SET(p[0], &set);
	struct timeval soon = {0, 20000};
	printf("select on an empty pipe, for a while: %s\n", said(select(p[0] + 1, &set, NULL, NULL, &soon)));
	w = (struct job){.fd = p[1], .size = 3, .after = 20000};
	pthread_create(&two, NULL, writer, &w);
	FD_ZERO(&set);
	FD_SET(p[0], &set);
	printf("select with a writer on its way: %s", said(select(p[0] + 1, &set, NULL, NULL, NULL)));
	printf(", the pipe is in the set: %d\n", FD_ISSET(p[0], &set) != 0);
	pthread_join(two, NULL);
	fd_set both;
	FD_ZERO(&set);
	FD_ZERO(&both);
	FD_SET(p[0], &set);
	FD_SET(p[0], &both);
	FD_SET(p[1], &both);
	printf("select for reading and writing both ends: %s", said(select(p[1] + 1, &set, &both, NULL, NULL)));
	printf("; to read: %d; to write: the reading end %d, the writing end %d\n", FD_ISSET(p[0], &set) != 0, FD_ISSET(p[0], &both) != 0, FD_ISSET(p[1], &both) != 0);
	return 0;
}
