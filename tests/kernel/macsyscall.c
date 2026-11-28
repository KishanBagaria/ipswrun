// macsyscall measures what a Mac's kernel answers to the __mac_syscall calls guests make, for a
// process that is not sandboxed: the emulator answers those, and only those, the same way (see
// sysMacSyscall in emulator/bsd.go). It is run on a Mac, natively, not in ipswrun:
//
//   clang -o /tmp/macsyscall tests/kernel/macsyscall.c -lsandbox && /tmp/macsyscall
//
// Each call is made directly, with the words the guests pass (as ipswrun's trace shows them), and
// printed with what it returned, its errno and what it wrote. A few are also made through the
// functions guests reach them by; the calls those make inside the shared cache cannot be
// interposed, so only their results show. Calls that change the process (quarantining it,
// Quarantine 87; looking up and applying a sandbox profile, Sandbox 22 and 0, as cfprefsd does on
// its way into its sandbox) are made in a child process.
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>

int __mac_syscall(const char *policy, int call, void *arg);
int sandbox_check(pid_t pid, const char *operation, int type, ...);
int sandbox_container_path_for_pid(pid_t pid, char *buffer, size_t size);
char *sandbox_extension_issue_file(const char *extension_class, const char *path, uint32_t flags);
extern const uint32_t SANDBOX_CHECK_NO_REPORT;
enum { SANDBOX_FILTER_NONE = 0, SANDBOX_FILTER_PATH = 1 };
struct sandbox_profile { uint32_t type; void *data; size_t size; };
struct sandbox_profile *sandbox_compile_string(const char *profile, void *parameters, char **error);
void *sandbox_create_params(void);

static void dump(const char *what, const void *p, size_t n) {
    printf("    %s:", what);
    for (size_t i = 0; i < n; i++) printf("%s%02x", i % 8 ? "" : " ", ((const uint8_t *)p)[i]);
    printf("\n");
}

// __mac_syscall, printed: the argument's first 64 bytes before and after, and the answer.
static int logged(const char *policy, int call, void *arg) {
    uint8_t before[64];
    memcpy(before, arg, sizeof before);
    errno = 0;
    int r = __mac_syscall(policy, call, arg);
    int e = errno;
    printf("%s %d = %d errno %d (%s)\n", policy, call, r, r ? e : 0, r ? strerror(e) : "-");
    dump("before", before, sizeof before);
    if (memcmp(before, arg, sizeof before)) dump("after ", arg, 64);
    errno = e;
    return r;
}

