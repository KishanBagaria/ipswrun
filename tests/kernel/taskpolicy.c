// What a task asks of how it is run (task_policy_set), as an editor says it is a program for
// the user and a daemon how soon its timers and its work may be: the kinds of program a task
// may say it is, the tiers it may ask for, and what it is told back (task_policy_get). It
// prints the same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <mach/task_policy.h>
#include <mach-o/dyld.h>
#include <stdio.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

static mach_port_t task;

// role says what the task is told when it says it is a kind of program, and what kind it is after.
static void role(const char *what, int kind) {
	task_category_policy_data_t is = {kind}, now = {-99};
	mach_msg_type_number_t count = TASK_CATEGORY_POLICY_COUNT;
	boolean_t usual = FALSE;
	kern_return_t set = task_policy_set(task, TASK_CATEGORY_POLICY, (task_policy_t)&is, TASK_CATEGORY_POLICY_COUNT);
	kern_return_t got = task_policy_get(task, TASK_CATEGORY_POLICY, (task_policy_t)&now, &count, &usual);
	printf(" %s (%d): %d, and it is then %d (%d);", what, kind, set, now.role, got);
}

// tiers says what the task is told when it asks for tiers of a kind, and what they are after.
static void tiers(const char *what, int flavor, int latency, int throughput) {
	struct task_qos_policy asks = {latency, throughput}, now = {-99, -99};
	mach_msg_type_number_t count = TASK_QOS_POLICY_COUNT;
	boolean_t usual = FALSE;
	kern_return_t set = task_policy_set(task, flavor, (task_policy_t)&asks, TASK_QOS_POLICY_COUNT);
	kern_return_t got = task_policy_get(task, flavor, (task_policy_t)&now, &count, &usual);
	printf(" %s: %d, and they are then %#x and %#x (%d);", what, set, now.task_latency_qos_tier, now.task_throughput_qos_tier, got);
}

