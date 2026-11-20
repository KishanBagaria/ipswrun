// Scratch: adiseq <request>... sends several requests to adid in one run (message 1200) and prints
// each answer. A request is pieces joined by '+': hex digits, S for the session the last
// successful start provisioning returned (the last four bytes of its answer), or F:<path> for a
// big-endian length and that file's bytes.
#include <mach/mach.h>
#include <servers/bootstrap.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
typedef struct {
    mach_msg_header_t header;
    mach_msg_size_t descriptors;
    mach_msg_ool_descriptor_t data;
    NDR_record_t ndr;
    mach_msg_size_t count;
} adi_message_t;
int main(int argc, char **argv) {
    static unsigned char request[1 << 20];
    unsigned char session[4] = {0};
    mach_port_t adid = MACH_PORT_NULL;
    setvbuf(stdout, NULL, _IONBF, 0);
    if (bootstrap_look_up(bootstrap_port, "com.apple.adid", &adid) != KERN_SUCCESS) { printf("no such service\n"); return 1; }
    for (int a = 1; a < argc; a++) {
        size_t n = 0;
        for (char *piece = strtok(argv[a], "+"); piece; piece = strtok(NULL, "+")) {
            if (!strcmp(piece, "S")) { memcpy(request + n, session, 4); n += 4; }
            else if (!strncmp(piece, "F:", 2)) {
                FILE *f = fopen(piece + 2, "rb");
                if (!f) { printf("cannot read %s\n", piece + 2); return 1; }
                size_t size = fread(request + n + 4, 1, sizeof request - n - 4, f);
                fclose(f);
                request[n] = (unsigned char)(size >> 24); request[n + 1] = (unsigned char)(size >> 16); request[n + 2] = (unsigned char)(size >> 8); request[n + 3] = (unsigned char)size;
                n += 4 + size;
            } else for (const char *h = piece; h[0] && h[1]; h += 2) { unsigned v; sscanf(h, "%2x", &v); request[n++] = (unsigned char)v; }
        }
        union { adi_message_t message; char room[sizeof(adi_message_t) + 64]; } buffer = {0};
        adi_message_t *m = &buffer.message;
        mach_port_t reply = mig_get_reply_port();
        m->header.msgh_bits = MACH_MSGH_BITS_COMPLEX | MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, MACH_MSG_TYPE_MAKE_SEND_ONCE);
        m->header.msgh_size = sizeof *m; m->header.msgh_remote_port = adid; m->header.msgh_local_port = reply; m->header.msgh_id = 1200;
        m->descriptors = 1; m->data.address = request; m->data.size = (mach_msg_size_t)n; m->data.copy = MACH_MSG_VIRTUAL_COPY; m->data.type = MACH_MSG_OOL_DESCRIPTOR;
        m->ndr = NDR_record; m->count = (mach_msg_size_t)n;
        kern_return_t r = mach_msg(&m->header, MACH_SEND_MSG | MACH_RCV_MSG | MACH_RCV_TIMEOUT, sizeof *m, sizeof buffer, reply, 120000, MACH_PORT_NULL);
        if (r != KERN_SUCCESS) { printf("mach_msg: %s\n", mach_error_string(r)); return 1; }
        unsigned char *answer = m->data.address;
        unsigned size = m->data.size;
        printf("request %d (%02x%02x%02x%02x, %zu bytes): %u bytes:", a, request[4], request[5], request[6], request[7], n, size);
        for (unsigned i = 0; i < size && i < 4096; i++) printf("%s%02x", i % 4 ? "" : " ", answer[i]);
        printf("\n");
        if (size > 8 && !memcmp(answer, "\0\0\0\0", 4) && !memcmp(request + 4, "\x71\x6b\xd8\x6c", 4)) memcpy(session, answer + size - 4, 4);
    }
    return 0;
}
