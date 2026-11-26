// Locks on parts of a file that are an open file's (fcntl's F_OFD_SETLK), as SQLite takes them
// on every database: held by the open file, so by its duplicates and a copy of the process too,
// and refused to another open of the file where they conflict. And what else SQLite and dyld
// ask of each file they open: how it is protected, whether it may be kept from being moved,
// and whether a library is signed. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

// The requests no SDK names.
enum { getProtection = 63, setLock = 90, getLock = 92, setConfined = 95, getConfined = 96, checkLibrary = 98, readAhead = 101 };

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

// lock asks for a lock of a kind on some bytes of a file from its start: 0 bytes is to its end.
static const char *lock(int fd, short kind, off_t start, off_t length) {
	struct flock f = {.l_type = kind, .l_whence = SEEK_SET, .l_start = start, .l_len = length};
	return said(fcntl(fd, setLock, &f));
}

// held says what stands against a lock to write some bytes.
static const char *held(int fd, off_t start, off_t length) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	struct flock f = {.l_type = F_WRLCK, .l_whence = SEEK_SET, .l_start = start, .l_len = length};
	if (fcntl(fd, getLock, &f))
		return strerror(errno);
	if (f.l_type == F_UNLCK)
		snprintf(t, 96, "nothing (and the rest as asked %d)", f.l_start == start && f.l_len == length && f.l_pid == 0);
	else
		snprintf(t, 96, "a lock to %s of %lld bytes from %lld, of process %d", f.l_type == F_WRLCK ? "write" : "read", f.l_len, f.l_start, f.l_pid);
	return t;
}

