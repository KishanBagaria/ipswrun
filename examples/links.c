// Names for files: symbolic links, which name a path, and hard links, which are another name
// for the same file, and the ways a name is removed. It prints the same natively on a Mac as
// under ipswrun.
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

static const char *is(const char *path, int follow) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	struct stat s;
	if (follow ? stat(path, &s) : lstat(path, &s))
		return strerror(errno);
	snprintf(t, 96, "%s, mode %o, %d links, %lld bytes", S_ISLNK(s.st_mode) ? "a link" : S_ISDIR(s.st_mode) ? "a directory" : "a file", s.st_mode & 07777,
	         S_ISDIR(s.st_mode) ? 0 : s.st_nlink, S_ISDIR(s.st_mode) ? 0 : s.st_size);
	return t;
}

static const char *holds(const char *path) {
	static char text[64];
	int fd = open(path, O_RDONLY);
	if (fd < 0)
		return strerror(errno);
	ssize_t n = read(fd, text, sizeof text - 1);
	text[n > 0 ? n : 0] = 0;
	close(fd);
	return text;
}

static ino_t inode(const char *path) {
	struct stat s;
	return lstat(path, &s) ? 0 : s.st_ino;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char dir[512], buf[256];
	snprintf(dir, sizeof dir, "%s/links.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	mkdir(dir, 0755);
	chdir(dir);
	int fd = open("file", O_CREAT | O_WRONLY, 0640);
	write(fd, "kept", 4);
	close(fd);
	mkdir("inner", 0755);
	int here = open(".", O_RDONLY);

	// A symbolic link names a path, which need not be there.
	printf("symlink: %s; it is %s\n", said(symlink("file", "soft")), is("soft", 0));
	ssize_t n = readlink("soft", buf, sizeof buf);
	printf("it reads \"%.*s\" (%s); followed, it is %s, and holds \"%s\"\n", n > 0 ? (int)n : 0, buf, said(n), is("soft", 1), holds("soft"));
	printf("again: %s; over a file: %s; in no directory: %s\n", said(symlink("file", "soft")), said(symlink("x", "file")), said(symlink("x", "none/soft")));
	printf("to nothing: %s; it is %s; followed: %s; opened: %s\n", said(symlink("nothing/there", "dangling")), is("dangling", 0), is("dangling", 1), holds("dangling"));
	printf("to a directory, from one: %s; through it: %s", said(symlinkat("..", here, "inner/up")), is("inner/up/file", 1));
	printf("; readlinkat: %s", said(n = readlinkat(here, "inner/up", buf, sizeof buf)));
	printf(" \"%.*s\"; into too little room: %s", n > 0 ? (int)n : 0, buf, said(n = readlink("soft", buf, 2)));
	printf(" \"%.*s\"; of a file: %s\n", n > 0 ? (int)n : 0, buf, said(readlink("file", buf, sizeof buf)));
	printf("written through the link: ");
	fd = open("soft", O_WRONLY | O_APPEND);
	write(fd, "!", 1);
	close(fd);
	printf("the file holds \"%s\"; opened not to follow: %s\n", holds("file"), said(open("soft", O_RDONLY | O_NOFOLLOW)));
	printf("the link removed: %s; the file is %s\n", said(unlink("soft")), is("file", 0));

	// A hard link is the file itself, by another name.
	printf("link: %s; the first is %s", said(link("file", "hard")), is("file", 0));
	printf("; the second is the same file: %d\n", inode("file") == inode("hard") && inode("file") != 0);
	printf("again: %s; of nothing: %s; of a directory: %s\n", said(link("file", "hard")), said(link("none", "x")), said(link("inner", "x")));
	symlink("file", "soft");
	printf("of a link: %s, which makes %s", said(linkat(AT_FDCWD, "soft", AT_FDCWD, "of-link", 0)), is("of-link", 0));
	printf("; told to follow it: %s, which makes %s\n", said(linkat(AT_FDCWD, "soft", AT_FDCWD, "of-file", AT_SYMLINK_FOLLOW)), is("of-file", 0));
	printf("the first name removed: %s; the second is %s, and holds \"%s\"\n", said(unlink("file")), is("hard", 0), holds("hard"));

	// A name is removed as what it is.
	printf("unlinkat of a file: %s; of a directory: %s; as a directory, of a file: %s, and of a directory: %s\n", said(unlinkat(here, "of-file", 0)), said(unlinkat(here, "inner", 0)),
	       said(unlinkat(here, "hard", AT_REMOVEDIR)), said(unlinkat(here, "inner", AT_REMOVEDIR)));
	unlink("inner/up");
	printf("with nothing left in it: %s", said(unlinkat(here, "inner", AT_REMOVEDIR)));
	mkdir("inner", 0755);
	printf("; as rm does it (0x100), of a file: %s, and of a directory: %s; with a flag there is not: %s\n", said(unlinkat(here, "hard", 0x100)), said(unlinkat(here, "inner", 0x100)),
	       said(unlinkat(here, "of-link", 0x4000)));

	unlink("hard");
	unlink("of-link");
	unlink("soft");
	unlink("dangling");
	close(here);
	chdir("/");
	printf("tidied: %s\n", said(rmdir(dir)));
	return 0;
}
