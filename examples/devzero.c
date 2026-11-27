// /dev/zero: nothing but zeroes to read, however much is asked, and a place where what is
// written goes nowhere; beside /dev/null, which has nothing to read. It prints the same
// natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/select.h>
#include <sys/stat.h>
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

static const char *mapped(void *p, int look) {
	static char text[4][64];
	static int next;
	char *t = text[next++ % 4];
	if (p == MAP_FAILED)
		snprintf(t, 64, "failed (%s)", strerror(errno));
	else if (look)
		snprintf(t, 64, "mapped, and reads %d", ((char *)p)[100]);
	else
		snprintf(t, 64, "mapped");
	return t;
}

static void of(const char *path) {
	int fd = open(path, O_RDWR);
	char buf[70000];
	memset(buf, 'x', sizeof buf);
	long got = read(fd, buf, sizeof buf), zeroes = 0;
	for (long i = 0; i < got; i++)
		zeroes += buf[i] == 0;
	const char *r1 = said(got), *r2 = said(write(fd, "written", 7)), *r3 = said(lseek(fd, 0, SEEK_CUR)), *r4 = said(lseek(fd, 1000, SEEK_SET)), *r5 = said(lseek(fd, 0, SEEK_END));
	printf("%s opened: %d; 70000 bytes read: %s, zeroes among them %ld; written: %s; where it is then: %s, moved to 1000: %s, to its end: %s\n", path, fd >= 0, r1, zeroes, r2, r3, r4, r5);
	memset(buf, 'x', 16);
	r1 = said(pread(fd, buf, 16, 1 << 20));
	r2 = said(pwrite(fd, "w", 1, 5));
	struct stat st, named;
	r3 = said(fstat(fd, &st));
	r4 = said(stat(path, &named));
	printf("read at a place: %s, a zero %d; written at one: %s; what it is: %s, a device of characters %d, of size %lld, the same by its name: %s %d, mode %o\n", r1, buf[0] == 0, r2, r3, S_ISCHR(st.st_mode), (long long)st.st_size, r4,
	       st.st_rdev == named.st_rdev && st.st_ino == named.st_ino, (unsigned)(st.st_mode & 07777));
	struct pollfd p = {.fd = fd, .events = POLLIN | POLLOUT};
	r1 = said(poll(&p, 1, 0));
	r2 = said(ftruncate(fd, 0));
	r3 = said(fsync(fd));
	r4 = said(isatty(fd));
	fd_set reading, writing;
	FD_ZERO(&reading);
	FD_ZERO(&writing);
	FD_SET(fd, &reading);
	FD_SET(fd, &writing);
	struct timeval none = {0, 0};
	r5 = said(select(fd + 1, &reading, &writing, NULL, &none));
	printf("poll: %s, events %#x; select: %s, to read %d, to write %d; cut to nothing: %s; fsync: %s; a terminal: %s; may be read %s, written %s\n", r1, (unsigned)p.revents, r5, FD_ISSET(fd, &reading) != 0, FD_ISSET(fd, &writing) != 0, r2, r3, r4,
	       said(access(path, R_OK)), said(access(path, W_OK)));
	r1 = mapped(mmap(NULL, 16384, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0), 1);
	r2 = mapped(mmap(NULL, 16384, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0), 1);
	printf("mapped for oneself: %s; to be shared: %s; opened to be emptied: %d, to be made anew alone: %s\n", r1, r2, open(path, O_WRONLY | O_TRUNC) >= 0, said(open(path, O_WRONLY | O_CREAT | O_EXCL, 0600)));
	close(fd);
}

int main(void) {
	of("/dev/zero");
	of("/dev/null");
	return 0;
}