// fat says whether a file holds several programs, and where in it the one for this machine is.
static int fat(int fd, off_t *at) {
	unsigned char head[8], arch[20];
	*at = 0;
	if (pread(fd, head, 8, 0) != 8 || head[0] != 0xca || head[1] != 0xfe || head[2] != 0xba || head[3] != 0xbe)
		return 0;
	for (unsigned i = 0; i < head[7]; i++) // each names a kind of processor and where its program is, the larger end first
		if (pread(fd, arch, 20, 8 + 20 * i) == 20 && arch[0] == 1 && arch[3] == 12 && !*at)
			*at = (off_t)arch[8] << 24 | arch[9] << 16 | arch[10] << 8 | arch[11];
	return 1;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char path[512];
	snprintf(path, sizeof path, "%s/rangelocks.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	int a = open(path, O_CREAT | O_RDWR, 0644), b = open(path, O_RDWR), reads = open(path, O_RDONLY), writes = open(path, O_WRONLY);
	write(a, "0123456789", 10);

	printf("one open file locks bytes 0 to 4 to write: %s; another open of the file, to write them: %s; to read two of them: %s; to write the next five: %s\n", lock(a, F_WRLCK, 0, 5), lock(b, F_WRLCK, 0, 5),
	       lock(b, F_RDLCK, 2, 2), lock(b, F_WRLCK, 5, 5));
	printf("what stands against the second writing all ten: %s; against the first: %s; against the first writing its own five: %s\n", held(b, 0, 10), held(a, 0, 10), held(a, 0, 5));
	int same = dup(a);
	printf("a duplicate of the first locks the same again: %s; unlocks the first two: %s; the second then writes those: %s, and the next three: %s\n", lock(same, F_WRLCK, 0, 5), lock(same, F_UNLCK, 0, 2),
	       lock(b, F_WRLCK, 0, 2), lock(b, F_WRLCK, 2, 3));
	printf("the first makes its three a lock to read: %s; the second reads them: %s, writes them: %s\n", lock(a, F_RDLCK, 2, 3), lock(b, F_RDLCK, 2, 3), lock(b, F_WRLCK, 2, 3));
	printf("one opened only to read, to write: %s, to read: %s; one opened only to write, to read: %s, to write: %s\n", lock(reads, F_WRLCK, 20, 2), lock(reads, F_RDLCK, 20, 2), lock(writes, F_RDLCK, 30, 2),
	       lock(writes, F_WRLCK, 30, 2));

	// A copy of the process holds what the open file holds, and its own opens do not.
	pid_t pid = fork();
	if (pid == 0) {
		int own = open(path, O_RDWR);
		printf("in a copy of the process: an open of its own writes the three: %s; the first, which it has too, unlocks them: %s", lock(own, F_WRLCK, 2, 3), lock(a, F_UNLCK, 2, 3));
		printf(", and its own open then reads them: %s\n", lock(own, F_RDLCK, 2, 3));
		_exit(0);
	}
	waitpid(pid, NULL, 0);
	printf("the first's three, unlocked by the copy: against the second writing them stands %s\n", held(b, 2, 3));

	// They go with the last descriptor of the open file.
	lock(a, F_WRLCK, 40, 5);
	close(same);
	printf("the first locks five more, and its duplicate is closed: the second writes them: %s", lock(b, F_WRLCK, 40, 5));
	close(a);
	printf("; the first is closed too: %s\n", lock(b, F_WRLCK, 40, 5));
	struct flock fromEnd = {.l_type = F_WRLCK, .l_whence = SEEK_END, .l_start = 0, .l_len = 1};
	printf("to the end of the file, from byte 100: %s; from before its start: %s; of no kind: %s; unlocking what is not locked: %s; counted from its end: %s\n", lock(b, F_WRLCK, 100, 0), lock(b, F_WRLCK, -1, 1),
	       lock(b, 7, 0, 1), lock(b, F_UNLCK, 500, 10), said(fcntl(b, setLock, &fromEnd)));
	int ends[2];
	pipe(ends);
	printf("of a pipe: %s\n", lock(ends[0], F_RDLCK, 0, 1));

	// What else is asked of each file.
	int dir = open("/", O_RDONLY);
	printf("how it is protected: a file %s, a directory %s, a pipe %s\n", said(fcntl(b, getProtection)), said(fcntl(dir, getProtection)), said(fcntl(ends[0], getProtection)));
	printf("kept from being moved: a file %s, a directory %s, a pipe %s; let be moved again: a file %s; whether it is kept: %s\n", said(fcntl(b, setConfined, 1)), said(fcntl(dir, setConfined, 1)),
	       said(fcntl(ends[0], setConfined, 1)), said(fcntl(b, setConfined, 0)), said(fcntl(b, getConfined)));
	char why[128] = "";
	int program = open("/usr/lib/dyld", O_RDONLY); // which is a file, and signed, on a Mac and an iPhone alike
	struct {
		off_t start;
		size_t room;
		char *message;
	} check = {0, sizeof why, why};
	// dyld's file holds several programs on a Mac and one on an iPhone: it is asked of from where
	// the one for this machine starts, and on a Mac from the start of the file too.
	int whole = fcntl(program, checkLibrary, &check), several = fat(program, &check.start);
	printf("whether a library is signed: dyld, from where its program for this machine starts: %s; from the start of a file that holds several: %s", said(fcntl(program, checkLibrary, &check)),
	       several ? (whole ? "refused" : "0") : "refused"); // an iPhone's holds one, and is not asked
	check.start = 0;
	why[0] = 0;
	printf("; a file that is none %s, \"%s\"\n", said(fcntl(b, checkLibrary, &check)), why);
	struct {
		unsigned flags, kept;
		off_t start, length;
	} soon = {0, 0, 0, 4096}, before = {0, 0, -1, 4096}, less = {0, 0, 0, -5};
	printf("told what will be read soon: of a file %s, from before its start %s, of less than nothing %s, of a directory %s, of a pipe %s\n", said(fcntl(b, readAhead, &soon)), said(fcntl(b, readAhead, &before)),
	       said(fcntl(b, readAhead, &less)), said(fcntl(dir, readAhead, &soon)), said(fcntl(ends[0], readAhead, &soon)));
	close(b);
	printf("tidied: %s\n", said(unlink(path)));
	return 0;
}