int main(void) {
    setvbuf(stdout, NULL, _IONBF, 0);
    pid_t pid = getpid();
    char path[1024];
    snprintf(path, sizeof path, "%s/macsyscall-probe.plist", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp");
    fclose(fopen(path, "w"));

    // AMFI 90, amfi_check_dyld_policy_self as dyld and libsystem_trace call it: {in flags, &out}.
    uint64_t out = 0xaaaaaaaaaaaaaaaa, amfi[2] = {0, (uint64_t)&out};
    printf("-- AMFI 90, as dyld calls it\n");
    logged("AMFI", 90, amfi);
    printf("    out flags %#llx\n", (unsigned long long)out);

    // Sandbox 2, a check, as dyld makes it for a system call: {result, pid, "syscall-unix",
    // type 0x41, the call's number (550, as dyld passes), 1}.
    uint64_t result[2] = {0xaaaaaaaaaaaaaaaa, 0xaaaaaaaaaaaaaaaa}, check[12] = {(uint64_t)result, (uint64_t)pid, (uint64_t)"syscall-unix", 0x41, 0x226, 1};
    printf("-- Sandbox 2, as dyld checks syscall-unix\n");
    logged("Sandbox", 2, check);
    dump("result", result, sizeof result);

    // The raw calls libsystem_sandbox makes for the guests, word for word as ipswrun's trace shows
    // them (macOS 15.6.1): the library here is another release's, and its calls to __mac_syscall
    // cannot be interposed. Sandbox 2 is a check, {result, pid, operation, type, argument, flags}.
    // That release's flags (0x40000011) mean something else to this Mac's kernel, which answers
    // some of them with a result of 3: those results say nothing about the guest's own kernel.
    uint64_t buf[2] = {0};
    char name[] = "apple.cfprefs.501v1", domain[] = "com.apple.TimeMachine", missing[1100];
    snprintf(missing, sizeof missing, "%s.missing/Library/Preferences/com.example.ipswrun.prefs.plist", path);
    struct { const char *what; uint64_t w[8]; } checks[] = {
        {"of the process, no operation (+0xdf4)", {(uint64_t)buf, (uint64_t)pid}},
        {"syscall-unix 41 (+0xdf4)", {(uint64_t)buf, (uint64_t)pid, (uint64_t)"syscall-unix", 0x41, 0x29, 1}},
        {"mach-lookup of cfprefsd (+0xdf4)", {(uint64_t)buf, (uint64_t)pid, (uint64_t)"mach-lookup", 6, (uint64_t)"com.apple.cfprefsd.daemon", 1}},
        {"user-preference-read (+0xdf4)", {(uint64_t)buf, (uint64_t)pid, (uint64_t)"user-preference-read", 0x1c, (uint64_t)domain, 0x40000011, 1}},
        {"ipc-posix-shm-write-data (+0xdf4)", {(uint64_t)buf, (uint64_t)pid, (uint64_t)"ipc-posix-shm-write-data", 5, (uint64_t)name, 1}},
        {"file-write-data by launchd of descriptor 3, closed (+0xa28)", {(uint64_t)buf, 1, (uint64_t)"file-write-data", 0xf0, 3, 0x20000005}},
        {"file-write-data by launchd of descriptor 3, open", {(uint64_t)buf, 1, (uint64_t)"file-write-data", 0xf0, 3, 0x20000005}},
        {"file-write-data by the process of descriptor 3, open", {(uint64_t)buf, (uint64_t)pid, (uint64_t)"file-write-data", 0xf0, 3, 0x20000005}},
        {"mach-lookup of cfprefsd by launchd", {(uint64_t)buf, 1, (uint64_t)"mach-lookup", 6, (uint64_t)"com.apple.cfprefsd.daemon", 1}},
        {"no operation, flags 0x40000001 (iOS 14.0)", {(uint64_t)buf, (uint64_t)pid, 0, 0, 0, 0x40000001, 1}},
        {"user-preference-write", {(uint64_t)buf, (uint64_t)pid, (uint64_t)"user-preference-write", 0x1c, (uint64_t)domain, 0x40000011, 1}},
        {"file-write-data of a path that is there (filter type 1)", {(uint64_t)buf, (uint64_t)pid, (uint64_t)"file-write-data", 1, (uint64_t)path, 0x40000011, 1}},
        {"file-write-data of a path that is not", {(uint64_t)buf, (uint64_t)pid, (uint64_t)"file-write-data", 1, (uint64_t)missing, 0x40000011, 1}},
    };
    for (size_t i = 0; i < sizeof checks / sizeof *checks; i++) {
        if (i == 6) dup2(open(path, O_RDWR), 3);
        memset(buf, 0xaa, sizeof buf);
        printf("-- Sandbox 2, %s\n", checks[i].what);
        logged("Sandbox", 2, checks[i].w);
        dump("result", buf, sizeof buf);
    }
    close(3);

    // The same checks of another process, as cfprefsd makes them of its client, and of one that
    // is not there.
    pid_t other = fork();
    if (other == 0) {
        pause();
        exit(0);
    }
    struct { const char *what; uint64_t w[8]; } others[] = {
        {"user-preference-read of another process", {(uint64_t)buf, (uint64_t)other, (uint64_t)"user-preference-read", 0x1c, (uint64_t)domain, 0x40000011, 1}},
        {"user-preference-read, flags 0x40000000, of another process", {(uint64_t)buf, (uint64_t)other, (uint64_t)"user-preference-read", 0x1c, (uint64_t)domain, 0x40000000, 1}},
        {"file-write-data of a path, of another process", {(uint64_t)buf, (uint64_t)other, (uint64_t)"file-write-data", 1, (uint64_t)path, 0x40000011, 1}},
        {"mach-lookup of another process", {(uint64_t)buf, (uint64_t)other, (uint64_t)"mach-lookup", 6, (uint64_t)"com.apple.cfprefsd.daemon", 1}},
        {"user-preference-read of a process that is not there", {(uint64_t)buf, 99999, (uint64_t)"user-preference-read", 0x1c, (uint64_t)domain, 0x40000011, 1}},
    };
    for (size_t i = 0; i < sizeof others / sizeof *others; i++) {
        memset(buf, 0xaa, sizeof buf);
        printf("-- Sandbox 2, %s\n", others[i].what);
        logged("Sandbox", 2, others[i].w);
        dump("result", buf, sizeof buf);
    }

    // Sandbox 4, {pid, kind, buffer, size, ...}: kind 0 the container (+0xb10), kind 1 (+0xee4).
    char c[1024];
    struct { const char *what; uint64_t w[8]; } queries[] = {
        {"kind 0, the container (+0xb10)", {(uint64_t)pid, 0, (uint64_t)c, 0x400, 0, 1}},
        {"kind 1, 0x400 bytes (+0xee4)", {(uint64_t)pid, 1, (uint64_t)c, 0x400, 1}},
        {"kind 1, 0x402 bytes (+0xee4)", {(uint64_t)pid, 1, (uint64_t)c, 0x402, 1}},
        {"kind 0 of another process", {(uint64_t)other, 0, (uint64_t)c, 0x400, 0, 1}},
        {"kind 1 of another process", {(uint64_t)other, 1, (uint64_t)c, 0x400, 1}},
        {"kind 1 of a process that is not there", {99999, 1, (uint64_t)c, 0x400, 1}},
    };
    for (size_t i = 0; i < sizeof queries / sizeof *queries; i++) {
        memset(c, 0x55, sizeof c);
        printf("-- Sandbox 4, %s\n", queries[i].what);
        logged("Sandbox", 4, queries[i].w);
        printf("    buffer starts %02x\n", (uint8_t)c[0]);
    }

    kill(other, SIGKILL);
    waitpid(other, NULL, 0);

    // Sandbox 12, sandbox_passthrough_access(descriptor, flags), as CFPreferences calls it for a
    // plist it opened: {descriptor, flags}, 0x102 as it passes them.
    int file = open(path, O_RDWR), dir = open("/tmp", O_RDONLY), pipes[2];
    pipe(pipes);
    struct { const char *what; uint64_t w[2]; } passthrough[] = {
        {"a regular file, flags 0x102", {(uint64_t)file, 0x102}},
        {"a regular file, no flags", {(uint64_t)file, 0}},
        {"a regular file, flags 0x2", {(uint64_t)file, 0x2}},
        {"a regular file, flags 0x100", {(uint64_t)file, 0x100}},
        {"a regular file, flags 0x1", {(uint64_t)file, 0x1}},
        {"a directory, flags 0x102", {(uint64_t)dir, 0x102}},
        {"a pipe, flags 0x102", {(uint64_t)pipes[0], 0x102}},
        {"a descriptor that is not open, flags 0x102", {77, 0x102}},
    };
    for (size_t i = 0; i < sizeof passthrough / sizeof *passthrough; i++) {
        printf("-- Sandbox 12, %s\n", passthrough[i].what);
        logged("Sandbox", 12, passthrough[i].w);
    }
    close(file), close(dir), close(pipes[0]), close(pipes[1]);

    // Sandbox 67, sandbox_get_container_expected as libsystem_containermanager calls it when a
    // process starts (iOS 18 and later): {&expected, &second}, two words the kernel sets.
    uint64_t expected[2] = {0xaaaaaaaaaaaaaaaa, 0xaaaaaaaaaaaaaaaa}, e67[2] = {(uint64_t)&expected[0], (uint64_t)&expected[1]};
    printf("-- Sandbox 67, whether a container is expected\n");
    logged("Sandbox", 67, e67);
    dump("words", expected, sizeof expected);

    printf("-- sandbox_check(pid, NULL, 0): is the process sandboxed\n");
    printf("   returned %d\n", sandbox_check(pid, NULL, SANDBOX_FILTER_NONE));
    printf("-- sandbox_check(pid, \"file-write-data\", path)\n");
    printf("   returned %d\n", sandbox_check(pid, "file-write-data", SANDBOX_FILTER_PATH | SANDBOX_CHECK_NO_REPORT, path));
    printf("-- sandbox_check(1, \"file-write-data\", path): of launchd\n");
    printf("   returned %d\n", sandbox_check(1, "file-write-data", SANDBOX_FILTER_PATH | SANDBOX_CHECK_NO_REPORT, path));

    char container[1024];
    memset(container, 0x55, sizeof container);
    printf("-- sandbox_container_path_for_pid(pid)\n");
    int r = sandbox_container_path_for_pid(pid, container, sizeof container);
    printf("   returned %d errno %d, buffer starts %02x\n", r, errno, (uint8_t)container[0]);

    printf("-- sandbox_extension_issue_file(read-write, path)\n");
    char *token = sandbox_extension_issue_file("com.apple.app-sandbox.read-write", path, 0);
    printf("   returned %s\n", token ? "a token" : "NULL");
    if (token) printf("   token %.40s... (%zu bytes)\n", token, strlen(token));

    // What cfprefsd does on its way into its sandbox, each in a child process, as it changes the
    // process. Quarantine 87, as _qtn_proc_apply_to_pid makes it: {0, the name, 0, two empty
    // buffers, 0, 0, 0, 6}.
    if (fork() == 0) {
        uint8_t qa[0x48] = {0}, qb[0x48] = {0};
        uint64_t q[12] = {0, (uint64_t)"com.apple.cfprefsd", 0, (uint64_t)qa, (uint64_t)qb, 0, 0, 0, 6};
        printf("-- Quarantine 87, the process quarantined, as cfprefsd starts\n");
        logged("Quarantine", 87, q);
        dump("first buffer", qa, 32);
        dump("second buffer", qb, 32);
        exit(0);
    }
    wait(NULL);
    // Sandbox 22, as sandbox_compile_named asks for a profile the kernel has by name: {the name,
    // the parameters, ...}. On this Mac the kernel has none by cfprefsd's name, and libsandbox
    // compiles the profile itself.
    if (fork() == 0) {
        uint64_t params[4] = {0, 0, 8, 0}, n[12] = {(uint64_t)"com.apple.cfprefsd", (uint64_t)params};
        printf("-- Sandbox 22, a profile the kernel has by cfprefsd's name\n");
        logged("Sandbox", 22, n);
        exit(0);
    }
    wait(NULL);
    // Sandbox 0, applying a compiled profile, as sandbox_apply_container does: {the profile, its
    // size, 0, 0}. One that allows everything, compiled by libsandbox.
    if (fork() == 0) {
        char *error = NULL;
        struct sandbox_profile *profile = sandbox_compile_string("(version 1) (allow default)", sandbox_create_params(), &error);
        if (!profile) {
            printf("-- Sandbox 0: the profile does not compile: %s\n", error);
            exit(1);
        }
        uint64_t apply[12] = {(uint64_t)profile->data, profile->size, 0, 0};
        printf("-- Sandbox 0, a compiled profile applied (%zu bytes)\n", profile->size);
        logged("Sandbox", 0, apply);
        printf("   sandbox_check(pid, NULL, 0) now returns %d\n", sandbox_check(getpid(), NULL, SANDBOX_FILTER_NONE));
        exit(0);
    }
    wait(NULL);
    unlink(path);
    return 0;
}
