// What the kernel keeps of an open file besides what it was opened to do, as fcntl reads it
// (F_GETFL) and sets it (F_SETFL): whether writes go to the end, whether it waits, whether it
// has been written. It is the open file's, so every descriptor for it sees it: a duplicate, and
// a copy of the process. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
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

	// What is asked of how it is read and written out, which the shell asks of each script it reads.
	int ends[2], dir = open("/", O_RDONLY), reads = open(path, O_RDONLY);
	pipe(ends);
	fd = open(path, O_RDWR);
	printf("not to be kept in memory: %d", fcntl(fd, F_NOCACHE, 1));
	printf(", and it is then %#x", fcntl(fd, F_GETFL));
	fcntl(fd, F_SETFL, 0);
	printf("; with nothing set after: %#x; to be kept again: %d", fcntl(fd, F_GETFL), fcntl(fd, F_NOCACHE, 0));
	printf(", and it is then %#x; of a directory: %d; of a pipe: %d (%s)\n", fcntl(fd, F_GETFL), fcntl(dir, F_NOCACHE, 1), fcntl(ends[0], F_NOCACHE, 1), strerror(errno));
	printf("not to be read ahead: %d", fcntl(fd, F_RDAHEAD, 0));
	printf(", and it is then %#x; to be read ahead again: %d", fcntl(fd, F_GETFL), fcntl(fd, F_RDAHEAD, 1));
	printf(", and it is then %#x; of a directory: %d; of a pipe: %d (%s)\n", fcntl(fd, F_GETFL), fcntl(dir, F_RDAHEAD, 0), fcntl(ends[0], F_RDAHEAD, 0), strerror(errno));
	printf("written out to the disk: %d, in order: %d; one opened only to read: %d and %d; a directory: %d and %d", fcntl(fd, F_FULLFSYNC), fcntl(fd, F_BARRIERFSYNC), fcntl(reads, F_FULLFSYNC),
	       fcntl(reads, F_BARRIERFSYNC), fcntl(dir, F_FULLFSYNC), fcntl(dir, F_BARRIERFSYNC));
	printf("; a pipe: %d (%s)", fcntl(ends[0], F_FULLFSYNC), strerror(errno));
	printf(" and %d (%s)\n", fcntl(ends[0], F_BARRIERFSYNC), strerror(errno));
	// Told what part of it will be read, as configd tells of a file it reads.
	struct radvisory parts[] = {{0, 4}, {9, 400}, {10, 4}, {-1, 4}, {0, -4}, {0, 0}};
	const char *named[] = {"its start", "its last byte and more", "from its end", "from before its start", "less than nothing", "nothing"};
	int told = open(path, O_RDWR);
	ftruncate(told, 0);
	write(told, "0123456789", 10);
	printf("told what will be read:");
	for (unsigned i = 0; i < sizeof parts / sizeof *parts; i++) {
		int r = fcntl(told, F_RDADVISE, &parts[i]);
		printf(" %s %d%s%s%s;", named[i], r, r ? " (" : "", r ? strerror(errno) : "", r ? ")" : "");
	}
	printf(" one opened only to read %d;", fcntl(reads, F_RDADVISE, &parts[0]));
	printf(" a directory %d (%s);", fcntl(dir, F_RDADVISE, &parts[0]), strerror(errno));
	printf(" a pipe %d (%s)\n", fcntl(ends[0], F_RDADVISE, &parts[0]), strerror(errno));
	close(told);
	// Room asked for ahead of writing, as cp asks.
	fstore_t room = {F_ALLOCATECONTIG | F_ALLOCATEALL, F_PEOFPOSMODE, 0, 5000, -7}, less = {F_ALLOCATEALL, F_PEOFPOSMODE, 0, -1, -7}, within = {F_ALLOCATEALL, F_PEOFPOSMODE, 5, 5000, -7},
	         elsewhere = {F_ALLOCATEALL, F_VOLPOSMODE, 0, 5000, -7}, again = {F_ALLOCATEALL, F_PEOFPOSMODE, 0, 0, -7};
	struct stat before, after;
	fstat(fd, &before);
	printf("room for 5000 bytes past its end: %d, had %lld", fcntl(fd, F_PREALLOCATE, &room), room.fst_bytesalloc);
	fstat(fd, &after);
	printf(", and it is as long as it was %d; for none: %d, had %lld", after.st_size == before.st_size, fcntl(fd, F_PREALLOCATE, &again), again.fst_bytesalloc);
	printf("; for less than none: %d (%s), had %lld", fcntl(fd, F_PREALLOCATE, &less), strerror(errno), less.fst_bytesalloc);
	printf("; from a place in it: %d (%s), had %lld", fcntl(fd, F_PREALLOCATE, &within), strerror(errno), within.fst_bytesalloc);
	printf("; from a place on the disk: %d (%s)\n", fcntl(fd, F_PREALLOCATE, &elsewhere), strerror(errno));
	printf("of one opened only to read: %d (%s)", fcntl(reads, F_PREALLOCATE, &room), strerror(errno));
	printf("; of a directory: %d (%s)", fcntl(dir, F_PREALLOCATE, &room), strerror(errno));
	printf("; of a pipe: %d (%s)\n", fcntl(ends[1], F_PREALLOCATE, &room), strerror(errno));
	close(fd);
	close(reads);
	close(dir);

	// A directory.
	dir = open("/", O_RDONLY);
	printf("a directory: %#x; appending set: %d", fcntl(dir, F_GETFL), fcntl(dir, F_SETFL, O_APPEND));
	printf(", and it is then %#x\n", fcntl(dir, F_GETFL));
	close(dir);
	printf("tidied: %d\n", unlink(path));
	return 0;
}
