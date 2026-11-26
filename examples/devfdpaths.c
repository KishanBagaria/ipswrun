// A descriptor's name, /dev/fd/N, given to the calls that take a path. Some act on the open
// file (its mode, its owner, its dates); the rest are told of a file of the device
// file system that is no directory, no link, and not to be run, renamed or removed. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <mach-o/dyld.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <sys/xattr.h>
#include <unistd.h>

extern char **environ;

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

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char path[512], other[512], name[64], below[80], buf[64];
	snprintf(path, sizeof path, "%s/devfdpaths.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	snprintf(other, sizeof other, "%s.other", path);
	int fd = open(path, O_CREAT | O_RDWR, 0644);
	write(fd, "abcdef", 6);
	snprintf(name, sizeof name, "/dev/fd/%d", fd);
	struct stat s;

	// What acts on the open file.
	printf("chmod: %s", said(chmod(name, 0600)));
	stat(path, &s);
	printf(", the file's mode then %o; chown to its own owner: %s", s.st_mode & 0777, said(chown(name, getuid(), getgid())));
	struct timeval then[2] = {{1000, 0}, {2000, 0}};
	printf("; utimes: %s", said(utimes(name, then)));
	stat(path, &s);
	printf(", the file's date then %ld\n", (long)s.st_mtimespec.tv_sec);
	printf("access, of one open to read and write: to write %s, to run %s\n", said(access(name, W_OK)), said(access(name, X_OK)));

	// What is told of the name, and not of the file.
	printf("an attribute set: %s; read: %s; listed: %s; removed: %s\n", said(setxattr(name, "note", "x", 1, 0, 0)), said(getxattr(name, "note", buf, sizeof buf, 0, 0)), said(listxattr(name, buf, sizeof buf, 0)),
	       said(removexattr(name, "note", 0)));
	printf("readlink: %s; truncate: %s", said(readlink(name, buf, sizeof buf)), said(truncate(name, 2)));
	stat(path, &s);
	printf(", the file still of %lld bytes; pathconf: %s\n", s.st_size, said(pathconf(name, _PC_NAME_MAX)));
	printf("unlink: %s; renamed: %s; a file renamed to it: %s; a link to it: %s\n", said(unlink(name)), said(rename(name, other)), said(rename(path, name)), said(link(name, other)));
	printf("made as a link: %s; as a directory: %s; as a pipe: %s; as a new file and no other: %s\n", said(symlink("x", name)), said(mkdir(name, 0755)), said(mkfifo(name, 0600)),
	       said(open(name, O_CREAT | O_EXCL | O_RDWR, 0600)));
	printf("rmdir: %s; chdir: %s; opened as a directory: %s\n", said(rmdir(name)), said(chdir(name)), said(open(name, O_RDONLY | O_DIRECTORY)));

	// A directory's descriptor is no directory by its name.
	int dir = open("/", O_RDONLY);
	snprintf(name, sizeof name, "/dev/fd/%d", dir);
	snprintf(below, sizeof below, "%s/dev", name);
	int again = open(name, O_RDONLY);
	printf("a directory's: chdir %s; opened %d, as a directory %s; a file below it: %s\n", said(chdir(name)), again > dir, said(open(name, O_RDONLY | O_DIRECTORY)), said(stat(below, &s)));

	// A program's is not to be run by it.
	char self[1024];
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	int program = open(self, O_RDONLY);
	snprintf(name, sizeof name, "/dev/fd/%d", program);
	char *argv[] = {"it", NULL};
	pid_t pid = 0;
	int refused = posix_spawn(&pid, name, NULL, NULL, argv, environ);
	if (refused == 0)
		waitpid(pid, NULL, 0);
	printf("a program's: access to run %s; started: %d (%s)\n", said(access(name, X_OK)), refused, strerror(refused));

	// One that is not open.
	printf("of a descriptor that is not open: chmod %s; unlink %s; an attribute read %s\n", said(chmod("/dev/fd/99", 0600)), said(unlink("/dev/fd/99")),
	       said(getxattr("/dev/fd/99", "note", buf, sizeof buf, 0, 0)));
	close(fd);
	printf("tidied: %s\n", said(unlink(path)));
	return 0;
}
