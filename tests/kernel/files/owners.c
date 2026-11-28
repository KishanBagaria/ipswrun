// Whose a file is, and what may be done with it by a path from a directory: a file is its
// maker's, only root gives one away, and fchmodat and fchownat do what chmod and chown do. It
// prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

static const char *said(int r) {
	static char text[8][64];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 64, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 64, "%d", r);
	return t;
}

static void whose(const char *what, int dir, const char *name) {
	struct stat s;
	if (fstatat(dir, name, &s, 0))
		printf("%s: %s\n", what, strerror(errno));
	else
		printf("%s: mode %o, its user is mine: %d, its group is its directory's: %d\n", what, s.st_mode & 07777, s.st_uid == getuid(), ({
			       struct stat d;
			       fstat(dir, &d);
			       d.st_gid == s.st_gid;
		       }));
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char path[1024];
	snprintf(path, sizeof path, "%s/owners.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	printf("mkdir: %s\n", said(mkdir(path, 0755)));
	int dir = open(path, O_RDONLY | O_DIRECTORY);
	int file = openat(dir, "file", O_CREAT | O_WRONLY, 0640);
	printf("a directory in it: %s\n", said(mkdirat(dir, "inner", 0700)));
	whose("the file", dir, "file");
	whose("the directory", dir, "inner");

	printf("fchmodat: %s", said(fchmodat(dir, "file", 0604, 0)));
	printf("; not following a link: %s", said(fchmodat(dir, "inner", 0750, AT_SYMLINK_NOFOLLOW)));
	printf("; of nothing: %s; with a flag there is not: %s\n", said(fchmodat(dir, "none", 0600, 0)), said(fchmodat(dir, "file", 0600, 0x4000)));
	whose("the file", dir, "file");
	whose("the directory", dir, "inner");

	printf("fchownat, to whom it is already: %s", said(fchownat(dir, "file", getuid(), getgid(), 0)));
	printf("; to nobody in particular: %s", said(fchownat(dir, "file", -1, -1, AT_SYMLINK_NOFOLLOW)));
	printf("; to root: %s", said(fchownat(dir, "file", 0, -1, 0)));
	printf("; of nothing: %s\n", said(fchownat(dir, "none", -1, -1, 0)));
	printf("chown to root: %s; fchown to root: %s; lchown to another user: %s\n", said(chown(path, 0, 0)), said(fchown(file, 0, -1)), said(lchown(path, getuid() + 1, -1)));
	whose("the file", dir, "file");

	close(file);
	unlinkat(dir, "file", 0);
	unlinkat(dir, "inner", AT_REMOVEDIR);
	close(dir);
	printf("tidied: %s\n", said(rmdir(path)));
	return 0;
}
