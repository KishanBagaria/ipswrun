// access and faccessat look at what is asked: whether a file may be read, written or run by
// who asks, by its mode and whose it is; what is asked of a file of the system, of a directory,
// of a device, of a link that leads nowhere, and with a purpose there is not. It prints the
// same natively on a Mac as under ipswrun.
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
		snprintf(t, 64, "%s", strerror(errno));
	else
		snprintf(t, 64, "yes");
	return t;
}

// all says each of the purposes a path may be asked for: to be there, read, written, run, and
// the three at once.
static void all(const char *what, const char *path) {
	const char *f = said(access(path, F_OK)), *r = said(access(path, R_OK)), *w = said(access(path, W_OK)), *x = said(access(path, X_OK)), *rwx = said(access(path, R_OK | W_OK | X_OK));
	printf("%s: there %s; read %s; written %s; run %s; all three %s\n", what, f, r, w, x, rwx);
}

int main(void) {
	char dir[400], path[512], link[512];
	snprintf(dir, sizeof dir, "%s/accessfor.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	mkdir(dir, 0700);
	static const int modes[] = {0, 0400, 0200, 0100, 0644, 0755, 0070, 0007, 0077};
	for (unsigned i = 0; i < sizeof modes / sizeof *modes; i++) {
		char what[64];
		snprintf(what, sizeof what, "a file of one's own of mode %03o", modes[i]);
		snprintf(path, sizeof path, "%s/file", dir);
		close(open(path, O_CREAT | O_WRONLY, 0600));
		chmod(path, modes[i]);
		all(what, path);
		unlink(path);
	}
	static const int dirs[] = {0, 0500, 0300, 0700};
	for (unsigned i = 0; i < sizeof dirs / sizeof *dirs; i++) {
		char what[64];
		snprintf(what, sizeof what, "a directory of one's own of mode %03o", dirs[i]);
		snprintf(path, sizeof path, "%s/inner", dir);
		mkdir(path, 0700);
		chmod(path, dirs[i]);
		all(what, path);
		rmdir(path);
	}
	all("a file of the system", "/System/Library/CoreServices/SystemVersion.plist");
	all("a directory of the system", "/usr/lib");
	all("the root", "/");
	all("/dev/null", "/dev/null");
	all("what is not there", "/nowhere/at/all");
	snprintf(path, sizeof path, "%s/file/under", dir);
	snprintf(link, sizeof link, "%s/file", dir);
	close(open(link, O_CREAT | O_WRONLY, 0644));
	all("a name under a file", path);
	all("the empty name", "");

	// A link: followed, and not; and one that leads nowhere.
	snprintf(link, sizeof link, "%s/link", dir);
	snprintf(path, sizeof path, "%s/file", dir);
	chmod(path, 0);
	symlink(path, link);
	const char *r1 = said(access(link, R_OK)), *r2 = said(faccessat(AT_FDCWD, link, R_OK, AT_SYMLINK_NOFOLLOW)), *r3 = said(faccessat(AT_FDCWD, link, X_OK, AT_SYMLINK_NOFOLLOW));
	printf("a link to a file of mode 000, read: %s; the link itself, read: %s, run: %s", r1, r2, r3);
	unlink(link);
	symlink("/nowhere/at/all", link);
	r1 = said(access(link, F_OK));
	r2 = said(faccessat(AT_FDCWD, link, F_OK, AT_SYMLINK_NOFOLLOW));
	printf("; a link that leads nowhere: %s, the link itself: %s\n", r1, r2);

	// Purposes there are not, flags there are not, by a directory's descriptor, and as who one acts as.
	chmod(path, 0644);
	int d = open(dir, O_RDONLY);
	r1 = said(access(path, 8));
	r2 = said(access(path, 0x100));
	r3 = said(faccessat(d, "file", R_OK, 0));
	const char *r4 = said(faccessat(d, "file", R_OK, AT_EACCESS)), *r5 = said(faccessat(d, "file", R_OK, 0x8000));
	printf("a purpose of 8: %s; of 0x100: %s; by its directory's descriptor: %s; as who one acts as: %s; with a flag there is not: %s; by a descriptor that is none: %s; by a file's: %s\n", r1, r2, r3, r4, r5,
	       said(faccessat(99, "file", R_OK, 0)), said(faccessat(open(path, O_RDONLY), "file", R_OK, 0)));
	unlink(link);
	unlink(path);
	rmdir(dir);
	return 0;
}
