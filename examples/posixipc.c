// Shared memory and semaphores by name (shm_open, sem_open): made, found again and taken away by
// their names, shared with a copy of the process, and what each refuses. It prints the same
// natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <semaphore.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

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

static const char *opened(sem_t *s) {
	static char text[4][64];
	static int next;
	char *t = text[next++ % 4];
	if (s == SEM_FAILED)
		snprintf(t, 64, "failed (%s)", strerror(errno));
	else
		snprintf(t, 64, "opened");
	return t;
}

static void nothing(int sig) { (void)sig; }

static const char *semaphore(sem_t *s) { return opened(s); }

static const char *mapping(void *p) {
	static char text[4][64];
	static int next;
	char *t = text[next++ % 4];
	if (p == MAP_FAILED)
		snprintf(t, 64, "failed (%s)", strerror(errno));
	else
		snprintf(t, 64, "mapped, off %ld", (long)((unsigned long)p & 16383));
	return t;
}

// rest is the rest of it: how long a name may be, who may open what by its mode, what is closed
// when another program is run, giving a size once, how it may be mapped, what the calls refuse,
// what the calls for files make of a semaphore, and the order waiters are let through in.
static void rest(void) {
	char n[64], sn[64], n31[40], n30[40], n32[40];
	snprintf(n, sizeof n, "/ipswrun.s2.%d", (int)getpid()); snprintf(sn, sizeof sn, "/ipswrun.m2.%d", (int)getpid());
	memset(n31, 'n', 31); n31[0] = '/'; n31[31] = 0; memset(n30, 'm', 30); n30[0] = '/'; n30[30] = 0; memset(n32, 'k', 32); n32[0] = '/'; n32[32] = 0;
	// 1
	sem_t *a = sem_open(n31, O_CREAT, 0600, 1); printf("sem name of 31: %s", semaphore(a)); if (a != SEM_FAILED) sem_unlink(n31);
	a = sem_open(n30, O_CREAT, 0600, 1); printf("; of 30: %s", semaphore(a)); if (a != SEM_FAILED) sem_unlink(n30);
	int f = shm_open(n31, O_CREAT | O_RDWR, 0600); printf("; shm name of 31: %s", said(f)); if (f >= 0) { shm_unlink(n31); close(f); }
	f = shm_open(n32, O_CREAT | O_RDWR, 0600); printf("; of 32: %s\n", said(f)); if (f >= 0) { shm_unlink(n32); close(f); }
	// 2
	a = sem_open(sn, O_CREAT, 0200, 1); printf("sem made 0200: %s; opened again: %s", semaphore(a), semaphore(sem_open(sn, 0))); sem_unlink(sn);
	a = sem_open(sn, O_CREAT, 0400, 1); printf("; made 0400, again: %s", semaphore(sem_open(sn, 0))); sem_unlink(sn);
	a = sem_open(sn, O_CREAT, 0, 1); printf("; made 0, again: %s, unlink %s\n", semaphore(sem_open(sn, 0)), said(sem_unlink(sn)));
	f = shm_open(n, O_CREAT | O_RDWR, 0400); printf("shm made 0400: %d; opened to write: %s; to read: %s", f >= 0, said(shm_open(n, O_RDWR, 0)), said(shm_open(n, O_RDONLY, 0) >= 0)); shm_unlink(n); close(f);
	f = shm_open(n, O_CREAT | O_RDWR, 0200); printf("; made 0200, to read: %s\n", said(shm_open(n, O_RDONLY, 0))); shm_unlink(n); close(f);
	// 3
	f = shm_open(n, O_CREAT | O_RDWR, 0600); a = sem_open(sn, O_CREAT, 0600, 1);
	printf("close-on-exec: shm %d, sem %d\n", fcntl(f, F_GETFD), fcntl((int)(long)a, F_GETFD));
	// 5
	printf("ftruncate 0: %s", said(ftruncate(f, 0))); printf(", then 20000: %s", said(ftruncate(f, 20000)));
	struct stat st; memset(&st, 0xff, sizeof st); fstat(f, &st);
	printf("\nfstat: mode %o nlink %d dev %d ino %lld mtime %ld blksize %d size %lld blocks %lld uid-mine %d gid-mine %d\n", (unsigned)st.st_mode, (int)st.st_nlink, (int)st.st_dev, (long long)st.st_ino, (long)st.st_mtime, (int)st.st_blksize, (long long)st.st_size, (long long)st.st_blocks, st.st_uid == getuid(), st.st_gid == getgid());
	shm_unlink(n); close(f);
	f = shm_open(n, O_CREAT | O_RDWR, 0600); printf("ftruncate 20000: %s, the same again: %s", said(ftruncate(f, 20000)), said(ftruncate(f, 20000)));
	printf("\n");
	// 6
	printf("MAP_PRIVATE: %s; offset 100: %s; offset 16384 len 100: %s; beyond: %s; length 0: %s\n", mapping(mmap(0, 100, PROT_READ, MAP_PRIVATE, f, 0)), mapping(mmap(0, 100, PROT_READ, MAP_SHARED, f, 100)), mapping(mmap(0, 100, PROT_READ, MAP_SHARED, f, 16384)), mapping(mmap(0, 100, PROT_READ, MAP_SHARED, f, 65536)), mapping(mmap(0, 0, PROT_READ, MAP_SHARED, f, 0)));
	// 4
	int ro = shm_open(n, O_RDONLY, 0); void *p = mmap(0, 16384, PROT_READ, MAP_SHARED, ro, 0);
	printf("mprotect to write of a read-only mapping: %s\n", said(mprotect(p, 16384, PROT_READ | PROT_WRITE)));
	// 7
	printf("access mode 3: %s", said(shm_open(n, 3, 0))); printf("; O_TRUNC of one with a size: %s", said(shm_open(n, O_RDWR | O_TRUNC, 0))); printf("; an empty name: %s\n", said(shm_open("", O_CREAT | O_RDWR, 0600)));
	// 8
	printf("shm_unlink empty: %s; of 40: %s; ", said(shm_unlink("")), said(shm_unlink("/kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk")));
	printf("sem_unlink empty: %s; of 40: %s\n", said(sem_unlink("")), said(sem_unlink("/kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk")));
	// 9
	printf("sem_open existing with value 40000: %s; with O_EXCL too: %s; new with 40000: %s; with 32767: ", semaphore(sem_open(sn, O_CREAT, 0600, 40000)), semaphore(sem_open(sn, O_CREAT | O_EXCL, 0600, 40000)), semaphore(sem_open(n30, O_CREAT, 0600, 40000)));
	sem_t *m = sem_open(n30, O_CREAT, 0600, 32767); printf("%s, post %s\n", semaphore(m), m == SEM_FAILED ? "-" : said(sem_post(m))); sem_unlink(n30);
	// 10
	int d = (int)(long)a; char c = 0;
	printf("on a semaphore's descriptor: read %s", said(read(d, &c, 1))); printf(", write %s", said(write(d, &c, 1))); printf(", fstat %s", said(fstat(d, &st))); printf(", ftruncate %s", said(ftruncate(d, 0))); printf(", mmap %s", mapping(mmap(0, 16384, PROT_READ, MAP_SHARED, d, 0))); printf(", lseek %s", said(lseek(d, 0, 0))); printf(", close %s\n", said(close(d)));
	// 11
	printf("pread of shm: %s", said(pread(f, &c, 1, 0))); printf("; pwrite: %s\n", said(pwrite(f, &c, 1, 0)));
	sem_unlink(sn); shm_unlink(n);
	// 14
	sem_t *q = sem_open(sn, O_CREAT, 0600, 0); int out[2]; pipe(out); fflush(stdout);
	for (int i = 0; i < 3; i++) { if (fork() == 0) { close(out[0]); sem_wait(q); char ch = '1' + i; write(out[1], &ch, 1); _exit(0); } usleep(100000); }
	close(out[1]); char order[4] = "";
	for (int i = 0; i < 3; i++) { sem_post(q); read(out[0], order + i, 1); }
	while (wait(NULL) > 0);
	printf("three waiters, started in order, let through: %s\n", order);
	sem_unlink(sn);
}

