// poll: which of some descriptors can be read or written, and waiting for one to be. The ends of
// a pipe, a file, a descriptor that is not open, and select asked the same of a file. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/select.h>
#include <sys/time.h>
#include <unistd.h>

static char big[70000];

// ask polls one descriptor for some events, without waiting, and says what came back.
static const char *ask(int fd, short events) {
	static char text[4][48];
	static int next;
	char *t = text[next++ % 4];
	struct pollfd p = {.fd = fd, .events = events, .revents = 0x7777};
	int n = poll(&p, 1, 0);
	if (n < 0)
		snprintf(t, 48, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 48, "%d, %#x", n, p.revents & 0xFFFF);
	return t;
}

static double now(void) {
	struct timeval t;
	gettimeofday(&t, NULL);
	return t.tv_sec + t.tv_usec / 1e6;
}

static void *write_later(void *arg) {
	usleep(30000);
	write(*(int *)arg, "late", 4);
	return NULL;
}

static void *signal_later(void *arg) {
	usleep(30000);
	pthread_kill(*(pthread_t *)arg, SIGUSR1);
	return NULL;
}

static void quiet(int sig) {}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGPIPE, SIG_IGN);
	int p[2];
	pipe(p);
	const short all = POLLIN | POLLOUT | POLLPRI | POLLRDNORM | POLLWRNORM | POLLRDBAND | POLLWRBAND;
	printf("an empty pipe: to read %s; to write %s\n", ask(p[0], POLLIN), ask(p[1], POLLOUT));
	printf("asked everything: the reading end %s; the writing end %s\n", ask(p[0], all), ask(p[1], all));
	printf("asked nothing: the reading end %s; the writing end %s\n", ask(p[0], 0), ask(p[1], 0));
	write(p[1], "hello", 5);
	printf("with 5 in it: to read %s; asked everything %s; the wrong way round: %s and %s\n", ask(p[0], POLLIN), ask(p[0], all), ask(p[0], POLLOUT), ask(p[1], POLLIN));
	fcntl(p[1], F_SETFL, O_NONBLOCK);
	write(p[1], big, sizeof big);
	printf("full: to write %s; asked everything %s\n", ask(p[1], POLLOUT), ask(p[1], all));
	char *sink = malloc(sizeof big);
	read(p[0], sink, 600);
	printf("with room for 600 more: to write %s\n", ask(p[1], POLLOUT));
	read(p[0], sink, sizeof big);
	close(p[1]);
	printf("the writer gone, and nothing left: to read %s; asked nothing %s\n", ask(p[0], POLLIN), ask(p[0], 0));
	close(p[0]);
	pipe(p);
	write(p[1], "abc", 3);
	close(p[1]);
	printf("the writer gone, with 3 left: to read %s\n", ask(p[0], POLLIN));
	close(p[0]);
	pipe(p);
	close(p[0]);
	printf("the reader gone: to write %s; asked nothing %s\n", ask(p[1], POLLOUT), ask(p[1], 0));
	close(p[1]);
	printf("a descriptor that is not open: %s; a negative one: %s\n", ask(p[1], POLLIN), ask(-1, POLLIN));

	// A file can always be read and written, to poll and to select.
	char path[1024];
	snprintf(path, sizeof path, "%s/poll.tmp", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp");
	int file = open(path, O_RDWR | O_CREAT | O_TRUNC, 0600);
	printf("an empty file: to read %s; to write %s; asked everything %s\n", ask(file, POLLIN), ask(file, POLLOUT), ask(file, all));
	fd_set readable, writable;
	FD_ZERO(&readable);
	FD_ZERO(&writable);
	FD_SET(file, &readable);
	FD_SET(file, &writable);
	struct timeval none = {0, 0};
	int n = select(file + 1, &readable, &writable, NULL, &none);
	printf("select of it: %d, to read %d, to write %d\n", n, FD_ISSET(file, &readable) != 0, FD_ISSET(file, &writable) != 0);
	int dir = open("/", O_RDONLY);
	printf("a directory: to read %s; asked everything %s\n", ask(dir, POLLIN), ask(dir, all));
	close(dir);
	close(file);
	unlink(path);

	// Several at once, and waiting.
	int q[2];
	pipe(p);
	pipe(q);
	write(q[1], "x", 1);
	struct pollfd several[4] = {{p[0], POLLIN, 0}, {q[0], POLLIN, 0}, {-1, POLLIN, 0x7777}, {p[1], POLLOUT, 0}};
	n = poll(several, 4, 0);
	printf("four at once: %d; %#x %#x %#x %#x\n", n, several[0].revents, several[1].revents, several[2].revents & 0xFFFF, several[3].revents);
	struct pollfd one = {p[0], POLLIN, 0};
	double began = now();
	n = poll(&one, 1, 30);
	printf("waiting 30 ms for an empty pipe: %d, %#x; it took at least that: %d\n", n, one.revents, now() - began >= 0.029);
	began = now();
	n = poll(NULL, 0, 30);
	printf("waiting 30 ms for nothing: %d; it took at least that: %d\n", n, now() - began >= 0.029);
	pthread_t thread;
	pthread_create(&thread, NULL, write_later, &p[1]);
	n = poll(&one, 1, -1);
	printf("waiting for a writer on its way: %d, %#x\n", n, one.revents);
	pthread_join(thread, NULL);
	read(p[0], sink, 16);
	struct sigaction interrupt = {.sa_handler = quiet};
	sigaction(SIGUSR1, &interrupt, NULL);
	pthread_t me = pthread_self();
	pthread_create(&thread, NULL, signal_later, &me);
	n = poll(&one, 1, 500);
	printf("waiting, and a signal comes: %d (%s)\n", n, n < 0 ? strerror(errno) : "");
	pthread_join(thread, NULL);
	return 0;
}
