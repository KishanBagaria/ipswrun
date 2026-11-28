// /dev/fd listed: a process's descriptors by their numbers, the one it is read through among
// them, and no names for the directory itself or the one above. And what mmap makes of the
// descriptor it is given when it maps memory that is no file's. It prints the same natively on
// a Mac as under ipswrun.
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

static const char *mapped(int flags, int fd) {
	static char text[8][48];
	static int next;
	char *t = text[next++ % 8];
	void *p = mmap(NULL, 16384, PROT_READ | PROT_WRITE, flags, fd, 0);
	snprintf(t, 48, "%s", p == MAP_FAILED ? strerror(errno) : "mapped");
	if (p != MAP_FAILED)
		munmap(p, 16384);
	return t;
}

int main(void) {
	int a = open("/dev/null", O_RDONLY), b = dup(a);
	DIR *d = opendir("/dev/fd");
	if (!d) {
		printf("/dev/fd is not opened: %s\n", strerror(errno));
		return 1;
	}
	int through = dirfd(d), has_a = 0, has_b = 0, has_through = 0, dots = 0, others = 0, kinds = 0, order = 1, last = -1;
	for (struct dirent *e; (e = readdir(d));) {
		if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) {
			dots++;
			continue;
		}
		char *end;
		long n = strtol(e->d_name, &end, 10);
		others += *end != 0;
		kinds += e->d_type != DT_UNKNOWN;
		order = order && n > last;
		last = (int)n;
		has_a += n == a;
		has_b += n == b;
		has_through += n == through;
	}
	printf("/dev/fd lists a descriptor: %d, its duplicate: %d, the one it is read through: %d; names that are no number: %d, for itself and the one above: %d; entries that say a kind: %d; in order: %d\n", has_a, has_b, has_through,
	       others, dots, kinds, order);
	close(b);
	rewinddir(d);
	int again = 0, closed = 0;
	for (struct dirent *e; (e = readdir(d));) {
		again += atoi(e->d_name) == a;
		closed += atoi(e->d_name) == b;
	}
	struct stat st;
	const char *r1 = fstat(through, &st) ? strerror(errno) : S_ISDIR(st.st_mode) ? "a directory" : "no directory";
	printf("read again from the start after the duplicate was closed: the descriptor %d, the duplicate %d; what it is read through is %s\n", again, closed, r1);
	closedir(d);

	const char *m1 = mapped(MAP_PRIVATE, -1), *m2 = mapped(MAP_SHARED, -1), *m3 = mapped(MAP_PRIVATE, 99), *m4 = mapped(MAP_PRIVATE | MAP_ANON, -1), *m5 = mapped(MAP_PRIVATE | MAP_ANON, 0);
	printf("mmap of no descriptor and no word that it is no file's, for oneself: %s, shared: %s; of a descriptor that is none: %s; of no file, with -1: %s, with 0: %s", m1, m2, m3, m4, m5);
	m1 = mapped(MAP_PRIVATE | MAP_ANON, 99);
	m2 = mapped(MAP_PRIVATE | MAP_ANON, 1);
	m3 = mapped(MAP_PRIVATE | MAP_ANON, (int)(240u << 24));
	m4 = mapped(MAP_ANON, -1);
	m5 = mapped(MAP_PRIVATE | MAP_SHARED | MAP_ANON, -1);
	printf(", with 99: %s, with 1: %s, with a tag to know it by: %s; neither for oneself nor shared: %s; both: %s\n", m1, m2, m3, m4, m5);
	return 0;
}
