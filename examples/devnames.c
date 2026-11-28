// The names every /dev has: each is among those the directory lists, with the kind the list
// says and the kind lstat says, and /dev/stdin, stdout and stderr are links to the
// descriptors' own names. It prints the same natively on a Mac as under ipswrun.
#include <dirent.h>
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

int main(void) {
	static const char *names[] = {"fd", "null", "random", "stderr", "stdin", "stdout", "tty", "urandom", "zero"};
	for (unsigned i = 0; i < sizeof names / sizeof *names; i++) {
		DIR *d = opendir("/dev");
		int listed = 0, kind = -1;
		for (struct dirent *e; d && (e = readdir(d));)
			if (!strcmp(e->d_name, names[i])) {
				listed++;
				kind = e->d_type;
			}
		if (d)
			closedir(d);
		char path[64], text[64] = {0};
		snprintf(path, sizeof path, "/dev/%s", names[i]);
		struct stat is, leads;
		const char *l = lstat(path, &is) ? strerror(errno) : S_ISLNK(is.st_mode) ? "a link" : S_ISDIR(is.st_mode) ? "a directory" : S_ISCHR(is.st_mode) ? "a device of characters" : "something else";
		const char *s = stat(path, &leads) ? strerror(errno) : "there";
		long n = readlink(path, text, sizeof text - 1);
		printf("%s: listed %d, as kind %d; by lstat %s; followed: %s; read as a link: %s\n", path, listed, kind, l, i >= 3 && i <= 5 ? s : "-", n < 0 ? strerror(errno) : text);
	}
	return 0;
}
