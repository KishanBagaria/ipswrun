// What cp, tar and gzip ask about a file they copy, beyond what is in it: whether it is in
// quarantine, by a descriptor (Quarantine 82) and by a path (Quarantine 80), which is its
// attribute com.apple.quarantine, and its Finder info, which is its attribute
// com.apple.FinderInfo. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/xattr.h>
#include <unistd.h>

extern int __mac_syscall(const char *policy, int call, void *arg);
static uint64_t size82 = 0x180;
static void byDescriptor(const char *what, int fd) {
	unsigned char buf[512]; memset(buf, 0xAA, sizeof buf);
	static uint64_t room; room = size82;
	uint64_t args[8] = {(uint64_t)fd, (uint64_t)&room, (uint64_t)buf, 0xBBBBBBBBBBBBBBBB, 0xBBBBBBBBBBBBBBBB, 0xBBBBBBBBBBBBBBBB, 0xBBBBBBBBBBBBBBBB, 0xBBBBBBBBBBBBBBBB};
	errno = 0; int r = __mac_syscall("Quarantine", 82, args);
	int e = errno, n = 0; for (int i = 0; i < 512; i++) if (buf[i] != 0xAA) n = i + 1;
	printf("%s: %d errno %d; args changed: %d; buffer written up to %d:", what, r, e, args[0] != (uint64_t)fd || args[2] != (uint64_t)buf, n);
	printf(" [room %#llx -> %#llx] ", size82, room);
	for (int i = 0; i < n && i < 80; i++) printf("%c", buf[i] >= 32 && buf[i] < 127 ? buf[i] : '.');
	printf("\n");
}
static void descriptors(void) {
	char path[256]; snprintf(path, sizeof path, "%s/q82.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	int fd = open(path, O_CREAT | O_RDWR, 0600);
	byDescriptor("a file not in quarantine", fd);
	int dir = open(getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", O_RDONLY);
	byDescriptor("a directory", dir);
	byDescriptor("no descriptor", 99);
	int p[2]; pipe(p); byDescriptor("a pipe", p[0]);
	byDescriptor("a system file", open("/bin/ls", O_RDONLY));
	printf("setxattr: %d\n", fsetxattr(fd, "com.apple.quarantine", "0081;00000000;Probe;", 20, 0, 0));
	byDescriptor("a file in quarantine", fd);
	size82 = 8; byDescriptor("in quarantine, room for 8", fd);
	size82 = 0; byDescriptor("in quarantine, room for none", fd);
	size82 = 0x180; fremovexattr(fd, "com.apple.quarantine", 0); byDescriptor("out of quarantine again", fd);
	unlink(path);
}

static uint64_t size80 = 0x180;
static void byPath(const char *what, const char *path) {
	unsigned char buf[512]; memset(buf, 0xAA, sizeof buf);
	static uint64_t room; room = size80;
	uint64_t args[8] = {(uint64_t)path, (uint64_t)&room, (uint64_t)buf, 0xBBBBBBBBBBBBBBBB, 0xBBBBBBBBBBBBBBBB};
	errno = 0; int r = __mac_syscall("Quarantine", 80, args);
	int e = errno, n = 0; for (int i = 0; i < 512; i++) if (buf[i] != 0xAA) n = i + 1;
	printf("%s: %d errno %d; room %#llx -> %#llx; buffer written up to %d: ", what, r, e, size80, room, n);
	for (int i = 0; i < n && i < 80; i++) printf("%c", buf[i] >= 32 && buf[i] < 127 ? buf[i] : '.');
	printf("\n");
}
static void paths(void) {
	char path[256], link[256]; snprintf(path, sizeof path, "%s/q80.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	snprintf(link, sizeof link, "%s.link", path);
	int fd = open(path, O_CREAT | O_RDWR, 0600);
	symlink(path, link);
	byPath("a file not in quarantine", path);
	byPath("a directory", "/usr");
	byPath("nothing", "/no/such/file");
	byPath("a system file", "/bin/ls");
	byPath("no path", NULL);
	printf("setxattr: %d\n", fsetxattr(fd, "com.apple.quarantine", "0081;00000000;Probe;", 20, 0, 0));
	byPath("a file in quarantine", path);
	byPath("a link to it", link);
	size80 = 8; byPath("in quarantine, room for 8", path);
	unlink(path); unlink(link);
}

static char path[256];
static void show(const char *what) {
	struct attrlist al = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_FNDRINFO};
	struct { uint32_t len; unsigned char info[32]; } __attribute__((packed)) got; memset(&got, 0xAA, sizeof got);
	int r = getattrlist(path, &al, &got, sizeof got, 0);
	unsigned char x[64]; ssize_t n = getxattr(path, "com.apple.FinderInfo", x, sizeof x, 0, 0); int e = errno;
	
	printf("%s: getattrlist %d, %u bytes, first %02x %02x, last %02x; the attribute: %zd%s%s\n", what, r, got.len, got.info[0], got.info[1], got.info[31], n, n < 0 ? " " : "", n < 0 ? strerror(e) : "");
}
static void finderInfo(void) {
	snprintf(path, sizeof path, "%s/fi.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	close(open(path, O_CREAT | O_RDWR, 0600));
	show("new");
	struct attrlist al = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_FNDRINFO};
	unsigned char info[32] = {0}; 
	printf("set to nothing: %d\n", setattrlist(path, &al, info, 32, 0)); show("after");
	info[0] = 'T'; info[1] = 'E'; info[31] = 9;
	printf("set: %d\n", setattrlist(path, &al, info, 32, 0)); show("after");
	printf("set with 16 bytes: %d errno %d\n", setattrlist(path, &al, info, 16, 0), errno);
	memset(info, 0, 32);
	printf("set to nothing again: %d\n", setattrlist(path, &al, info, 32, 0)); show("after");
	info[5] = 1; printf("by the attribute: %d\n", setxattr(path, "com.apple.FinderInfo", info, 32, 0, 0)); show("after");
	printf("by the attribute, 16 bytes: %d errno %d\n", setxattr(path, "com.apple.FinderInfo", info, 16, 0, 0), errno);
	unlink(path);
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	printf("in quarantine, by a descriptor:\n");
	descriptors();
	printf("by a path:\n");
	paths();
	printf("Finder info:\n");
	finderInfo();
	return 0;
}
