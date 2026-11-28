// A path that ends in a slash names a directory: it is of what a link leads to and never of the
// link, of what is no directory it is refused, and nothing is made by such a name but a
// directory. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

static const char *kind(const char *path, int follow) {
	struct stat s;
	if (follow ? stat(path, &s) : lstat(path, &s))
		return strerror(errno);
	return S_ISLNK(s.st_mode) ? "a link" : S_ISDIR(s.st_mode) ? "a directory" : S_ISREG(s.st_mode) ? "a file" : "something else";
}

static const char *said(long r) { return r < 0 ? strerror(errno) : "yes"; }

static void of(const char *path) {
	char text[256];
	const char *l = kind(path, 0), *s = kind(path, 1), *r = said(readlink(path, text, sizeof text));
	int fd = open(path, O_RDONLY);
	const char *o = said(fd);
	if (fd >= 0)
		close(fd);
	printf("%s: by lstat %s, by stat %s; read as a link: %s; opened: %s; there: %s\n", path, l, s, r, o, said(access(path, F_OK)));
}

int main(void) {
	char dir[400];
	snprintf(dir, sizeof dir, "%s/trailing.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	mkdir(dir, 0700);
	chdir(dir);
	mkdir("dir", 0700);
	close(open("file", O_CREAT | O_WRONLY, 0600));
	symlink("dir", "todir");
	symlink("file", "tofile");
	symlink("nowhere", "tonone");
	static const char *paths[] = {"dir/", "file/", "todir", "todir/", "tofile/", "tonone/", "todir/.", "dir//", "todir//", "/etc/", "/etc/passwd/", "/tmp/", "/System/Library/CoreServices/SystemVersion.plist/", "/usr/lib/", "//"};
	for (unsigned i = 0; i < sizeof paths / sizeof *paths; i++)
		of(paths[i]);

	const char *r1 = said(unlink("todir/") - 0), *r2 = said(rmdir("todir/."));
	const char *r3 = said(rmdir("todir/"));
	printf("a link to a directory, written as one, unlinked: %s; removed as a directory by its own name in it: %s; removed as a directory: %s, and then the directory is %s and the link %s\n", r1, r2, r3, kind("dir", 0), kind("todir", 0));
	r1 = said(mkdir("new/", 0700));
	int fd = open("made/", O_CREAT | O_WRONLY, 0600);
	r2 = said(fd);
	r3 = said(rename("file", "moved/"));
	const char *r4 = said(rename("file", "new/"));
	printf("a directory made by such a name: %s; a file: %s; a file moved to such a name that is not there: %s; to one that is a directory: %s\n", r1, r2, r3, r4);
	unlink("todir");
	unlink("tofile");
	unlink("tonone");
	unlink("file");
	rmdir("dir");
	rmdir("new");
	chdir("/");
	rmdir(dir);
	return 0;
}
