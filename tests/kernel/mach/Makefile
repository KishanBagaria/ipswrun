# Guests for Mach: ports, tasks, threads and the I/O Registry: make, or make PLATFORM=ios. See ../../guests.mk for how they are
# built and ../../guests.txt for the list.
WHERE = tests/kernel/mach
ROOT = ../../..
include $(ROOT)/tests/guests.mk

$(BIN)/ipc: LIBS = -framework CoreFoundation
$(BIN)/registry: LIBS = -framework CoreFoundation -framework IOKit
$(BIN)/power: LIBS = -framework CoreFoundation -framework IOKit
$(BIN)/ipc: $(ROOT)/examples/xpc_request.h
$(BIN)/ipc $(BIN)/machports: $(ROOT)/examples/darwin.h
