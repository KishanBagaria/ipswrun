# The programs that show what ipswrun runs: make, or make PLATFORM=ios. See ../tests/guests.mk
# for how they are built and ../tests/guests.txt for the list.
WHERE = examples
ROOT = ..
include ../tests/guests.mk

$(BIN)/guest: LIBS = -lsqlite3
$(BIN)/crypto: LIBS = -framework Security
$(BIN)/prefs: LIBS = -framework CoreFoundation
$(BIN)/client $(BIN)/service $(BIN)/adi: xpc_request.h
$(BIN)/client $(BIN)/service $(BIN)/adi $(BIN)/server: darwin.h
$(BIN)/server $(BIN)/client: service.h
$(BIN)/client: | $(BIN)/server

# adid serves a client that is signed as entitled to it. The signature is ad hoc: nothing here
# checks one, and a Mac will not let this run as the client it claims to be.
$(BIN)/adi: adi.c adi.entitlements | $(BIN)
	$(XCRUN) clang $(TARGET) -o $@ $<
	codesign --force --sign - --entitlements adi.entitlements $@

# Swift concurrency is part of the system from macOS 12 and iOS 15.
$(BIN)/swiftasync: swiftasync.swift | $(BIN)
	$(XCRUN) swiftc -target $(ASYNC_TARGET) -O -parse-as-library -Xlinker -no_fixup_chains -o $@ $<
