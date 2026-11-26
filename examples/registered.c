// The three ports a task keeps for whoever asks (mach_ports_register and mach_ports_lookup), as
// a shell keeps the port it found launchd by: what is kept is given back, to the task itself
// and to a copy of it, as rights to send. It prints the same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <stdio.h>
#include <sys/wait.h>
#include <unistd.h>

extern mach_port_t bootstrap_port; // of <servers/bootstrap.h>, which an iPhone's SDK leaves out

static int held(mach_port_t name) {
	mach_port_urefs_t n = 0;
	return mach_port_get_refs(mach_task_self(), name, MACH_PORT_RIGHT_SEND, &n) ? -1 : (int)n;
}

// kept prints what the task keeps, against the two ports it may have been given.
static void kept(const char *when, mach_port_t first, mach_port_t second) {
	mach_port_array_t ports = NULL;
	mach_msg_type_number_t count = 99;
	kern_return_t r = mach_ports_lookup(mach_task_self(), &ports, &count);
	printf("%s: %d, %u of them", when, r, count);
	for (unsigned i = 0; r == 0 && i < count; i++)
		printf("; %s", ports[i] == MACH_PORT_NULL ? "none" : ports[i] == first ? "the first given" : ports[i] == second ? "the second given" : "another");
	printf("\n");
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	mach_port_t task = mach_task_self(), own = MACH_PORT_NULL, only = MACH_PORT_NULL;
	mach_port_allocate(task, MACH_PORT_RIGHT_RECEIVE, &own);
	mach_port_insert_right(task, own, own, MACH_MSG_TYPE_MAKE_SEND);
	mach_port_allocate(task, MACH_PORT_RIGHT_RECEIVE, &only);

	mach_port_t none[3] = {MACH_PORT_NULL, MACH_PORT_NULL, MACH_PORT_NULL}, two[3] = {bootstrap_port, own, MACH_PORT_NULL}, four[4] = {own, own, own, own}, bad[1] = {only};
	printf("keeping none: %d\n", mach_ports_register(task, none, 3));
	kept("then asked what is kept", bootstrap_port, own);
	int before = held(own);
	printf("keeping the port launchd is found by, and one of its own: %d; its own is held as often as before %d\n", mach_ports_register(task, two, 2), held(own) == before);
	kept("asked what is kept", bootstrap_port, own);
	printf("its own is then held once more %d\n", held(own) == before + 1);
	printf("keeping four: %d; keeping one it can only receive from: %#x\n", mach_ports_register(task, four, 4), mach_ports_register(task, bad, 1));
	kept("after those", bootstrap_port, own);

	pid_t pid = fork();
	if (pid == 0) {
		mach_port_array_t ports = NULL;
		mach_msg_type_number_t count = 0;
		kern_return_t r = mach_ports_lookup(mach_task_self(), &ports, &count);
		printf("a copy of the process, asked what is kept: %d, %u of them; the first is the port it finds launchd by %d; it has the second %d, which is no name the copy had %d; the third none %d\n", r, count,
		       r == 0 && ports[0] == bootstrap_port, r == 0 && ports[1] != MACH_PORT_NULL, r == 0 && ports[1] != own, r == 0 && ports[2] == MACH_PORT_NULL);
		_exit(0);
	}
	waitpid(pid, NULL, 0);
	printf("keeping one alone: %d\n", mach_ports_register(task, &own, 1));
	kept("then", bootstrap_port, own);
	printf("asked of the host: %d\n", mach_ports_register(mach_host_self(), none, 3));
	return 0;
}
