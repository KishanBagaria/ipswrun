// What each call that changes a file says of one on the system volume, which nobody writes:
// of a file there, a directory there, a name to be made there, and the root; by a path and
// by a descriptor. And of a name on the way to which a directory may not be searched. It
// prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/clonefile.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/un.h>
#include <sys/xattr.h>
#include <unistd.h>

#define FILE_ "/System/Library/CoreServices/SystemVersion.plist"
#define DIR_ "/System/Library/CoreServices"
#define NEW_ "/System/Library/CoreServices/ipswrun-new"

static const char *said(int r) {
	static char text[16][48];
	static int next;
	char *t = text[next++ % 16];
	snprintf(t, 48, "%s", r < 0 ? strerror(errno) : "done");
	return t;
}

// bound binds a socket to a name: what that comes to.
static const char *bound(const char *path) {
	int s = socket(AF_UNIX, SOCK_STREAM, 0);
	struct sockaddr_un a = {.sun_family = AF_UNIX};
	snprintf(a.sun_path, sizeof a.sun_path, "%s", path);
	const char *r = said(bind(s, (struct sockaddr *)&a, sizeof a));
	close(s);
	return r;
}

static const char *opened(const char *path, int flags) {
	int fd = open(path, flags, 0600);
	const char *r = said(fd);
	if (fd >= 0)
		close(fd);
	return r;
}

int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	char own[1024], made[1024];
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(own, sizeof own, "%s/sysvolume.%d", tmp, getpid());
	snprintf(made, sizeof made, "%s/sysvolume.%d.b", tmp, getpid());
	close(open(own, O_CREAT | O_WRONLY, 0600));
	struct timeval times[2] = {{1000000000, 0}, {1000000000, 0}};
	struct attrlist list = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_MODTIME};
	struct timespec when = {1000000000, 0};

	printf("a file of the system: its mode set: %s; its owner: %s; its times: %s; its flags: %s; an attribute set: %s, and taken away: %s; a time set as an attribute: %s\n", said(chmod(FILE_, 0644)), said(chown(FILE_, getuid(), getgid())),
	       said(utimes(FILE_, times)), said(chflags(FILE_, UF_HIDDEN)), said(setxattr(FILE_, "user.ipswrun", "x", 1, 0, 0)), said(removexattr(FILE_, "user.ipswrun", 0)), said(setattrlist(FILE_, &list, &when, sizeof when, 0)));
	printf("  given another name beside it: %s; one's own name: %s; a second name beside it: %s, and among one's own: %s; copied beside it: %s, and among one's own: %s\n", said(rename(FILE_, NEW_)), said(rename(FILE_, made)), said(link(FILE_, NEW_)),
	       said(link(FILE_, made)), said(clonefile(FILE_, NEW_, 0)), said(clonefile(FILE_, made, 0)));
	unlink(made);

	int fd = open(FILE_, O_RDONLY);
	printf("  by a descriptor it is read through: its mode: %s; its owner: %s; its times: %s; its flags: %s; an attribute: %s; its length: %s\n", said(fchmod(fd, 0644)), said(fchown(fd, getuid(), getgid())), said(futimes(fd, times)),
	       said(fchflags(fd, UF_HIDDEN)), said(fsetxattr(fd, "user.ipswrun", "x", 1, 0, 0)), said(ftruncate(fd, 10)));
	close(fd);

	printf("a directory of the system: its mode: %s; its owner: %s; its times: %s; its flags: %s; an attribute: %s; given another name: %s; removed: %s\n", said(chmod(DIR_, 0755)), said(chown(DIR_, getuid(), getgid())), said(utimes(DIR_, times)),
	       said(chflags(DIR_, UF_HIDDEN)), said(setxattr(DIR_, "user.ipswrun", "x", 1, 0, 0)), said(rename(DIR_, "/System/Library/ipswrun-new")), said(rmdir(DIR_)));
	printf("a name to be made in it: a file of one's own brought in: %s; a link to it: %s; a link to anything: %s; a pipe: %s; a socket: %s; a copy of one's own: %s; a directory of one's own brought in: %s\n", said(rename(own, NEW_)), said(link(own, NEW_)),
	       said(symlink("anything", NEW_)), said(mkfifo(NEW_, 0600)), bound(NEW_), said(clonefile(own, NEW_, 0)), said(({
		       mkdir(made, 0700);
		       int r = rename(made, NEW_);
		       rmdir(made);
		       r;
	       })));
	printf("  a name that is there already: a file of one's own put in its place: %s; a link to anything: %s; a second name for one's own: %s\n", said(rename(own, FILE_)), said(symlink("anything", FILE_)), said(link(own, FILE_)));

	printf("the root: its mode: %s; its times: %s; a link made in it: %s; a file of one's own brought in: %s; a pipe: %s; a socket: %s\n", said(chmod("/", 0755)), said(utimes("/", times)), said(symlink("anything", "/ipswrun-new")),
	       said(rename(own, "/ipswrun-new")), said(mkfifo("/ipswrun-new", 0600)), bound("/ipswrun-new"));
	printf("  a second name for one's own: %s; a name that is there already (/usr): a link to anything: %s; a second name for one's own: %s; a pipe: %s; a socket: %s; a file of one's own put in its place: %s\n", said(link(own, "/ipswrun-new")),
	       said(symlink("anything", "/usr")), said(link(own, "/usr")), said(mkfifo("/usr", 0600)), bound("/usr"), said(rename(own, "/usr")));
	printf("a name that is there already in a directory of the system: a pipe: %s; a socket: %s; a directory: %s; a file to be made only if it is not: %s\n", said(mkfifo(FILE_, 0600)), bound(FILE_), said(mkdir(FILE_, 0700)), opened(FILE_, O_CREAT | O_EXCL | O_WRONLY));
	int mine = open(own, O_RDONLY);
	printf("a file of one's own, by a descriptor it is read through: its length: %s; its mode: %s\n", said(ftruncate(mine, 10)), said(fchmod(mine, 0600)));
	close(mine);
	printf("a name that is not there, in a directory of the system that is not there: its mode: %s; given another name: %s; a link made: %s\n", said(chmod("/System/ipswrun-none/x", 0644)), said(rename("/System/ipswrun-none/x", made)),
	       said(symlink("anything", "/System/ipswrun-none/x")));

	// A directory that is root's and lets nobody else in: what is said of a name beyond it.
	struct stat st;
	printf("root's own home: looked at: %s; opened: %s; entered: %s; a name in it looked at: %s, opened: %s, made: %s, removed: %s; a name two deep: %s\n", said(stat("/private/var/root", &st)), opened("/private/var/root", O_RDONLY),
	       said(chdir("/private/var/root")), said(stat("/private/var/root/x", &st)), opened("/private/var/root/x", O_RDONLY), opened("/private/var/root/x", O_CREAT | O_WRONLY), said(unlink("/private/var/root/x")),
	       said(stat("/private/var/root/x/y", &st)));
	printf("  by access: %s; read as a link: %s; a directory made: %s; its mode set: %s; given a name of one's own: %s\n", said(access("/private/var/root/x", F_OK)), said((int)readlink("/private/var/root/x", made, 10)), said(mkdir("/private/var/root/x", 0700)),
	       said(chmod("/private/var/root/x", 0644)), said(rename("/private/var/root/x", own)));

	unlink(own);
	return 0;
}
