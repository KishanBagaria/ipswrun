// A file mapped into memory to be shared (mmap with MAP_SHARED): what is written through the
// mapping is the file's, and what is written to the file is the mapping's, at once and without
// msync; two mappings, and a copy of the process, share it; and what msync and the rest refuse.
// It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <mach/mach.h>
#include <setjmp.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/clonefile.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#define PAGE 16384

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

static const char *mapped(void *p) {
	static char text[4][64];
	static int next;
	char *t = text[next++ % 4];
	if (p == MAP_FAILED)
		snprintf(t, 64, "failed (%s)", strerror(errno));
	else
		snprintf(t, 64, "mapped");
	return t;
}

// at reads a few bytes of the file where they are, as text.
static const char *at(int fd, off_t where, size_t n) {
	static char text[4][32];
	static int next;
	char *t = text[next++ % 4];
	memset(t, 0, 32);
	if (pread(fd, t, n, where) < 0)
		snprintf(t, 32, "(%s)", strerror(errno));
	for (size_t i = 0; i < n; i++)
		if (t[i] == 0)
			t[i] = '.';
	return t;
}

// touched reads a byte of memory, or writes one, and says what it read or which signal came.
static sigjmp_buf back;
static volatile sig_atomic_t came;
static void fault(int sig) {
	came = sig;
	siglongjmp(back, 1);
}
static const char *touched(char *p, int write) {
	static char text[4][32];
	static int next;
	char *t = text[next++ % 4];
	came = 0;
	if (sigsetjmp(back, 1) == 0) {
		if (write)
			*p = 'w';
		snprintf(t, 32, "%d", *p);
	} else
		snprintf(t, 32, "signal %d", (int)came);
	return t;
}

