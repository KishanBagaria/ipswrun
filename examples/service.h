// What the server guest and its client agree on (server.c, client.c).
#include <mach/mach.h>

#define ECHO_SERVICE "com.example.ipswrun.echo"
#define REVERSE_SERVICE "com.example.ipswrun.reverse"

// A request to reverse memory sent out of line, and its answer with the memory reversed: pid is
// then the process the server was told the request came from.
typedef struct {
    mach_msg_header_t header;
    mach_msg_size_t count;
    mach_msg_ool_descriptor_t memory;
    int pid;
} reverse_message_t;
