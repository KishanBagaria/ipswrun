// What the kernel keeps of an open file besides what it was opened to do, as fcntl reads it
// (F_GETFL) and sets it (F_SETFL): whether writes go to the end, whether it waits, whether it
// has been written. It is the open file's, so every descriptor for it sees it: a duplicate, and
// a copy of the process. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

static char path[512];

// opened is what fcntl says of the file opened so.
static int opened(int how) {
	int fd = open(path, how, 0644);
	if (fd < 0)
		return -errno;
	int flags = fcntl(fd, F_GETFL);
	close(fd);
	return flags;
}

static const char *holds(int fd) {
	static char text[4][32];
	static int next;
	char *t = text[next++ % 4], held[24] = "";
	ssize_t n = pread(fd, held, sizeof held - 1, 0);
	snprintf(t, 32, "\"%.*s\"", n > 0 ? (int)n : 0, held);
	return t;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	snprintf(path, sizeof path, "%s/fileflags.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	close(open(path, O_CREAT | O_WRONLY, 0644));

	printf("opened to read: %#x; to write: %#x; for both: %#x\n", opened(O_RDONLY), opened(O_WRONLY), opened(O_RDWR));
	printf("for both and to append: %#x; not to wait: %#x; to be made: %#x; to be emptied: %#x\n", opened(O_RDWR | O_APPEND), opened(O_RDWR | O_NONBLOCK), opened(O_RDWR | O_CREAT),
	       opened(O_RDWR | O_TRUNC));
	printf("to be closed in a new program: %#x; not following a link: %#x; and no other, of one that is there: %#x\n", opened(O_RDWR | O_CLOEXEC), opened(O_RDWR | O_NOFOLLOW), opened(O_RDWR | O_EXCL));
	printf("written through at once: %#x; its data at once: %#x; signalling when ready: %#x; only to be watched: %#x\n", opened(O_RDWR | O_FSYNC), opened(O_RDWR | O_DSYNC), opened(O_RDWR | O_ASYNC),
	       opened(O_RDONLY | O_EVTONLY));

	// What may be set afterwards, and what is left alone.
	int fd = open(path, O_RDWR);
	int set[] = {O_APPEND, O_NONBLOCK, O_APPEND | O_NONBLOCK, 0, O_WRONLY | O_APPEND, O_CREAT | O_TRUNC | O_EXCL, O_FSYNC, O_DSYNC, O_CLOEXEC | O_NOFOLLOW | O_EVTONLY, 0};
	printf("set in turn:");
	for (unsigned i = 0; i < sizeof set / sizeof *set; i++) {
		int r = fcntl(fd, F_SETFL, set[i]);
		printf(" %#x gives %d and %#x;", set[i], r, fcntl(fd, F_GETFL));
	}
	printf(" of a descriptor that is not open: %d (%s)\n", fcntl(99, F_SETFL, 0), strerror(errno));

	// It is the open file's: a duplicate's too, and not another open's.
	int same = dup(fd), other = open(path, O_RDWR);
	fcntl(same, F_SETFL, O_APPEND);
	printf("appending set by a duplicate: the first then %#x, another open of the file %#x\n", fcntl(fd, F_GETFL), fcntl(other, F_GETFL));
	write(other, "abcdef", 6);
	lseek(fd, 0, SEEK_SET);
	write(fd, "XY", 2);
	printf("written at the start by one that appends: the file is %s", holds(other));
	fcntl(fd, F_SETFL, 0);
	lseek(fd, 0, SEEK_SET);
	write(fd, "12", 2);
	printf("; and by one that no longer does: %s\n", holds(other));
	pid_t pid = fork();
	if (pid == 0) {
		fcntl(fd, F_SETFL, O_NONBLOCK);
		_exit(0);
	}
	waitpid(pid, NULL, 0);
	printf("not waiting set by a copy of the process: the first then %#x\n", fcntl(fd, F_GETFL) & ~0x10000);
	close(same);
	close(other);
	close(fd);

	// Whether it has been written.
	char byte;
	fd = open(path, O_RDWR);
	read(fd, &byte, 1);
	write(fd, "", 0);
	printf("read, and written nothing: %#x", fcntl(fd, F_GETFL));
	write(fd, "z", 1);
	same = dup(fd);
	printf("; written: %#x, a duplicate %#x", fcntl(fd, F_GETFL), fcntl(same, F_GETFL));
	fcntl(fd, F_SETFL, 0);
	printf("; with nothing set after: %#x\n", fcntl(fd, F_GETFL));
	close(same);
	close(fd);
	fd = open(path, O_RDWR);
	pwrite(fd, "q", 1, 0);
	printf("written at a place: %#x", fcntl(fd, F_GETFL));
	close(fd);
	fd = open(path, O_RDWR);
	ftruncate(fd, 1);
	printf("; cut short: %#x\n", fcntl(fd, F_GETFL));
	close(fd);

	// A directory.
	int dir = open("/", O_RDONLY);
	printf("a directory: %#x; appending set: %d", fcntl(dir, F_GETFL), fcntl(dir, F_SETFL, O_APPEND));
	printf(", and it is then %#x\n", fcntl(dir, F_GETFL));
	close(dir);
	printf("tidied: %d\n", unlink(path));
	return 0;
}
