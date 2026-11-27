// What open is refused: a file of the system volume is read and never written, emptied or
// added to, nothing is made beside it, and a file of one's own is opened as its mode allows.
// It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

static const char *opened(const char *path, int flags) {
	static char text[8][48];
	static int next;
	char *t = text[next++ % 8];
	int fd = open(path, flags, 0644);
	snprintf(t, 48, "%s", fd < 0 ? strerror(errno) : "opened");
	if (fd >= 0)
		close(fd);
	return t;
}

int main(void) {
	const char *file = "/System/Library/CoreServices/SystemVersion.plist";
	const char *r1 = opened(file, O_RDONLY), *r2 = opened(file, O_WRONLY), *r3 = opened(file, O_RDWR), *r4 = opened(file, O_RDONLY | O_TRUNC), *r5 = opened(file, O_WRONLY | O_APPEND);
	printf("a file of the system to read: %s; to write: %s; both: %s; to read and be emptied: %s; to be added to: %s", r1, r2, r3, r4, r5);
	r1 = opened(file, O_RDONLY | O_CREAT);
	r2 = opened(file, O_RDONLY | O_CREAT | O_EXCL);
	printf("; to read, made if it is not there: %s; and only if it is not: %s\n", r1, r2);
	r1 = opened("/usr/lib", O_RDONLY);
	r2 = opened("/usr/lib", O_WRONLY);
	r3 = opened("/usr/lib/openmay.new", O_WRONLY | O_CREAT);
	r4 = opened("/openmay.new", O_WRONLY | O_CREAT);
	r5 = opened("/", O_WRONLY);
	printf("a directory of the system to read: %s; to write: %s; a file made in it: %s; one made in the root: %s; the root to write: %s; a file made among the devices: %s; one that is nowhere, to read: %s\n", r1, r2, r3, r4, r5,
	       opened("/dev/openmay.new", O_WRONLY | O_CREAT), opened("/usr/lib/openmay.none", O_RDONLY));

	char path[512];
	snprintf(path, sizeof path, "%s/openmay.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	static const int modes[] = {0, 0400, 0200, 0600, 0044, 0066};
	for (unsigned i = 0; i < sizeof modes / sizeof *modes; i++) {
		close(open(path, O_CREAT | O_WRONLY, 0600));
		chmod(path, modes[i]);
		r1 = opened(path, O_RDONLY);
		r2 = opened(path, O_WRONLY);
		r3 = opened(path, O_RDWR);
		r4 = opened(path, O_RDONLY | O_TRUNC);
		printf("a file of one's own of mode %03o to read: %s; to write: %s; both: %s; to read and be emptied: %s\n", modes[i], r1, r2, r3, r4);
		unlink(path);
	}
	mkdir(path, 0700);
	chmod(path, 0300);
	r1 = opened(path, O_RDONLY);
	chmod(path, 0500);
	r2 = opened(path, O_RDONLY);
	r3 = opened(path, O_WRONLY);
	printf("a directory of one's own that may not be read, to read: %s; one that may, to read: %s, to write: %s\n", r1, r2, r3);
	rmdir(path);
	return 0;
}
