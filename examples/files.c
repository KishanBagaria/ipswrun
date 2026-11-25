// Files guest: what a process changes about a file besides its bytes, and what it says about
// signals and memory, read back as a real system gives it back. Mode, owner and dates through
// chmod, chown, utimes and setattrlist; extended attributes; flock between two descriptors and
// between two threads; a file of the system volume, which cannot be changed; sigaction,
// sigprocmask and sigaltstack; mprotect. Each line is printed as its step finishes.
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/file.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/xattr.h>
#include <unistd.h>

static const char *system_file = "/System/Library/CoreServices/SystemVersion.plist";
static char path[1024];
static int locked; // set by the thread once it has the lock it waited for

static void *lock_later(void *arg) {
    int fd = open(path, O_RDONLY);
    int r = flock(fd, LOCK_EX); // waits for the main thread's lock
    locked = 1;
    flock(fd, LOCK_UN);
    close(fd);
    return (void *)(long)r;
}

static void handler(int sig) {}

int main(void) {
    snprintf(path, sizeof path, "%s/files.txt", getenv("HOME"));
    int fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
    write(fd, "hello\n", 6);

    struct stat st;
    int r = chmod(path, 0600);
    stat(path, &st);
    printf("chmod    %d, mode %o", r, st.st_mode & 07777);
    r = fchmod(fd, 0640);
    fstat(fd, &st);
    printf("; fchmod %d, mode %o\n", r, st.st_mode & 07777);

    int same = chown(path, getuid(), getgid());
    int root = chown(path, 0, 0);
    printf("chown    to its owner %d; to root %d errno %s\n", same, root, root ? strerror(errno) : "-");

    struct timeval times[2] = {{1000000000, 0}, {1234567890, 0}};
    r = utimes(path, times);
    stat(path, &st);
    printf("utimes   %d, modified %ld", r, (long)st.st_mtime);
    struct attrlist list = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_MODTIME};
    struct timespec modified = {1500000000, 0};
    r = setattrlist(path, &list, &modified, sizeof modified, 0);
    stat(path, &st);
    printf("; setattrlist %d, modified %ld\n", r, (long)st.st_mtime);

    r = setxattr(path, "com.example.colour", "blue", 4, 0, 0);
    char value[64] = {0};
    ssize_t got = getxattr(path, "com.example.colour", value, sizeof value, 0, 0);
    int created = setxattr(path, "com.example.colour", "red", 3, 0, XATTR_CREATE);
    int create_errno = errno;
    fsetxattr(fd, "com.example.size", "big", 3, 0, 0);
    // Only this guest's names: a Mac adds its own, such as com.apple.provenance.
    char names[256], mine_names[256] = "";
    ssize_t listed = listxattr(path, names, sizeof names, 0);
    for (char *name = names; name < names + listed; name += strlen(name) + 1)
        if (!strncmp(name, "com.example.", 12)) {
            if (*mine_names) strlcat(mine_names, ",", sizeof mine_names);
            strlcat(mine_names, name, sizeof mine_names);
        }
    printf("xattr    set %d, read %zd \"%s\"; again with XATTR_CREATE %d %s; names %s\n", r, got, value, created,
           strerror(create_errno), mine_names);
    r = removexattr(path, "com.example.colour", 0);
    got = getxattr(path, "com.example.colour", value, sizeof value, 0, 0);
    printf("xattr    removed %d, then read %zd %s; of a system file %zd %s\n", r, got, strerror(errno),
           getxattr(system_file, "com.example.colour", value, sizeof value, 0, 0), strerror(errno));

    r = chmod(system_file, 0600);
    printf("system   chmod %d %s; utimes %d %s; setxattr %d %s\n", r, strerror(errno), utimes(system_file, times),
           strerror(errno), setxattr(system_file, "com.example.colour", "blue", 4, 0, 0), strerror(errno));

    int other = open(path, O_RDONLY);
    int mine = flock(fd, LOCK_EX);
    int theirs = flock(other, LOCK_EX | LOCK_NB);
    printf("flock    exclusive %d; another's %d %s", mine, theirs, theirs ? strerror(errno) : "-");
    pthread_t thread;
    pthread_create(&thread, NULL, lock_later, NULL);
    usleep(100000);
    int waited = !locked;
    flock(fd, LOCK_UN);
    void *result;
    pthread_join(thread, &result);
    printf("; a thread waited %d, then got it %d (%ld)\n", waited, locked, (long)result);
    close(other);

    struct sigaction action = {.sa_handler = handler, .sa_flags = SA_RESTART}, old;
    sigemptyset(&action.sa_mask);
    sigaddset(&action.sa_mask, SIGUSR2);
    sigaction(SIGUSR1, &action, NULL);
    sigaction(SIGUSR1, NULL, &old);
    printf("signals  handler kept %d, mask %d, flags %#x", old.sa_handler == handler, sigismember(&old.sa_mask, SIGUSR2),
           old.sa_flags);
    sigset_t block, now;
    sigemptyset(&block);
    sigaddset(&block, SIGUSR1);
    sigprocmask(SIG_BLOCK, &block, NULL);
    sigprocmask(SIG_BLOCK, NULL, &now);
    printf("; blocked %d", sigismember(&now, SIGUSR1));
    stack_t stack = {.ss_sp = malloc(SIGSTKSZ), .ss_size = SIGSTKSZ}, old_stack;
    sigaltstack(&stack, NULL);
    sigaltstack(NULL, &old_stack);
    printf("; alternate stack %d\n", old_stack.ss_sp == stack.ss_sp && old_stack.ss_size == SIGSTKSZ);

    long page = getpagesize();
    char *memory = mmap(NULL, 2 * page, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
    memory[0] = 'x';
    int readonly = mprotect(memory, page, PROT_READ);
    int none = mprotect(memory + page, page, PROT_NONE);
    int unaligned = mprotect(memory + 1, page, PROT_READ);
    printf("mprotect read-only %d, still reads '%c'; no access %d; unaligned %d %s\n", readonly, memory[0], none,
           unaligned, strerror(errno));

    close(fd);
    unlink(path);
    return 0;
}
