// Scratch: adiq <hex request> sends it to adid by Mach message 1200 and prints the answer in hex.
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
    static unsigned char request[1 << 16];
    size_t n = 0;
    for (const char *h = argc > 1 ? argv[1] : ""; h[0] && h[1]; h += 2) { unsigned v; sscanf(h, "%2x", &v); request[n++] = (unsigned char)v; }
    union { adi_message_t message; char room[sizeof(adi_message_t) + 64]; } buffer = {0};
    mach_port_t adid = MACH_PORT_NULL, reply = mig_get_reply_port();
    if (bootstrap_look_up(bootstrap_port, "com.apple.adid", &adid) != KERN_SUCCESS) { printf("no such service\n"); return 1; }
    adi_message_t *m = &buffer.message;
    m->header.msgh_bits = MACH_MSGH_BITS_COMPLEX | MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, MACH_MSG_TYPE_MAKE_SEND_ONCE);
    m->header.msgh_size = sizeof *m; m->header.msgh_remote_port = adid; m->header.msgh_local_port = reply; m->header.msgh_id = 1200;
    m->descriptors = 1; m->data.address = request; m->data.size = (mach_msg_size_t)n; m->data.copy = MACH_MSG_VIRTUAL_COPY; m->data.type = MACH_MSG_OOL_DESCRIPTOR;
    m->ndr = NDR_record; m->count = (mach_msg_size_t)n;
    kern_return_t r = mach_msg(&m->header, MACH_SEND_MSG | MACH_RCV_MSG | MACH_RCV_TIMEOUT, sizeof *m, sizeof buffer, reply, 120000, MACH_PORT_NULL);
    if (r != KERN_SUCCESS) { printf("mach_msg: %s\n", mach_error_string(r)); return 1; }
    printf("answer %d, %u bytes:", m->header.msgh_id, m->data.size);
    for (unsigned i = 0; i < m->data.size && i < 4096; i++) printf("%s%02x", i % 4 ? "" : " ", ((unsigned char *)m->data.address)[i]);
    printf("\n");
    return 0;
}