int main(void) {
	char path[256];
	const char *dir = getenv("TMPDIR");
	snprintf(path, sizeof path, "%s/mapfile.XXXXXX", dir && *dir ? dir : "/tmp");
	int fd = mkstemp(path);
	char page[PAGE];
	memset(page, 'a', sizeof page);
	for (int i = 0; i < 3; i++)
		write(fd, page, sizeof page);

	char *m = mmap(NULL, 3 * PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	printf("a file of three pages mapped to be shared: %s; it reads '%c'\n", mapped(m), m != MAP_FAILED ? m[100] : '?');
	memcpy(m + 100, "MAPPED", 6);
	printf("written through the mapping: the file reads \"%s\"", at(fd, 100, 6));
	pwrite(fd, "WRITTEN", 7, PAGE + 200);
	printf("; written to the file: the mapping reads \"%.7s\"\n", m + PAGE + 200);

	// Another mapping of it, by another descriptor, and one of its second page alone.
	int other = open(path, O_RDWR);
	char *second = mmap(NULL, 3 * PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, other, 0);
	char *part = mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, PAGE);
	memcpy(second + 300, "SECOND", 6);
	memcpy(part + 400, "PART", 4);
	printf("a second mapping: %s, it reads \"%.6s\"; written through it, the first reads \"%.6s\"; a mapping of the second page alone: %s, written through, the first reads \"%.4s\"\n", mapped(second),
	       second + 100, m + 300, mapped(part), m + PAGE + 400);

	// A copy of the process shares it.
	fflush(stdout);
	pid_t child = fork();
	if (child == 0) {
		memcpy(m + 500, "CHILD", 5);
		_exit(0);
	}
	waitpid(child, NULL, 0);
	printf("written by a copy of the process: the mapping reads \"%.5s\", the file \"%s\"\n", m + 500, at(fd, 500, 5));

	// msync.
	printf("msync, waiting: %s; not waiting: %s; both: %s; neither: %s", said(msync(m, 3 * PAGE, MS_SYNC)), said(msync(m, 3 * PAGE, MS_ASYNC)), said(msync(m, 3 * PAGE, MS_SYNC | MS_ASYNC)), said(msync(m, 3 * PAGE, 0)));
	printf("; to let go what is held: %s; of part of a page: %s", said(msync(m, PAGE, MS_INVALIDATE)), said(msync(m + 100, 50, MS_SYNC)));
	char *alone = mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
	printf("; of memory that is no file's: %s", said(msync(alone, PAGE, MS_SYNC)));
	munmap(alone, PAGE);
	printf("; of what is not mapped: %s\n", said(msync(alone, PAGE, MS_SYNC)));

	// What a mapping may be.
	int readonly = open(path, O_RDONLY);
	char *looked = mmap(NULL, PAGE, PROT_READ, MAP_SHARED, readonly, 0);
	printf("of a file opened to read only, shared and to write: %s; to read: %s, it reads \"%.6s\"", mapped(mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, readonly, 0)), mapped(looked), looked + 100);
	printf("; made writable after: %s", said(mprotect(looked, PAGE, PROT_READ | PROT_WRITE)));
	char *own = mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_PRIVATE, readonly, 0);
	if (own != MAP_FAILED)
		memcpy(own + 100, "MYSELF", 6);
	printf("; for oneself and to write: %s, written through, the file reads \"%s\"\n", mapped(own), at(fd, 100, 6));
	printf("from the middle of a page: %s; of no length: %s; of a descriptor that is none: %s\n", mapped(mmap(NULL, PAGE, PROT_READ, MAP_SHARED, fd, 100)), mapped(mmap(NULL, 0, PROT_READ, MAP_SHARED, fd, 0)),
	       mapped(mmap(NULL, PAGE, PROT_READ, MAP_SHARED, 99, 0)));

	// A file shorter than its mapping's pages, and one that grows under its mapping.
	char spath[256];
	snprintf(spath, sizeof spath, "%s/mapfile.XXXXXX", dir && *dir ? dir : "/tmp");
	int small = mkstemp(spath);
	write(small, "short", 5);
	char *s = mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, small, 0);
	struct stat st;
	printf("a file of 5 bytes mapped: %s, it reads \"%.5s\", then %d", mapped(s), s, s[5]);
	memcpy(s + 10, "beyond", 6);
	fstat(small, &st);
	printf("; written past its end in the page: the file is of %lld", (long long)st.st_size);
	ftruncate(small, 20);
	printf("; grown to 20, it reads past 10 \"%s\"", at(small, 10, 6));
	pwrite(small, "grown", 5, 30);
	fstat(small, &st);
	printf("; written at 30, of %lld, and the mapping reads \"%.5s\"\n", (long long)st.st_size, s + 30);

	// A mapping for oneself is the file as it was when it was mapped.
	char *before = mmap(NULL, PAGE, PROT_READ, MAP_PRIVATE, fd, 2 * PAGE);
	m[2 * PAGE] = 'Z';
	pwrite(fd, "W", 1, 2 * PAGE + 1);
	printf("mapped for oneself, then written through the shared mapping and to the file: it reads \"%.2s\"\n", before);

	// The file's time of change is not moved by a write through the mapping, nor by a read of
	// the file after it. (msync moves it, most times: three runs of four on the measuring Mac.)
	struct stat was, now;
	sleep(1);
	m[700] = 't';
	fstat(fd, &was);
	char one;
	pread(fd, &one, 1, 700);
	fstat(fd, &now);
	printf("after a write through the mapping the file reads '%c'; its time of change moved: %d", one, was.st_mtimespec.tv_sec != now.st_mtimespec.tv_sec || was.st_mtimespec.tv_nsec != now.st_mtimespec.tv_nsec);
	printf("\n");

	// A copy of the file has what was written through the mapping.
	char copy[300];
	snprintf(copy, sizeof copy, "%s.copy", path);
	m[800] = 'Q';
	int cloned = clonefile(path, copy, 0);
	int cfd = open(copy, O_RDONLY);
	printf("the file copied (clonefile) after a write through the mapping: %s, the copy reads \"%s\"\n", said(cloned), at(cfd, 800, 1));
	close(cfd);
	unlink(copy);

	// What is refused.
	printf("the mapping of a file opened to read only made writable by the task's own call: %d\n", vm_protect(mach_task_self(), (vm_address_t)looked, PAGE, 0, VM_PROT_READ | VM_PROT_WRITE));
	printf("unmapped from inside a page: %s; of no length: %s; msync of no length: %s\n", said(munmap(before + 1, PAGE)), said(munmap(before, 0)), said(msync(m, 0, MS_SYNC)));
	int writeonly = open(path, O_WRONLY);
	printf("of a file opened to write only: shared, to read and write: %s; shared, to write: %s; for oneself, to read: %s\n", mapped(mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, writeonly, 0)),
	       mapped(mmap(NULL, PAGE, PROT_WRITE, MAP_SHARED, writeonly, 0)), mapped(mmap(NULL, PAGE, PROT_READ, MAP_PRIVATE, writeonly, 0)));
	const char *folder = dir && *dir ? dir : "/tmp"; // where the files are
	int ends[2], pair[2];
	pipe(ends);
	socketpair(AF_UNIX, SOCK_STREAM, 0, pair);
	printf("of a directory: %s, for oneself: %s; of a pipe: %s; of a socket: %s; of /dev/null: %s\n", mapped(mmap(NULL, PAGE, PROT_READ, MAP_SHARED, open(folder, O_RDONLY), 0)),
	       mapped(mmap(NULL, PAGE, PROT_READ, MAP_PRIVATE, open(folder, O_RDONLY), 0)), mapped(mmap(NULL, PAGE, PROT_READ, MAP_SHARED, ends[0], 0)), mapped(mmap(NULL, PAGE, PROT_READ, MAP_SHARED, pair[0], 0)),
	       mapped(mmap(NULL, PAGE, PROT_READ, MAP_SHARED, open("/dev/null", O_RDONLY), 0)));
	int system_file = open("/usr/lib/dyld", O_RDONLY);
	printf("of a file of the system's, shared and to write: %s; to read: %s\n", mapped(mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, system_file, 0)), mapped(mmap(NULL, PAGE, PROT_READ, MAP_SHARED, system_file, 0)));
	char *gone = mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
	munmap(gone, PAGE);
	printf("advice of a kind there is not: %s; of memory that is not mapped: %s; of the usual kind: %s\n", said(madvise(m, PAGE, 99)), said(madvise(gone, PAGE, MADV_NORMAL)), said(madvise(m, PAGE, MADV_NORMAL)));

	// Past the file's end: the page its last byte is in is there, with nothing in the rest of
	// it; a page past that one faults, until the file has grown to it; and when the file is cut,
	// by its descriptor, by its name or by being opened to be emptied, the pages it no longer
	// has fault again.
	signal(SIGBUS, fault);
	signal(SIGSEGV, fault);
	char lpath[256];
	snprintf(lpath, sizeof lpath, "%s/mapfile.XXXXXX", dir && *dir ? dir : "/tmp");
	int little = mkstemp(lpath);
	write(little, "short", 5);
	char *l = mmap(NULL, 3 * PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, little, 0);
	printf("a file of 5 bytes mapped for three pages: %s; its first page reads %s, its second %s, written %s", mapped(l), touched(l + 100, 0), touched(l + PAGE, 0), touched(l + PAGE, 1));
	ftruncate(little, 3 * PAGE);
	printf("; grown to three pages, the second reads %s", touched(l + PAGE, 0));
	l[PAGE] = 'k';
	ftruncate(little, 5);
	printf("; cut to 5 again, the second reads %s, the first past the end %s\n", touched(l + PAGE, 0), touched(l + 100, 0));
	ftruncate(little, 2 * PAGE);
	l[0] = 'X';
	printf("grown to two pages and cut to nothing by its name: %s", said(truncate(lpath, 0)));
	fstat(little, &st);
	printf(", of %lld, the mapping reads %s", (long long)st.st_size, touched(l, 0));
	ftruncate(little, 2 * PAGE);
	l[0] = 'X';
	int emptied = open(lpath, O_WRONLY | O_TRUNC);
	fstat(little, &st);
	printf("; grown again and opened to be emptied: %d, of %lld, the mapping reads %s, written %s", emptied >= 0, (long long)st.st_size, touched(l, 0), touched(l, 1));
	fstat(little, &st);
	printf(", of %lld then\n", (long long)st.st_size);

	// A small file mapped for far more than it has, and then grown far: the mapping is the
	// file's there too, both ways.
	const size_t far = 96u << 20, grown = 80u << 20, here = 70u << 20;
	char *f = mmap(NULL, far, PROT_READ | PROT_WRITE, MAP_SHARED, little, 0);
	printf("a file of nothing mapped for 96 MB: %s; at 70 MB it reads %s", mapped(f), touched(f + here, 0));
	ftruncate(little, grown);
	printf("; grown to 80 MB, %s", touched(f + here, 0));
	f[here] = 'F';
	printf("; written there, the file reads \"%s\"", at(little, here, 1));
	pwrite(little, "G", 1, here + PAGE);
	printf("; the file written a page on, the mapping reads %s", touched(f + here + PAGE, 0));
	printf("; past 80 MB %s\n", touched(f + grown, 0));
	ftruncate(little, 0);
	munmap(f, far);
	unlink(lpath);
	signal(SIGBUS, SIG_DFL);
	signal(SIGSEGV, SIG_DFL);

	// The mapping outlives its descriptor and the file's name.
	close(fd);
	unlink(path);
	memcpy(m + 600, "AFTER", 5);
	printf("its descriptor closed and its name taken away: the mapping is written, and the other descriptor reads \"%s\"\n", at(other, 600, 5));
	printf("unmapped: %s; again: %s\n", said(munmap(m, 3 * PAGE)), said(munmap(m, 3 * PAGE)));
	unlink(spath);
	return 0;
}
