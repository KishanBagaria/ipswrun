// macsyscall measures what a Mac's kernel answers to the __mac_syscall calls guests make, for a
// process that is not sandboxed: the emulator answers those, and only those, the same way (see
// sysMacSyscall in emulator/bsd.go). It is run on a Mac, natively, not in ipswrun:
//
//   clang -o /tmp/macsyscall examples/macsyscall.c && /tmp/macsyscall
//
// The calls are made through the functions guests reach them by, with __mac_syscall interposed so
// that each raw call is printed with what it returned, its errno and what it wrote, so the
// arguments are the ones Apple's code builds. dyld's own calls, which nothing can interpose, are
// made directly with the arguments dyld passes. Calls that change the process (applying a sandbox
// profile, Sandbox 1 and 22; applying quarantine to itself, Quarantine 87) are not made: the
// emulator cannot do what they do, and stops on them.
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int __mac_syscall(const char *policy, int call, void *arg);
int sandbox_check(pid_t pid, const char *operation, int type, ...);
int sandbox_container_path_for_pid(pid_t pid, char *buffer, size_t size);
char *sandbox_extension_issue_file(const char *extension_class, const char *path, uint32_t flags);
extern const uint32_t SANDBOX_CHECK_NO_REPORT;
enum { SANDBOX_FILTER_NONE = 0, SANDBOX_FILTER_PATH = 1 };

static void dump(const char *what, const void *p, size_t n) {
    printf("    %s:", what);
    for (size_t i = 0; i < n; i++) printf("%s%02x", i % 8 ? "" : " ", ((const uint8_t *)p)[i]);
    printf("\n");
}

// The interposed __mac_syscall: the argument's first 64 bytes before and after, and the answer.
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
__attribute__((used, section("__DATA,__interpose"))) static struct {
    const void *replacement, *replacee;
} interpose[] = {{(const void *)logged, (const void *)__mac_syscall}};

int main(void) {
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
    uint64_t result[2] = {0}, check[12] = {(uint64_t)result, (uint64_t)pid, (uint64_t)"syscall-unix", 0x41, 0x226, 1};
    printf("-- Sandbox 2, as dyld checks syscall-unix\n");
    logged("Sandbox", 2, check);
    dump("result", result, sizeof result);

    // The raw calls libsystem_sandbox makes for the guests, word for word as ipswrun's trace shows
    // them (macOS 15.6.1): the library here is another release's, and its calls to __mac_syscall
    // cannot be interposed. Sandbox 2 is a check, {result, pid, operation, type, argument, flags}.
    uint64_t buf[2] = {0};
    char name[] = "apple.cfprefs.501v1", domain[] = "com.apple.TimeMachine";
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
    };
    for (size_t i = 0; i < sizeof checks / sizeof *checks; i++) {
        if (i == 6) dup2(open(path, O_RDWR), 3);
        printf("-- Sandbox 2, %s\n", checks[i].what);
        logged("Sandbox", 2, checks[i].w);
        dump("result", buf, sizeof buf);
    }
    close(3);

    // Sandbox 4, {pid, kind, buffer, size, ...}: kind 0 the container (+0xb10), kind 1 (+0xee4).
    char c[1024];
    struct { const char *what; uint64_t w[8]; } queries[] = {
        {"kind 0, the container (+0xb10)", {(uint64_t)pid, 0, (uint64_t)c, 0x400, 0, 1}},
        {"kind 1, 0x400 bytes (+0xee4)", {(uint64_t)pid, 1, (uint64_t)c, 0x400, 1}},
        {"kind 1, 0x402 bytes (+0xee4)", {(uint64_t)pid, 1, (uint64_t)c, 0x402, 1}},
    };
    for (size_t i = 0; i < sizeof queries / sizeof *queries; i++) {
        memset(c, 0x55, sizeof c);
        printf("-- Sandbox 4, %s\n", queries[i].what);
        logged("Sandbox", 4, queries[i].w);
        printf("    buffer starts %02x\n", (uint8_t)c[0]);
    }

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
    unlink(path);
    return 0;
}
