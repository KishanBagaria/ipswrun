// What everyday tools ask that no other example does: a file's dates set from a directory (cp -p,
// touch), a named pipe made (mkfifo), the groups a process is in (ditto), a wait that also asks
// for children that were continued (zsh), and what a process's children used (time). It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/resource.h>
#include <sys/stat.h>
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

static long modified(const char *path, int follow) {
	struct stat s;
	return (follow ? stat(path, &s) : lstat(path, &s)) ? -1 : (long)s.st_mtimespec.tv_sec;
}

static long dated(int fd, const char *path, long when, unsigned long options) {
	struct attrlist list = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_MODTIME};
	struct timespec t = {when, 0};
	return setattrlistat(fd, path, &list, &t, sizeof t, options);
}

static long used(struct rusage *u) { return u->ru_utime.tv_sec * 1000000L + u->ru_utime.tv_usec + u->ru_stime.tv_sec * 1000000L + u->ru_stime.tv_usec; }

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char dir[512];
	snprintf(dir, sizeof dir, "%s/everyday.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	mkdir(dir, 0755);
	chdir(dir);
	close(open("file", O_CREAT | O_WRONLY, 0644));
	mkdir("inner", 0755);
	close(open("inner/file", O_CREAT | O_WRONLY, 0644));
	symlink("file", "soft");
	int here = open(".", O_RDONLY), inner = open("inner", O_RDONLY), file = open("file", O_RDONLY);

	// A file's dates, by a name in a directory.
	printf("setattrlistat here: %s, modified %ld", said(dated(AT_FDCWD, "file", 1000, 0)), modified("file", 1));
	printf("; in a directory: %s, modified %ld", said(dated(inner, "file", 2000, 0)), modified("inner/file", 1));
	printf("; by a whole path, the directory any: %s, modified %ld\n", said(dated(-7, strcat(strcpy(malloc(600), dir), "/file"), 3000, 0)), modified("file", 1));
	printf("through a link: %s, the file %ld", said(dated(here, "soft", 4000, 0)), modified("file", 1));
	printf("; of the link itself: %s, the file %ld, the link %ld\n", said(dated(here, "soft", 5000, FSOPT_NOFOLLOW)), modified("file", 1), modified("soft", 0));
	printf("of nothing: %s; in no directory: %s; in a file: %s; with no name: %s\n", said(dated(here, "none", 1, 0)), said(dated(99, "file", 1, 0)), said(dated(file, "file", 1, 0)),
	       said(dated(here, "", 1, 0)));
	printf("kept beneath the directory (0x1000), a name in it: %s, modified %ld", said(dated(inner, "file", 6000, 0x1000)), modified("inner/file", 1));
	long above = dated(inner, "../file", 1, 0x1000);
	printf("; a name above it: %ld, error %d", above, errno); // ENOTCAPABLE, which not every release's library has a name for
	long whole = dated(inner, strcat(strcpy(malloc(600), dir), "/file"), 1, 0x1000);
	printf("; a whole path: %ld, error %d\n", whole, errno);

	// A named pipe.
	umask(022);
	struct stat s;
	printf("mkfifo: %s", said(mkfifo("pipe", 0666)));
	lstat("pipe", &s);
	printf("; it is %s, mode %o, %d links, %lld bytes", S_ISFIFO(s.st_mode) ? "a pipe" : "something else", s.st_mode & 07777, s.st_nlink, s.st_size);
	printf("; again: %s; over a file: %s; in no directory: %s; removed: %s\n", said(mkfifo("pipe", 0666)), said(mkfifo("file", 0666)), said(mkfifo("none/pipe", 0666)), said(unlink("pipe")));

	// The groups of a process: its own is among them, and is the first.
	gid_t groups[64];
	int n = getgroups(64, groups), mine = 0;
	for (int i = 0; i < n; i++)
		mine += groups[i] == getegid();
	printf("getgroups: some %d, the process's own among them %d and first %d; asked how many: the same %d; with room for fewer than none: %s\n", n > 0, mine, n > 0 && groups[0] == getegid(),
	       getgroups(0, NULL) == n, said(getgroups(-1, groups)));

	// What children used, which is nothing until they are waited for.
	struct rusage u;
	getrusage(RUSAGE_CHILDREN, &u);
	printf("children, before any: used %ld, most memory %ld\n", used(&u), (long)u.ru_maxrss);
	pid_t child = fork();
	if (child == 0) {
		volatile unsigned long x = 0;
		struct rusage own;
		do {
			for (int i = 0; i < 1000000; i++)
				x += i;
			getrusage(RUSAGE_SELF, &own);
		} while (used(&own) < 20000);
		_exit(3);
	}
	int status = 0;
	printf("a child at work, asked of without waiting and of those continued too (0x13): %s", said(wait4(-1, &status, WNOHANG | WUNTRACED | WCONTINUED, NULL)));
	getrusage(RUSAGE_CHILDREN, &u);
	printf("; children used %ld\n", used(&u));
	struct rusage its;
	pid_t got = wait4(child, &status, WUNTRACED | WCONTINUED, &its);
	printf("waited for: the child %d, exited with %d, having used 20 ms or more %d\n", got == child, WEXITSTATUS(status), used(&its) >= 20000);
	getrusage(RUSAGE_CHILDREN, &u);
	printf("children, after: used what it did %d, most memory its %d, faults its %d\n", used(&u) == used(&its), u.ru_maxrss == its.ru_maxrss && u.ru_maxrss > 0, u.ru_minflt == its.ru_minflt);
	if ((child = fork()) == 0)
		_exit(0);
	struct rusage second;
	wait4(child, &status, 0, &second);
	getrusage(RUSAGE_CHILDREN, &u);
	printf("after a second: used both %d, most memory the most of the two %d\n", used(&u) == used(&its) + used(&second),
	       u.ru_maxrss == (its.ru_maxrss > second.ru_maxrss ? its.ru_maxrss : second.ru_maxrss));
	printf("of neither (1): %s\n", said(getrusage(1, &u)));

	close(here);
	close(inner);
	close(file);
	unlink("soft");
	unlink("inner/file");
	rmdir("inner");
	unlink("file");
	chdir("/");
	printf("tidied: %s\n", said(rmdir(dir)));
	return 0;
}