int main(void) {
	char name[64], sname[64], longname[64];
	snprintf(name, sizeof name, "/ipswrun.shm.%d", (int)getpid());
	snprintf(sname, sizeof sname, "/ipswrun.sem.%d", (int)getpid());
	memset(longname, 'n', 40);
	longname[0] = '/';
	longname[40] = 0;

	// Shared memory.
	printf("shared memory that is not there: %s", said(shm_open(name, O_RDWR, 0)));
	int fd = shm_open(name, O_RDWR | O_CREAT | O_EXCL, 0600);
	printf("; made: %d; made again, to be alone: %s; a name of 40: %s\n", fd >= 0, said(shm_open(name, O_RDWR | O_CREAT | O_EXCL, 0600)), said(shm_open(longname, O_RDWR | O_CREAT, 0600)));
	struct stat st;
	memset(&st, 0, sizeof st);
	printf("its size at first: %s %lld", said(fstat(fd, &st)), (long long)st.st_size);
	void *early = mmap(NULL, 16384, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	printf("; mapped before it has one: %s", early == MAP_FAILED ? strerror(errno) : "mapped");
	printf("; set to 20000: %s", said(ftruncate(fd, 20000)));
	fstat(fd, &st);
	printf(", it says %lld, mode %o, the user's %d", (long long)st.st_size, (unsigned)st.st_mode & 07777, st.st_uid == getuid());
	printf("; set again: %s\n", said(ftruncate(fd, 40000)));
	char byte = 'x';
	printf("read: %s", said(read(fd, &byte, 1)));
	printf("; written: %s", said(write(fd, &byte, 1)));
	printf("; lseek: %s\n", said(lseek(fd, 0, SEEK_SET)));
	char *mem = mmap(NULL, 20000, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	printf("mapped: %d; zero at first: %d", mem != MAP_FAILED, mem != MAP_FAILED && mem[0] == 0 && mem[19999] == 0);
	int again = shm_open(name, O_RDWR, 0);
	char *other = mmap(NULL, 20000, PROT_READ | PROT_WRITE, MAP_SHARED, again, 0);
	strcpy(mem, "written through the first");
	printf("; opened again by its name and mapped: %d, it reads \"%s\"\n", other != MAP_FAILED, other != MAP_FAILED ? other : "");
	int readonly = shm_open(name, O_RDONLY, 0);
	void *denied = mmap(NULL, 20000, PROT_READ | PROT_WRITE, MAP_SHARED, readonly, 0);
	void *allowed = mmap(NULL, 20000, PROT_READ, MAP_SHARED, readonly, 0);
	printf("opened to read only: %d; mapped to write: %s; to read: %d\n", readonly >= 0, denied == MAP_FAILED ? strerror(errno) : "mapped", allowed != MAP_FAILED);
	fflush(stdout);
	pid_t child = fork();
	if (child == 0) {
		strcpy(mem, "written by a copy of the process");
		_exit(0);
	}
	waitpid(child, NULL, 0);
	printf("after a copy of the process wrote to it: \"%s\"\n", mem);
	printf("its name taken away: %s; again: %s; opened by it: %s", said(shm_unlink(name)), said(shm_unlink(name)), said(shm_open(name, O_RDWR, 0)));
	strcpy(other, "still shared");
	printf("; the mappings still share: \"%s\"\n", mem);
	int fresh = shm_open(name, O_RDWR | O_CREAT, 0600);
	fstat(fresh, &st);
	printf("made anew by the same name: %d, of size %lld\n", fresh >= 0, (long long)st.st_size);
	shm_unlink(name);

	// Semaphores.
	sem_t *none = sem_open(sname, 0);
	printf("a semaphore that is not there: %s", opened(none));
	sem_t *s = sem_open(sname, O_CREAT | O_EXCL, 0600, 2);
	printf("; made with 2: %s", opened(s));
	printf("; made again, to be alone: %s", opened(sem_open(sname, O_CREAT | O_EXCL, 0600, 2)));
	printf("; a name of 40: %s\n", opened(sem_open(longname, O_CREAT, 0600, 1)));
	printf("taken: %s, %s; tried a third time: %s", said(sem_wait(s)), said(sem_wait(s)), said(sem_trywait(s)));
	printf("; given back: %s; tried: %s\n", said(sem_post(s)), said(sem_trywait(s)));
	int value = -5;
	printf("its value asked: %s", said(sem_getvalue(s, &value)));
	sem_t unnamed;
	printf("; one without a name: %s\n", said(sem_init(&unnamed, 1, 1)));
	fflush(stdout);
	// A copy of the process waits for it, and is let through by the first.
	int told[2];
	pipe(told);
	child = fork();
	if (child == 0) {
		close(told[0]);
		write(told[1], "w", 1);
		int r = sem_wait(s);
		_exit(r == 0 ? 7 : 8);
	}
	close(told[1]);
	char c;
	read(told[0], &c, 1);
	usleep(100000);
	int status = 0;
	printf("a copy of the process waits for it: still waiting %d", waitpid(child, &status, WNOHANG) == 0);
	sem_post(s);
	waitpid(child, &status, 0);
	printf("; given, the copy ends with %d\n", WEXITSTATUS(status));
	// A wait broken off by a signal.
	fflush(stdout);
	child = fork();
	if (child == 0) {
		struct sigaction act;
		memset(&act, 0, sizeof act);
		act.sa_handler = nothing;
		sigaction(SIGUSR1, &act, NULL);
		int r = sem_wait(s);
		_exit(r < 0 && errno == EINTR ? 9 : 10);
	}
	usleep(100000);
	kill(child, SIGUSR1);
	waitpid(child, &status, 0);
	printf("a wait broken off by a signal that is caught: the copy ends with %d\n", WEXITSTATUS(status));
	sem_t *second = sem_open(sname, 0);
	printf("opened again by its name: %s; the same one %d", opened(second), second == s);
	printf("; its name taken away: %s; again: %s; opened by it: %s", said(sem_unlink(sname)), said(sem_unlink(sname)), opened(sem_open(sname, 0)));
	printf("; still given and taken: %s %s\n", said(sem_post(s)), said(sem_trywait(second)));
	printf("closed: %s; closed again: %s; given after that: %s\n", said(sem_close(s)), said(sem_close(s)), said(sem_post(s)));
	fflush(stdout);
	rest();
	return 0;
}
