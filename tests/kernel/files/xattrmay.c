// Which calls on a file's extended attributes its mode refuses its owner: to list and read
// them it has to be one that may be read, to set one or take one away one that may be
// written, as it is when the call is made, through a descriptor opened before too. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/xattr.h>
#include <unistd.h>
static const char *said(long r) { static char t[16][48]; static int n; char *s = t[n++ % 16]; if (r < 0) snprintf(s, 48, "%s", strerror(errno)); else snprintf(s, 48, "%ld", r); return s; }
static void with(const char *what, const char *path, mode_t mode) {
	char buf[64];
	chmod(path, 0700);
	setxattr(path, "user.kept", "v", 1, 0, 0);
	int fd = open(path, O_RDONLY);
	chmod(path, mode);
	printf("%s of mode %03o: listed %s; read %s; the size of one %s; one that is not there %s; set %s; set anew %s; taken away %s; by a descriptor opened before: listed %s, read %s, set %s\n", what, mode,
	       said(listxattr(path, buf, sizeof buf, 0) < 0 ? -1 : 0), said(getxattr(path, "user.kept", buf, sizeof buf, 0, 0)), said(getxattr(path, "user.kept", NULL, 0, 0, 0)), said(getxattr(path, "user.none", buf, sizeof buf, 0, 0)),
	       said(setxattr(path, "user.kept", "w", 1, 0, 0)), said(setxattr(path, "user.new", "n", 1, 0, 0)), said(removexattr(path, "user.new", 0)), said(flistxattr(fd, buf, sizeof buf, 0) < 0 ? -1 : 0),
	       said(fgetxattr(fd, "user.kept", buf, sizeof buf, 0, 0)), said(fsetxattr(fd, "user.fd", "f", 1, 0, 0)));
	close(fd);
	chmod(path, 0700);
	removexattr(path, "user.kept", 0); removexattr(path, "user.new", 0); removexattr(path, "user.fd", 0);
}
int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	char f[1024], d[1024];
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(f, sizeof f, "%s/xattrmay.%d", tmp, getpid()); snprintf(d, sizeof d, "%s/xattrmay.%d.d", tmp, getpid());
	close(open(f, O_CREAT | O_WRONLY, 0600)); mkdir(d, 0700);
	mode_t modes[] = {0600, 0400, 0200, 0000};
	for (int i = 0; i < 4; i++) with("a file", f, modes[i]);
	mode_t dmodes[] = {0700, 0500, 0300, 0100, 0000};
	for (int i = 0; i < 5; i++) with("a directory", d, dmodes[i]);
	unlink(f); rmdir(d);
	return 0;
}
