// What a directory's mode lets its owner do with it: one that may be read and not searched
// (0600), searched and not read (0300), and neither (0000), by each call that lists it, enters
// it, or names something inside it. And what a descriptor opened before the mode was taken
// away still may. It prints the same natively on a Mac as under ipswrun.
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/stat.h>
#include <unistd.h>

static const char *said(int r) {
	static char text[16][48];
	static int next;
	char *t = text[next++ % 16];
	snprintf(t, 48, "%s", r < 0 ? strerror(errno) : "yes");
	return t;
}

static const char *opened(const char *path, int flags) {
	int fd = open(path, flags, 0600);
	const char *s = said(fd);
	if (fd >= 0)
		close(fd);
	return s;
}

// listed reads a directory's names through fd: how many besides the dots, or why not.
static const char *listed(int fd) {
	static char text[4][48];
	static int next;
	char *t = text[next++ % 4];
	int copy = dup(fd), n = 0;
	lseek(copy, 0, SEEK_SET);
	DIR *d = fdopendir(copy);
	if (!d) {
		snprintf(t, 48, "%s", strerror(errno));
		close(copy);
		return t;
	}
	errno = 0;
	for (struct dirent *e; (e = readdir(d));)
		n += strcmp(e->d_name, ".") && strcmp(e->d_name, "..");
	if (errno)
		snprintf(t, 48, "%s", strerror(errno));
	else
		snprintf(t, 48, "%d names", n);
	closedir(d);
	return t;
}

// bulk asks getattrlistbulk of fd for the attributes in common: how many entries, or why not.
static const char *bulk(int fd, attrgroup_t common) {
	static char text[8][48];
	static int next;
	char *t = text[next++ % 8];
	static char buf[8192];
	struct attrlist want = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_RETURNED_ATTRS | common};
	lseek(fd, 0, SEEK_SET);
	int n = getattrlistbulk(fd, &want, buf, sizeof buf, 0);
	if (n < 0)
		snprintf(t, 48, "%s", strerror(errno));
	else
		snprintf(t, 48, "%d entries", n);
	return t;
}

static void with(const char *label, mode_t mode) {
	char back[1024];
	getcwd(back, sizeof back);
	mkdir("dir", 0700);
	mkdir("dir/sub", 0700);
	close(open("dir/file", O_CREAT | O_WRONLY, 0600));
	close(open("dir/gone", O_CREAT | O_WRONLY, 0600));
	close(open("beside", O_CREAT | O_WRONLY, 0600));
	int before = open("dir", O_RDONLY | O_DIRECTORY);
	chmod("dir", mode);
	printf("%s:\n", label);

	struct stat st;
	printf("  itself: stat %s, open to read %s, as a directory %s, to write %s, to search only %s\n", said(stat("dir", &st)), opened("dir", O_RDONLY), opened("dir", O_RDONLY | O_DIRECTORY), opened("dir", O_WRONLY),
	       opened("dir", O_SEARCH));
	int now = open("dir", O_RDONLY | O_DIRECTORY);
	if (now >= 0)
		printf("  listed through a descriptor opened now: %s; by getattrlistbulk for names %s, names and kinds %s, names and numbers %s, kinds alone %s, names and errors %s, names and times %s\n", listed(now), bulk(now, ATTR_CMN_NAME),
		       bulk(now, ATTR_CMN_NAME | ATTR_CMN_OBJTYPE), bulk(now, ATTR_CMN_NAME | ATTR_CMN_FILEID), bulk(now, ATTR_CMN_OBJTYPE), bulk(now, ATTR_CMN_NAME | ATTR_CMN_ERROR), bulk(now, ATTR_CMN_NAME | ATTR_CMN_MODTIME));
	printf("  listed through one opened before: %s; by getattrlistbulk for names %s, and times %s\n", listed(before), bulk(before, ATTR_CMN_NAME), bulk(before, ATTR_CMN_NAME | ATTR_CMN_MODTIME));

	const char *c1 = said(chdir("dir"));
	chdir(back);
	const char *c2 = said(fchdir(before));
	chdir(back);
	const char *c3 = now >= 0 ? said(fchdir(now)) : "not opened";
	chdir(back);
	printf("  entered by its name: %s, by the descriptor opened before: %s, by one opened now: %s\n", c1, c2, c3);

	printf("  a file inside: stat %s, lstat %s, access %s, open %s; a name that is not there: stat %s; a directory inside: stat %s, open %s\n", said(stat("dir/file", &st)), said(lstat("dir/file", &st)), said(access("dir/file", F_OK)),
	       opened("dir/file", O_RDONLY), said(stat("dir/none", &st)), said(stat("dir/sub", &st)), opened("dir/sub", O_RDONLY));
	printf("  through the descriptor opened before: fstatat %s, openat %s, a new file %s\n", said(fstatat(before, "file", &st, 0)), said(({
		       int fd = openat(before, "file", O_RDONLY);
		       if (fd >= 0)
			       close(fd);
		       fd;
	       })),
	       said(({
		       int fd = openat(before, "made", O_CREAT | O_WRONLY | O_EXCL, 0600);
		       if (fd >= 0)
			       close(fd);
		       fd;
	       })));
	printf("  a new file: %s, a new directory: %s, a link: %s, a name taken away: %s, a file brought in: %s, a file taken out: %s, a directory inside removed: %s\n", opened("dir/new", O_CREAT | O_WRONLY | O_EXCL), said(mkdir("dir/newdir", 0700)),
	       said(symlink("x", "dir/link")), said(unlink("dir/gone")), said(rename("beside", "dir/beside")), said(rename("dir/file", "out")), said(rmdir("dir/sub")));
	printf("  removed while it has names: %s\n", said(rmdir("dir")));

	if (now >= 0)
		close(now);
	close(before);
	chmod("dir", 0700);
	const char *files[] = {"dir/file", "dir/gone", "dir/new", "dir/made", "dir/link", "dir/beside", "beside", "out"};
	for (unsigned i = 0; i < sizeof files / sizeof *files; i++)
		unlink(files[i]);
	rmdir("dir/sub");
	rmdir("dir/newdir");
	rmdir("dir");
}

int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	char dir[1024];
	snprintf(dir, sizeof dir, "%s/searchmay.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	if (mkdir(dir, 0700) || chdir(dir)) {
		printf("no directory to work in: %s\n", strerror(errno));
		return 1;
	}
	with("a directory its owner may read, write and search (0700)", 0700);
	with("read and write, not search (0600)", 0600);
	with("write and search, not read (0300)", 0300);
	with("read and search, not write (0500)", 0500);
	with("search only (0100)", 0100);
	with("nothing (0000)", 0000);
	chdir("/");
	rmdir(dir);
	return 0;
}
