# The guests that hold the emulated kernel against a Mac's: make, or make PLATFORM=ios. See
# ../guests.mk for how they are built and ../guests.txt for the list.
WHERE = tests/kernel
ROOT = ../..
include ../guests.mk

$(BIN)/sqlitewal: LIBS = -lsqlite3
$(BIN)/ipc: LIBS = -framework CoreFoundation
$(BIN)/registry: LIBS = -framework CoreFoundation -framework IOKit
$(BIN)/power: LIBS = -framework CoreFoundation -framework IOKit
$(BIN)/ipc: $(ROOT)/examples/xpc_request.h
$(BIN)/ipc $(BIN)/machports: $(ROOT)/examples/darwin.h
