// A lock on a whole file (flock) and locks on parts of one (fcntl) are kept in one list: each
// stands against the other, a whole-file lock is its open file's just as an open file's lock
// on a part is, and unlocking the whole file gives up the parts. It prints the same natively
// on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <sys/wait.h>
#include <unistd.h>

// The requests no SDK names: an open file's lock, set and asked about.
enum { setOpenLock = 90, getOpenLock = 92 };

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

// part asks for a lock of a kind on some bytes of a file from its start: 0 bytes is to its end.
static const char *part(int fd, int request, short kind, off_t start, off_t length) {
	struct flock f = {.l_type = kind, .l_whence = SEEK_SET, .l_start = start, .l_len = length};
	return said(fcntl(fd, request, &f));
}

// held says what stands against a lock to write some bytes.
static const char *held(int fd, int request, off_t start, off_t length) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	struct flock f = {.l_type = F_WRLCK, .l_whence = SEEK_SET, .l_start = start, .l_len = length};
	if (fcntl(fd, request, &f))
		return strerror(errno);
	if (f.l_type == F_UNLCK)
		snprintf(t, 96, "nothing");
	else if (f.l_pid == getpid())
		snprintf(t, 96, "a lock to %s of %lld bytes from %lld, of this process", f.l_type == F_WRLCK ? "write" : "read", f.l_len, f.l_start);
	else
		snprintf(t, 96, "a lock to %s of %lld bytes from %lld, of process %d", f.l_type == F_WRLCK ? "write" : "read", f.l_len, f.l_start, f.l_pid);
	return t;
}

static const char *whole(int fd, int how) { return said(flock(fd, how | LOCK_NB)); }

