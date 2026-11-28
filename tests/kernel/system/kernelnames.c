// What a kernel says it is: by uname, which asks for all of it at once and has nothing if one
// part is not to be had, and by each name. It says how the answers hang together, and not
// what they are, which is each machine's own. It prints the same natively on a Mac as under
// ipswrun.
#include <ctype.h>
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/sysctl.h>
#include <sys/utsname.h>

// named reads a name that is text.
static const char *named(const char *name) {
	static char text[4][512];
	static int next;
	char *t = text[next++ % 4];
	size_t length = 512;
	if (sysctlbyname(name, t, &length, NULL, 0))
		snprintf(t, 512, "(%s)", strerror(errno));
	return t;
}

int main(void) {
	struct utsname u;
	memset(&u, 0, sizeof u);
	int r = uname(&u);
	printf("uname: %s; the system is %s; the node, the release, the version and the machine are each said: %d %d %d %d\n", r ? strerror(errno) : "answered", u.sysname, u.nodename[0] != 0, u.release[0] != 0, u.version[0] != 0,
	       u.machine[0] != 0);

	const char *type = named("kern.ostype"), *release = named("kern.osrelease"), *version = named("kern.version");
	printf("by name: the system is %s; the release is what uname says: %d, and is numbers with dots: %d; the version is what uname says: %d\n", type, !strcmp(release, u.release),
	       isdigit((unsigned char)release[0]) && strspn(release, "0123456789.") == strlen(release) && strchr(release, '.') != NULL, !strcmp(version, u.version));

	char start[600];
	snprintf(start, sizeof start, "Darwin Kernel Version %s: ", release);
	const char *build = strstr(version, "; root:xnu-"), *kind = strrchr(version, '/');
	printf("the version begins with the system's name and the release: %d; says who built what: %d; ends in the kind of build it is: %d; is one line: %d\n", !strncmp(version, start, strlen(start)), build != NULL,
	       kind != NULL && !strncmp(kind, "/RELEASE_ARM64_", 15) && kind[15] != 0, strchr(version, '\n') == NULL);

	size_t length = 4;
	char small[8] = "#######";
	int short_ = sysctlbyname("kern.version", small, &length, NULL, 0);
	printf("into four bytes: %s; it then says %zu, and has \"%.8s\"\n", short_ ? strerror(errno) : "it fits", length, small);
	unsigned number[2] = {7, 7};
	length = 2;
	short_ = sysctlbyname("hw.pagesize", number, &length, NULL, 0);
	printf("a number of four bytes into two: %s; it then says %zu, and has %u\n", short_ ? strerror(errno) : "it fits", length, number[0]);
	length = 8;
	short_ = sysctlbyname("hw.pagesize", number, &length, NULL, 0);
	printf("into eight: %s; it then says %zu, and the second four are %s\n", short_ ? strerror(errno) : "it fits", length, number[1] == 7 ? "as they were" : "written");
	length = 0;
	sysctlbyname("kern.version", NULL, &length, NULL, 0);
	printf("how long it is, asked first: one more than its letters: %d\n", length == strlen(version) + 1);
	return 0;
}
