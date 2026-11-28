// The release's own /bin/sh, interactive, at a pseudo-terminal it leads its session with: what
// is typed at it, a line or a key at a time, and every byte it shows back, with a command, a
// pipeline, the keys that interrupt and suspend, jobs, kill, a job behind and wait. What a
// release's shell shows is that release's; on the release a Mac runs, it is what that Mac's
// shows.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/wait.h>
#include <termios.h>
#include <unistd.h>
static int m;
static void show(const char *bytes, long n) {
	for (long i = 0; i < n; i++) {
		unsigned char c = (unsigned char)bytes[i];
		if (c == '\n') printf("\\n\n   ");
		else if (c == '\r') printf("\\r");
		else if (c < 32 || c == 127) printf("^%c", c ^ 64);
		else putchar(c);
	}
}
// until reads what the shell shows until it ends with the prompt, or for five seconds.
static void until(const char *prompt) {
	char seen[8192]; long n = 0; size_t len = strlen(prompt);
	for (int waited = 0; waited < 100;) {
		struct pollfd p = {m, POLLIN, 0};
		if (poll(&p, 1, 50) <= 0) { waited++; continue; }
		long r = read(m, seen + n, sizeof seen - 1 - (size_t)n);
		if (r <= 0) break;
		n += r;
		if ((size_t)n >= len && !memcmp(seen + n - len, prompt, len)) break;
	}
	// The number of a process the shell puts behind ("[2] 4253") is whatever it happens to be.
	for (long i = 0; i + 4 < n; i++)
		if (seen[i] == '[' && seen[i + 1] >= '1' && seen[i + 1] <= '9' && seen[i + 2] == ']' && seen[i + 3] == ' ' && seen[i + 4] >= '1' && seen[i + 4] <= '9') {
			long end = i + 4;
			while (end < n && seen[end] >= '0' && seen[end] <= '9')
				end++;
			memmove(seen + i + 7, seen + end, (size_t)(n - end));
			memcpy(seen + i + 4, "pid", 3);
			n -= end - (i + 7);
		}
	printf("   "); show(seen, n); printf("\n");
}
static void type(const char *text, const char *prompt) {
	if (text[0] < 32) usleep(400000); // a key: once the shell has put what it runs in front
	printf("typed: "); show(text, (long)strlen(text)); printf("\n");
	write(m, text, strlen(text));
	until(prompt);
}
int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	m = posix_openpt(O_RDWR | O_NOCTTY); grantpt(m); unlockpt(m);
	char name[128]; strlcpy(name, ptsname(m), sizeof name);
	struct winsize size = {24, 80, 0, 0}; ioctl(m, TIOCSWINSZ, &size);
	pid_t child = fork();
	if (child == 0) {
		close(m); setsid();
		for (int sig = 1; sig < 32; sig++) signal(sig, SIG_DFL); // whatever started this may have had some ignored
		sigset_t none; sigemptyset(&none); sigprocmask(SIG_SETMASK, &none, NULL);
		int s = open(name, O_RDWR); ioctl(s, TIOCSCTTY, 0);
		dup2(s, 0); dup2(s, 1); dup2(s, 2); if (s > 2) close(s);
		char *env[] = {"PS1=@@ ", "PATH=/usr/bin:/bin", "TERM=dumb", "HOME=/tmp", "MallocNanoZone=0", NULL};
		char *args[] = {"sh", "-i", NULL};
		execve("/bin/sh", args, env);
		_exit(127);
	}
	until("@@ ");
	type("echo hello\n", "@@ ");
	type("echo one two | tr a-z A-Z\n", "@@ ");
	type("sleep 30\n", "\n");
	type("\x03", "@@ ");
	type("sleep 30\n", "\n");
	type("\x1a", "@@ ");
	type("jobs\n", "@@ ");
	type("kill %1\n", "@@ ");
	type("sleep 1 &\n", "@@ ");
	type("wait\n", "@@ ");
	type("exit\n", "@@@@");
	int status = 0; waitpid(child, &status, 0);
	printf("sh ended: %d, with %d\n", WIFEXITED(status), WEXITSTATUS(status));
	return 0;
}