// said is the kind of program a task is, and the first tier it asked for.
static void said(const char *who) {
	task_category_policy_data_t now = {-99};
	struct task_qos_policy tiers = {-99, -99};
	mach_msg_type_number_t one = TASK_CATEGORY_POLICY_COUNT, two = TASK_QOS_POLICY_COUNT;
	boolean_t usual = FALSE;
	task_policy_get(mach_task_self(), TASK_CATEGORY_POLICY, (task_policy_t)&now, &one, &usual);
	task_policy_get(mach_task_self(), TASK_BASE_QOS_POLICY, (task_policy_t)&tiers, &two, &usual);
	printf("%s: the kind of program %d, its first tier %#x\n", who, now.role, tiers.task_latency_qos_tier);
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	task = mach_task_self();
	if (argc > 1) {
		said(argv[1]);
		return 0;
	}
	task_category_policy_data_t now = {-99};
	mach_msg_type_number_t count = TASK_CATEGORY_POLICY_COUNT;
	boolean_t usual = FALSE;
	kern_return_t got = task_policy_get(task, TASK_CATEGORY_POLICY, (task_policy_t)&now, &count, &usual);
	printf("as it starts, the kind of program it is: %d (%d), %u word; asked what is usual: ", now.role, got, count);
	usual = TRUE;
	now.role = -99;
	got = task_policy_get(task, TASK_CATEGORY_POLICY, (task_policy_t)&now, &count, &usual);
	printf("%d (%d)\n", now.role, got);

	printf("saying it is:");
	role("one for the user, as an editor says", TASK_DEFAULT_APPLICATION);
	role("in front", TASK_FOREGROUND_APPLICATION);
	role("behind", TASK_BACKGROUND_APPLICATION);
	role("one that controls others", TASK_CONTROL_APPLICATION);
	role("the one that draws the screen", TASK_GRAPHICS_SERVER);
	role("throttled", TASK_THROTTLE_APPLICATION);
	role("one with no windows", TASK_NONUI_APPLICATION);
	role("of no kind", TASK_UNSPECIFIED);
	role("renicing itself", TASK_RENICED);
	role("of a kind there is not", 99);
	printf("\n");

	printf("asking for tiers:");
	tiers("the first of each, as a daemon asks", TASK_BASE_QOS_POLICY, LATENCY_QOS_TIER_0, THROUGHPUT_QOS_TIER_0);
	tiers("the last of each", TASK_BASE_QOS_POLICY, LATENCY_QOS_TIER_5, THROUGHPUT_QOS_TIER_5);
	tiers("none said", TASK_BASE_QOS_POLICY, LATENCY_QOS_TIER_UNSPECIFIED, THROUGHPUT_QOS_TIER_UNSPECIFIED);
	tiers("one past the last", TASK_BASE_QOS_POLICY, LATENCY_QOS_TIER_5 + 1, THROUGHPUT_QOS_TIER_5 + 1);
	tiers("each of the other's", TASK_BASE_QOS_POLICY, THROUGHPUT_QOS_TIER_0, LATENCY_QOS_TIER_0);
	tiers("plain numbers", TASK_BASE_QOS_POLICY, 1, 1);
	printf("\n");
	printf("to be over what the task asks for itself:");
	tiers("the first of each", TASK_OVERRIDE_QOS_POLICY, LATENCY_QOS_TIER_0, THROUGHPUT_QOS_TIER_0);
	tiers("the third of each", TASK_OVERRIDE_QOS_POLICY, LATENCY_QOS_TIER_2, THROUGHPUT_QOS_TIER_2);
	printf("\n");

	// What a copy of the process has of it, and the program a copy becomes.
	task_category_policy_data_t user = {TASK_DEFAULT_APPLICATION};
	struct task_qos_policy second = {LATENCY_QOS_TIER_1, THROUGHPUT_QOS_TIER_1};
	task_policy_set(task, TASK_CATEGORY_POLICY, (task_policy_t)&user, TASK_CATEGORY_POLICY_COUNT);
	task_policy_set(task, TASK_BASE_QOS_POLICY, (task_policy_t)&second, TASK_QOS_POLICY_COUNT);
	said("having said it is one for the user, with the second tier");
	pid_t pid = fork();
	if (pid == 0) {
		said("a copy of it");
		task_policy_set(mach_task_self(), TASK_CATEGORY_POLICY, (task_policy_t)&user, TASK_CATEGORY_POLICY_COUNT);
		task_policy_set(mach_task_self(), TASK_BASE_QOS_POLICY, (task_policy_t)&second, TASK_QOS_POLICY_COUNT);
		char self[1024];
		uint32_t size = sizeof self;
		_NSGetExecutablePath(self, &size);
		char *becomes[] = {self, "the program the copy becomes, having said the same", NULL};
		execve(self, becomes, environ);
		_exit(1);
	}
	waitpid(pid, NULL, 0);

	struct task_qos_policy both = {LATENCY_QOS_TIER_1, THROUGHPUT_QOS_TIER_1};
	integer_t words[16] = {0};
	mach_msg_type_number_t room = 16, less = 0;
	printf("with no room for the kind: %d; with room for one tier of two: %d; a kind of asking there is not: %d, and to be told of it: %d; told of the kind into no room: %d; asked of the host: %d\n",
	       task_policy_set(task, TASK_CATEGORY_POLICY, (task_policy_t)&now, 0), task_policy_set(task, TASK_BASE_QOS_POLICY, (task_policy_t)&both, 1), task_policy_set(task, 99, (task_policy_t)words, 16),
	       task_policy_get(task, 99, (task_policy_t)words, &room, &usual), task_policy_get(task, TASK_CATEGORY_POLICY, (task_policy_t)&now, &less, &usual),
	       task_policy_set(mach_host_self(), TASK_CATEGORY_POLICY, (task_policy_t)&now, TASK_CATEGORY_POLICY_COUNT));
	return 0;
}
