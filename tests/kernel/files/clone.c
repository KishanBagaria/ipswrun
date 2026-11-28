// A copy of a file made by the file system (clonefile, clonefileat, fclonefileat), as ditto and
// Finder make one: a file of its own that holds what the first held, with its mode, its dates
// and its attributes. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/clonefile.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/xattr.h>
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

static const char *is(const char *path) {
	static char text[4][128];
	static int next;
	char *t = text[next++ % 4], held[32] = "", attr[32] = "";
	struct stat s;
	if (lstat(path, &s))
		return strerror(errno);
	if (S_ISLNK(s.st_mode)) {
		ssize_t n = readlink(path, held, sizeof held - 1);
		snprintf(t, 128, "a link to \"%.*s\"", n > 0 ? (int)n : 0, held);
		return t;
	}
	int fd = open(path, O_RDONLY);
	ssize_t n = read(fd, held, sizeof held - 1);
	close(fd);
	ssize_t a = getxattr(path, "note", attr, sizeof attr - 1, 0, 0);
	snprintf(t, 128, "\"%.*s\", mode %o, %d links, modified %ld, its note \"%.*s\"", n > 0 ? (int)n : 0, held, s.st_mode & 07777, s.st_nlink, (long)s.st_mtimespec.tv_sec, a > 0 ? (int)a : 0, attr);
	return t;
}

static ino_t inode(const char *path) {
	struct stat s;
	return lstat(path, &s) ? 0 : s.st_ino;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char dir[512];
	snprintf(dir, sizeof dir, "%s/clone.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	mkdir(dir, 0755);
	chdir(dir);
	umask(077);
	int fd = open("file", O_CREAT | O_WRONLY, 0600);
	write(fd, "first", 5);
	fchmod(fd, 0754);
	close(fd);
	setxattr("file", "note", "kept", 4, 0, 0);
	struct timeval then[2] = {{1000, 0}, {2000, 0}};
	utimes("file", then);
	mkdir("inner", 0755);
	symlink("file", "soft");
	int here = open(".", O_RDONLY), inner = open("inner", O_RDONLY);

	printf("the file: %s\n", is("file"));
	printf("clonefile: %s; the copy: %s; a file of its own: %d\n", said(clonefile("file", "copy", 0)), is("copy"), inode("copy") != inode("file") && inode("copy") != 0);
	fd = open("copy", O_WRONLY);
	write(fd, "other", 5);
	close(fd);
	utimes("copy", then); // writing it set the date
	printf("the copy written to: %s; the file still: %s\n", is("copy"), is("file"));
	printf("over a file that is there: %s; of nothing: %s; into no directory: %s\n", said(clonefile("file", "copy", 0)), said(clonefile("none", "x", 0)), said(clonefile("file", "none/x", 0)));
	printf("clonefileat, from one directory into another: %s; the copy: %s\n", said(clonefileat(here, "file", inner, "copy", 0)), is("inner/copy"));
	printf("from no directory: %s; into none: %s; with a flag there is not: %s\n", said(clonefileat(99, "file", inner, "x", 0)), said(clonefileat(here, "file", 99, "x", 0)),
	       said(clonefileat(here, "file", inner, "x", 0x8000)));
	printf("of a link: %s; the copy: %s", said(clonefile("soft", "of-link", 0)), is("of-link"));
	printf("; told not to follow it: %s; the copy: %s\n", said(clonefile("soft", "the-link", CLONE_NOFOLLOW)), is("the-link"));
	printf("as ditto asks (not following, with the access list): %s; the copy: %s\n", said(clonefileat(AT_FDCWD, "file", AT_FDCWD, "dittoed", CLONE_NOFOLLOW | CLONE_ACL)), is("dittoed"));
	fd = open("file", O_RDONLY);
	printf("fclonefileat, of an open file: %s; the copy: %s", said(fclonefileat(fd, inner, "opened", 0)), is("inner/opened"));
	printf("; of one that is not open: %s\n", said(fclonefileat(77, inner, "x", 0)));
	close(fd);

	unlink("inner/copy");
	unlink("inner/opened");
	close(here);
	close(inner);
	rmdir("inner");
	unlink("file");
	unlink("copy");
	unlink("soft");
	unlink("of-link");
	unlink("the-link");
	unlink("dittoed");
	chdir("/");
	printf("tidied: %s\n", said(rmdir(dir)));
	return 0;
}
