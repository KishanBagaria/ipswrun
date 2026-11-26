// The descriptors a process has, as files: /dev/fd/3 is what descriptor 3 is, and /dev/stdin,
// /dev/stdout and /dev/stderr are 0, 1 and 2. Opening one is duplicating the descriptor, as a
// tool told to write to /dev/stdout does. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
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

static const char *named(int fd) {
	static char text[4][32];
	static int next;
	char *t = text[next++ % 4];
	snprintf(t, 32, "/dev/fd/%d", fd);
	return t;
}

static const char *is(const char *path, int follow) {
	static char text[4][64];
	static int next;
	char *t = text[next++ % 4];
	struct stat s;
	if (follow ? stat(path, &s) : lstat(path, &s))
		return strerror(errno);
	snprintf(t, 64, "%s of %lld bytes", S_ISREG(s.st_mode) ? "a file" : S_ISFIFO(s.st_mode) ? "a pipe" : S_ISDIR(s.st_mode) ? "a directory" : S_ISCHR(s.st_mode) ? "a device" : "something else",
	         s.st_size);
	return t;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char path[512], buf[16] = "";
	snprintf(path, sizeof path, "%s/devfd.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	int file = open(path, O_CREAT | O_RDWR, 0644);
	write(file, "abcdef", 6);
	lseek(file, 2, SEEK_SET);

	// Opening it is another descriptor for the same open file: where one reads on, so does the other.
	int again = open(named(file), O_RDONLY);
	long n = read(again, buf, 2);
	printf("a file's descriptor opened by its name: a descriptor of its own %d; it reads \"%.*s\" from where the first was, and the first is then at %s\n", again > file, n > 0 ? (int)n : 0, buf,
	       said(lseek(file, 0, SEEK_CUR)));
	printf("it is %s; itself, not followed, %s; there to be asked of: %s\n", is(named(file), 1), is(named(file), 0), said(access(named(file), F_OK)));
	int kept = open(named(file), O_RDONLY | O_CLOEXEC);
	printf("opened to be closed in a new program: marked so %d, and the first is not %d\n", fcntl(kept, F_GETFD) & FD_CLOEXEC, fcntl(file, F_GETFD) & FD_CLOEXEC);
	close(kept);
	close(again);

	// What the descriptor may do is all its name may be opened for.
	int reads = open(path, O_RDONLY);
	printf("one that only reads, opened to write: %s; to read and write: %s; to read: %d\n", said(open(named(reads), O_WRONLY)), said(open(named(reads), O_RDWR)),
	       (again = open(named(reads), O_RDONLY)) > reads);
	close(again);
	printf("what each says it was opened for (fcntl): the first %d, the one that only reads %d\n", fcntl(file, F_GETFL) & O_ACCMODE, fcntl(reads, F_GETFL) & O_ACCMODE);
	printf("opened to be emptied: %d", (again = open(named(file), O_RDWR | O_TRUNC)) > 0);
	printf("; the file is then %s\n", is(path, 1));
	close(again);
	close(reads);
	printf("of a descriptor that is not open: %s; asked of: %s; of no number: %s; the directory: %s\n", said(open("/dev/fd/99", O_RDONLY)), said(access("/dev/fd/99", F_OK)),
	       said(open("/dev/fd/x", O_RDONLY)), is("/dev/fd", 1));

	// A pipe's end, by its name.
	int ends[2];
	pipe(ends);
	int into = open(named(ends[1]), O_WRONLY);
	write(into, "piped", 5);
	n = read(ends[0], buf, sizeof buf);
	printf("a pipe's end for writing, opened by its name and written to: the other end reads \"%.*s\"; the name is %s; opened to read: %s\n", n > 0 ? (int)n : 0, buf, is(named(ends[1]), 1),
	       said(open(named(ends[1]), O_RDONLY)));
	close(into);
	close(ends[0]);
	close(ends[1]);

	// The three every process starts with.
	int out = open("/dev/stdout", O_WRONLY), err = open("/dev/stderr", O_WRONLY), in = open("/dev/stdin", O_RDONLY);
	printf("/dev/stdout, /dev/stderr and /dev/stdin opened: %d %d %d\n", out > 2, err > 2, in > 2);
	write(out, "written to /dev/stdout\n", 23);
	close(out);
	close(err);
	close(in);
	close(file);
	printf("tidied: %s\n", said(unlink(path)));
	return 0;
}
