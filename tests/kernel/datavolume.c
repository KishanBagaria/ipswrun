// The data volume: where a Mac keeps what changes, beside the sealed volume the system is on.
// What a newly installed one has is there to be read, and what is made there stays. Under
// ipswrun it is a directory of the host's (--data), read through to what the release has for a
// new one. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/clonefile.h>
#include <sys/stat.h>
#include <unistd.h>

static const char *kind(const char *path) {
	struct stat s;
	if (lstat(path, &s))
		return strerror(errno);
	return S_ISDIR(s.st_mode) ? "a directory" : S_ISLNK(s.st_mode) ? "a link" : S_ISREG(s.st_mode) ? "a file" : "something else";
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	static const char *paths[] = {"/Library", "/Library/Preferences", "/Library/Preferences/Logging", "/Library/Preferences/com.apple.security.appsandbox.plist",
	                              "/private/var/db", "/private/var/db/dslocal", "/private/var/tmp", "/var", "/etc/localtime", "/Users/Shared", "/usr/local",
	                              "/Library/No Such Thing"};
	for (size_t i = 0; i < sizeof paths / sizeof *paths; i++)
		printf("%s: %s\n", paths[i], kind(paths[i]));
	char real[PATH_MAX] = "";
	printf("/var/db is really %s\n", realpath("/var/db", real) ? real : strerror(errno));
	char text[256] = "";
	int fd = open("/Library/Preferences/com.apple.security.appsandbox.plist", O_RDONLY);
	ssize_t n = fd < 0 ? -1 : read(fd, text, 5);
	printf("the preferences file begins \"%.*s\"\n", n > 0 ? (int)n : 0, text);
	close(fd);

	// What is made there is there, and is gone when it is removed.
	char path[128];
	snprintf(path, sizeof path, "/private/var/tmp/datavolume.%d", getpid());
	fd = open(path, O_CREAT | O_EXCL | O_RDWR, 0600);
	printf("a file made in /private/var/tmp: %s", fd < 0 ? strerror(errno) : "made");
	printf("; written: %zd", write(fd, "kept", 4));
	n = pread(fd, text, sizeof text, 0);
	printf("; read back by its other name: ");
	close(fd);
	snprintf(text, sizeof text, "/var/tmp/datavolume.%d", getpid());
	fd = open(text, O_RDONLY);
	char back[8] = "";
	n = fd < 0 ? -1 : read(fd, back, sizeof back);
	printf("\"%.*s\"", n > 0 ? (int)n : 0, back);
	close(fd);
	printf("; removed: %d; then: %s\n", unlink(path), kind(path));

	// It is one volume, the user's own directories with it: a file is moved from one part of it
	// to another, is given a second name there, and is copied by the file system, none of which
	// is done between two volumes.
	char there[512], linked[512], cloned[512];
	const char *own = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(path, sizeof path, "/private/tmp/datavolume.%d", getpid());
	snprintf(there, sizeof there, "%s/datavolume.%d", own, getpid());
	snprintf(linked, sizeof linked, "/private/tmp/datavolume.%d.linked", getpid());
	snprintf(cloned, sizeof cloned, "/private/tmp/datavolume.%d.cloned", getpid());
	fd = open(path, O_CREAT | O_EXCL | O_WRONLY, 0600);
	write(fd, "moved", 5);
	close(fd);
	printf("a file of /private/tmp moved to the user's own temporary directory: %s", rename(path, there) ? strerror(errno) : "moved");
	printf("; where it was: %s; where it is: %s\n", kind(path), kind(there));
	struct stat s = {0};
	printf("a second name for it back in /private/tmp: %s", link(there, linked) ? strerror(errno) : "made");
	stat(there, &s);
	printf("; it has %d names", s.st_nlink);
	printf("; copied there by the file system: %s", clonefile(there, cloned, 0) ? strerror(errno) : "copied");
	fd = open(cloned, O_RDONLY);
	n = fd < 0 ? -1 : read(fd, back, sizeof back);
	close(fd);
	printf(", holding \"%.*s\"\n", n > 0 ? (int)n : 0, back);
	printf("tidied: %d\n", unlink(there) + unlink(linked) + unlink(cloned));
	return 0;
}
