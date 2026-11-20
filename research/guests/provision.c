// Scratch: the guest's half of machine provisioning, beside adid on macOS 13.0 (message 1200).
//
//   provision               with spim.bin in the home: start provisioning (716bd86c), write cpim.bin, wait for
//                           ptm.bin, tk.bin and then done to appear there (the host fetches them meanwhile),
//                           end provisioning (0b2b3196), then ask for a one-time password (cfe0b46a)
//   provision otp           only ask for a one-time password (adid already has its state)
//
// One run has to hold the whole exchange, because the session lives in adid's memory.
#include <mach/mach.h>
#include <servers/bootstrap.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

typedef struct {
    mach_msg_header_t header;
    mach_msg_size_t descriptors;
    mach_msg_ool_descriptor_t data;
    NDR_record_t ndr;
    mach_msg_size_t count;
} adi_message_t;

// Paths are absolute: ipswrun answers chdir without changing anything.
#define HOME "/Users/emu/"

static mach_port_t adid;
static unsigned char request[1 << 20], *answer;
static size_t requested, answered;

static void put32(uint32_t v) { for (int i = 3; i >= 0; i--) request[requested++] = (unsigned char)(v >> (8 * i)); }
static void put64(uint64_t v) { put32((uint32_t)(v >> 32)); put32((uint32_t)v); }
static uint32_t get32(size_t at) { return at + 4 <= answered ? (uint32_t)answer[at] << 24 | answer[at + 1] << 16 | answer[at + 2] << 8 | answer[at + 3] : 0; }
static void begin(uint32_t code) { requested = 0; put32(1); put32(code); put32(1); }

static size_t putfile(const char *name) {
    FILE *f = fopen(name, "rb");
    if (!f) { printf("cannot read %s\n", name); exit(1); }
    size_t n = fread(request + requested + 4, 1, sizeof request - requested - 4, f);
    fclose(f);
    put32((uint32_t)n);
    requested += n;
    return n;
}

// call sends the request and returns the answer's status; the answer stays in answer/answered.
static int32_t call(const char *what) {
    union { adi_message_t message; char room[sizeof(adi_message_t) + 64]; } buffer = {0};
    adi_message_t *m = &buffer.message;
    mach_port_t reply = mig_get_reply_port();
    m->header.msgh_bits = MACH_MSGH_BITS_COMPLEX | MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, MACH_MSG_TYPE_MAKE_SEND_ONCE);
    m->header.msgh_size = sizeof *m; m->header.msgh_remote_port = adid; m->header.msgh_local_port = reply; m->header.msgh_id = 1200;
    m->descriptors = 1; m->data.address = request; m->data.size = (mach_msg_size_t)requested; m->data.copy = MACH_MSG_VIRTUAL_COPY; m->data.type = MACH_MSG_OOL_DESCRIPTOR;
    m->ndr = NDR_record; m->count = (mach_msg_size_t)requested;
    kern_return_t r = mach_msg(&m->header, MACH_SEND_MSG | MACH_RCV_MSG | MACH_RCV_TIMEOUT, sizeof *m, sizeof buffer, reply, 120000, MACH_PORT_NULL);
    if (r != KERN_SUCCESS) { printf("%s: mach_msg: %s\n", what, mach_error_string(r)); exit(1); }
    answer = m->data.address; answered = m->data.size;
    int32_t status = (int32_t)get32(0);
    printf("%s: status %d, answer of %zu bytes\n", what, status, answered);
    return status;
}

// A field of an answer: a big-endian length and that many bytes. Returns the offset after it.
static size_t field(const char *name, size_t at, const char *file) {
    uint32_t n = get32(at);
    if (at + 4 + n > answered) { printf("%s: length %u does not fit\n", name, n); exit(1); }
    printf("%s: %u bytes at offset %zu\n", name, n, at + 4);
    if (file) {
        FILE *f = fopen(file, "wb");
        if (!f || fwrite(answer + at + 4, 1, n, f) != n || fclose(f)) { printf("cannot write %s\n", file); exit(1); }
    }
    return at + 4 + n;
}

static int otp(void) {
    begin(0xcfe0b46a); put64((uint64_t)-2);
    if (call("one-time password")) return 1;
    size_t at = field("machine id", 4, HOME "mid.bin");
    at = field("password", at, HOME "otp.bin");
    printf("%zu bytes follow\n", answered - at);
    return 0;
}

int main(int argc, char **argv) {
    setvbuf(stdout, NULL, _IONBF, 0);
    if (bootstrap_look_up(bootstrap_port, "com.apple.adid", &adid) != KERN_SUCCESS) { printf("no such service\n"); return 1; }
    if (argc > 1 && !strcmp(argv[1], "otp")) return otp();

    begin(0x716bd86c); put64((uint64_t)-2); putfile(HOME "spim.bin");
    if (call("start provisioning")) return 1;
    size_t at = field("client challenge", 4, HOME "cpim.tmp");
    uint32_t session = get32(at);
    printf("session %08x, %zu bytes follow it\n", session, answered - at - 4);
    rename(HOME "cpim.tmp", HOME "cpim.bin"); // the host waits for this name

    struct stat s;
    int waited = 0;
    while (stat(HOME "done", &s)) { // the host writes ptm.bin and tk.bin, then HOME "done"
        if (++waited > 600) { printf("the host never answered\n"); return 1; }
        usleep(200000);
    }
    begin(0x0b2b3196); put32(session);
    size_t ptm = putfile(HOME "ptm.bin"), tk = putfile(HOME "tk.bin");
    printf("ptm %zu bytes, tk %zu bytes\n", ptm, tk);
    if (call("end provisioning")) return 1;
    printf("%zu bytes follow the status\n", answered - 4);
    return otp();
}