static void rung(int sig) { (void)sig; }

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char path[512];
	snprintf(path, sizeof path, "%s/locklist.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	int a = open(path, O_CREAT | O_RDWR, 0644), b = open(path, O_RDWR), c = open(path, O_RDWR);
	write(a, "0123456789", 10);
	const char *r1, *r2, *r3, *r4, *r5;

	// A whole-file lock against a process's own locks and another open file's.
	r1 = whole(a, LOCK_EX);
	r2 = part(b, F_SETLK, F_WRLCK, 0, 1);
	r3 = part(b, setOpenLock, F_RDLCK, 5, 1);
	r4 = part(a, F_SETLK, F_RDLCK, 0, 1);
	printf("one open file locks the whole file to write: %s; the process then locks a byte through another: %s; the other open file locks one to read: %s; the process through the first itself: %s\n", r1, r2, r3, r4);
	printf("what stands against the process writing a byte: %s; against the other open file: %s; against the first: %s\n", held(b, F_GETLK, 3, 1), held(b, getOpenLock, 3, 1), held(a, getOpenLock, 3, 1));

	// The open file that holds the whole file takes a part of it another way: it is one holder.
	r1 = part(a, setOpenLock, F_RDLCK, 4, 2);
	r2 = whole(b, LOCK_SH);
	r3 = part(b, setOpenLock, F_RDLCK, 4, 2);
	r4 = part(b, setOpenLock, F_RDLCK, 3, 2);
	printf("the first makes bytes 4 and 5 of its own a lock to read: %s; the other open file then shares the whole: %s, reads those two: %s, reads from one before: %s\n", r1, r2, r3, r4);
	printf("what stands against the other writing the whole: %s\n", held(b, getOpenLock, 0, 0));
	r1 = said(flock(a, LOCK_UN));
	r2 = held(c, getOpenLock, 0, 0);
	r3 = whole(c, LOCK_EX);
	r4 = said(flock(b, LOCK_UN));
	r5 = whole(c, LOCK_EX);
	printf("the first unlocks the whole file: %s; against a third writing the whole then stands %s; it locks the whole: %s; the other has unlocked the whole: %s, and then: %s\n", r1, r2, r3, r4, r5);
	flock(c, LOCK_UN);

	// A process's own lock stands against a whole-file lock, of any open file.
	r1 = part(a, F_SETLK, F_RDLCK, 7, 1);
	r2 = whole(b, LOCK_EX);
	r3 = whole(a, LOCK_EX);
	r4 = whole(a, LOCK_SH);
	r5 = said(flock(a, LOCK_UN));
	printf("the process locks a byte to read: %s; an open file locks the whole to write: %s; the one it was locked through: %s, and shares it: %s; unlocks the whole: %s", r1, r2, r3, r4, r5);
	printf("; the process's byte is then held by %s\n", held(b, getOpenLock, 7, 1));
	part(a, F_SETLK, F_UNLCK, 0, 0);

	// One that shares the whole and waits to write it gives up its share before it waits; one
	// that does not wait keeps it.
	flock(a, LOCK_SH);
	flock(b, LOCK_SH);
	r1 = whole(a, LOCK_EX);
	r2 = whole(c, LOCK_EX);
	flock(b, LOCK_UN);
	r3 = whole(c, LOCK_EX);
	printf("two share the whole; the first, to write it and not waiting: %s; a third to write: %s; the second gone, the third: %s\n", r1, r2, r3);
	flock(b, LOCK_SH);
	struct sigaction act = {.sa_handler = rung};
	sigaction(SIGALRM, &act, NULL);
	alarm(1);
	r1 = said(flock(a, LOCK_EX));
	flock(b, LOCK_UN);
	r2 = whole(c, LOCK_EX);
	printf("shared again by two; the first waits to write it until a signal: %s; the second gone, a third to write: %s\n", r1, r2);
	flock(c, LOCK_UN);
	flock(a, LOCK_UN);

	// A duplicate and a copy of the process are the same open file; the lock goes with the last
	// descriptor of it. One opened only to read may lock to write.
	int same = dup(a);
	r1 = whole(a, LOCK_EX);
	r2 = whole(same, LOCK_EX);
	pid_t child = fork();
	if (child == 0) {
		_exit(flock(a, LOCK_UN) ? 1 : 0);
	}
	int status;
	waitpid(child, &status, 0);
	r3 = whole(b, LOCK_EX);
	printf("locked to write: %s; again through a duplicate: %s; a copy of the process has unlocked it: %d; another open file then: %s\n", r1, r2, WEXITSTATUS(status), r3);
	close(same);
	r1 = whole(a, LOCK_EX);
	close(b);
	r2 = whole(a, LOCK_EX);
	int reads = open(path, O_RDONLY);
	r3 = whole(reads, LOCK_EX);
	flock(a, LOCK_UN);
	r4 = whole(reads, LOCK_EX);
	r5 = part(a, F_SETLK, F_RDLCK, 0, 1);
	printf("the first while the other holds it: %s; the other closed: %s; one opened only to read, to write: %s; the first having unlocked: %s; the process then reads a byte: %s\n", r1, r2, r3, r4, r5);
	// Unlocking the whole file and an open file's own locks on parts; a lock to write made one
	// to share; both kinds asked at once.
	flock(reads, LOCK_UN);
	part(a, F_SETLK, F_UNLCK, 0, 0);
	r1 = part(a, setOpenLock, F_WRLCK, 0, 2);
	r2 = whole(a, LOCK_SH);
	r3 = said(flock(a, LOCK_UN));
	r4 = part(c, setOpenLock, F_WRLCK, 0, 2);
	r5 = part(a, setOpenLock, F_UNLCK, 0, 0);
	printf("an open file locks two bytes to write: %s, shares the whole: %s, unlocks the whole: %s; another then writes the two: %s; the first unlocks all its parts: %s", r1, r2, r3, r4, r5);
	printf("; and then: %s\n", part(c, setOpenLock, F_WRLCK, 0, 2));
	part(c, setOpenLock, F_UNLCK, 0, 0);
	r1 = whole(a, LOCK_EX);
	r2 = part(a, setOpenLock, F_UNLCK, 0, 0);
	r3 = whole(c, LOCK_SH);
	r4 = whole(a, LOCK_SH);
	r5 = whole(c, LOCK_SH);
	printf("locked whole to write: %s; its holder unlocks all its parts: %s; another shares the whole: %s; the holder makes it one to share: %s, and the other: %s", r1, r2, r3, r4, r5);
	printf("; to share and to write at once, by the first: %s\n", whole(a, LOCK_SH | LOCK_EX));
	flock(a, LOCK_UN);
	r1 = whole(c, LOCK_SH | LOCK_EX);
	r2 = whole(a, LOCK_SH);
	printf("by the other, alone: %s; the first then shares: %s\n", r1, r2);
	// Those that wait for the whole file get it one after the other in the order they came,
	// even two that would share it: each says when it has it and when it lets go.
	flock(a, LOCK_UN);
	flock(c, LOCK_UN);
	int says[2];
	pipe(says);
	static const int hows[2][3] = {{LOCK_SH, LOCK_EX, LOCK_SH}, {LOCK_SH, LOCK_SH, 0}};
	for (int round = 0; round < 2; round++) {
		flock(a, LOCK_EX);
		int waiters = round == 0 ? 3 : 2;
		for (int i = 0; i < waiters; i++) {
			if (fork() == 0) {
				close(says[0]);
				int f = open(path, O_RDWR);
				char in = '1' + i, out = 'a' + i;
				alarm(15);
				flock(f, hows[round][i]);
				write(says[1], &in, 1);
				usleep(400000);
				write(says[1], &out, 1);
				flock(f, LOCK_UN);
				_exit(0);
			}
			usleep(300000);
		}
		flock(a, LOCK_UN);
		while (wait(NULL) > 0)
			;
		char order[8] = {0};
		for (int n = 0; n < 2 * waiters; n += read(says[0], order + n, 2 * waiters - n))
			;
		printf("%s waiting for the whole file had it in this order: %s\n", round == 0 ? "one to share it, one to write it and one to share it" : "two to share it", order);
	}

	// A file and a directory of the system volume, and a device.
	int plist = open("/System/Library/CoreServices/SystemVersion.plist", O_RDONLY), again = open("/System/Library/CoreServices/SystemVersion.plist", O_RDONLY);
	int share = open("/usr/share", O_RDONLY), null = open("/dev/null", O_RDWR), null2 = open("/dev/null", O_RDWR);
	r1 = whole(plist, LOCK_EX);
	r2 = whole(again, LOCK_SH);
	close(plist);
	r3 = whole(again, LOCK_SH);
	r4 = whole(share, LOCK_EX);
	printf("a file of the system: %s; another open of it: %s; the first closed: %s; a directory of the system: %s", r1, r2, r3, r4);
	r1 = whole(null, LOCK_EX);
	r2 = whole(null2, LOCK_EX);
	printf("; /dev/null: %s, another open of it: %s\n", r1, r2);
	printf("a kind of whole-file lock there is not: %s; unlocking with no lock: %s; of no descriptor: %s\n", said(flock(a, 16)), said(flock(c, LOCK_UN)), said(flock(99, LOCK_EX)));
	unlink(path);
	return 0;
}
