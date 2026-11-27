// The standard input a program was started with, where it is not a terminal: nothing, a file or
// a pipe. Whether poll and select find it ready to be read, and what a read of it gives. Under
// ipswrun the run's own standard input is the host's, and is asked of the host. It prints the
// same natively on a Mac as under ipswrun, for the same standard input.
#include <errno.h>
#include <poll.h>
#include <stdio.h>
#include <string.h>
#include <sys/select.h>
#include <sys/stat.h>
#include <unistd.h>

int main(void) {
	struct stat st;
	fstat(0, &st);
	printf("standard input is %s; a terminal: %d\n", S_ISREG(st.st_mode) ? "a file" : S_ISFIFO(st.st_mode) ? "a pipe" : S_ISCHR(st.st_mode) ? "a device" : "something else", isatty(0));
	struct pollfd asked = {0, POLLIN | POLLOUT, 0};
	int found = poll(&asked, 1, 0);
	printf("poll finds %d, with %#x", found, asked.revents);
	fd_set in, out;
	FD_ZERO(&in);
	FD_ZERO(&out);
	FD_SET(0, &in);
	FD_SET(0, &out);
	struct timeval none = {0, 0};
	found = select(1, &in, &out, NULL, &none);
	printf("; select finds %d%s%s\n", found, found > 0 && FD_ISSET(0, &in) ? ", to read" : "", found > 0 && FD_ISSET(0, &out) ? ", to write" : "");
	char bytes[64];
	ssize_t n = read(0, bytes, sizeof bytes);
	printf("a read gives %zd", n);
	asked.revents = 0;
	asked.events = POLLIN;
	found = poll(&asked, 1, 0);
	printf("; then poll finds %d, with %#x", found, asked.revents);
	n = read(0, bytes, sizeof bytes);
	printf(", and a read %zd\n", n);
	return 0;
}
