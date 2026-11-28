// A name given from a directory, the one a process is in or a descriptor's, is looked for from
// there: a parent that has since been closed to the process stands in the way of the whole
// path and of what is reached by going up, and not of what is in the directory. And links
// that lead to each other, or under themselves. It prints the same natively on a Mac as under
// ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>
static const char *said(int r) { static char t[16][48]; static int n; char *s = t[n++ % 16]; snprintf(s, 48, "%s", r < 0 ? strerror(errno) : "done"); return s; }
static const char *opened(int dir, const char *path) { int fd = openat(dir, path, O_RDONLY); const char *s = said(fd); if (fd >= 0) close(fd); return s; }
int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	char base[1024], abs[1200], cwd[1200];
	snprintf(base, sizeof base, "%s/relsearch.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	mkdir(base, 0700); chdir(base);
	mkdir("a", 0700); mkdir("a/b", 0700); close(open("a/b/f", O_CREAT | O_WRONLY, 0600)); close(open("a/g", O_CREAT | O_WRONLY, 0600));
	symlink("a/b", "to-b"); symlink("loop2", "loop1"); symlink("loop1", "loop2");
	symlink("self/x", "a/b/self");
	int dirfd = open("a/b", O_RDONLY | O_DIRECTORY);
	snprintf(abs, sizeof abs, "%s/a/b/f", base);
	struct stat st;
	chdir("a/b");
	chmod("../../a", 0000);
	printf("in a directory whose parent may no longer be searched: a name in it: %s; by its whole path: %s; from a descriptor of it: %s; the directory itself (.): %s; its parent (..): %s; a name in the parent (../g): %s; where it is (getcwd): %s\n",
	       opened(AT_FDCWD, "f"), opened(AT_FDCWD, abs), opened(dirfd, "f"), said(stat(".", &st)), said(stat("..", &st)), said(stat("../g", &st)), getcwd(cwd, sizeof cwd) ? "said" : strerror(errno));
	printf("  a file made in it: %s; entered again by .: %s; from the descriptor, the parent: %s\n", said(({ int fd = open("new", O_CREAT | O_WRONLY, 0600); if (fd >= 0) close(fd); fd; })), said(chdir(".")), said(fstatat(dirfd, "..", &st, 0)));
	chdir(base);
	printf("from outside: through a link to it: %s; the link itself: %s\n", opened(AT_FDCWD, "to-b/f"), said(lstat("to-b", &st)));
	chmod("a", 0700);
	printf("links that lead to each other: %s; a link under itself: %s, and a name beyond it: %s\n", said(stat("loop1/x", &st)), said(stat("a/b/self", &st)), said(stat("a/b/self/z", &st)));
	unlink("a/b/f"); unlink("a/b/new"); unlink("a/b/self"); unlink("a/g"); unlink("to-b"); unlink("loop1"); unlink("loop2"); close(dirfd); rmdir("a/b"); rmdir("a"); chdir("/"); rmdir(base);
	return 0;
}
