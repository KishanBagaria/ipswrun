# Guests for ipswrun. Chained fixups are turned off so that every release's dyld can load them.
# ipswrun builds a source file it is given with these flags too: see compiler in cmd/ipswrun/main.go.
#
# They are built for macOS into bin/, or with PLATFORM=ios for iOS into bin/ios/, against the iOS
# SDK that comes with Xcode: make PLATFORM=ios bin/ios/guest.
PLATFORM ?= macos
ifeq ($(PLATFORM),ios)
XCRUN = xcrun --sdk iphoneos
TARGET = -arch arm64 -mios-version-min=14.0 -Wl,-no_fixup_chains
SWIFT_TARGET = arm64-apple-ios14.0
ASYNC_TARGET = arm64-apple-ios15.0
BIN = bin/ios
else
XCRUN =
TARGET = -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains
SWIFT_TARGET = arm64-apple-macos11.0
ASYNC_TARGET = arm64-apple-macos12.0
BIN = bin
endif
GUESTS = guest foundation objcinfo crypto swiftrt threads swiftasync ipc service client prefs adi files registry portstatus pipes pipeevents usage spawn portsets shell kqueue signals handlers faults
# What a guest is run beside, and not by itself.
DAEMONS = server

all: $(addprefix $(BIN)/,$(GUESTS) $(DAEMONS))

$(BIN)/guest: LIBS = -lsqlite3
$(BIN)/crypto: LIBS = -framework Security
$(BIN)/ipc: LIBS = -framework CoreFoundation
$(BIN)/prefs: LIBS = -framework CoreFoundation
$(BIN)/registry: LIBS = -framework CoreFoundation -framework IOKit
$(BIN)/client $(BIN)/service $(BIN)/ipc $(BIN)/adi: xpc_request.h
$(BIN)/client $(BIN)/service $(BIN)/ipc $(BIN)/adi $(BIN)/server: darwin.h
$(BIN)/server $(BIN)/client: service.h
$(BIN)/client: | $(BIN)/server

$(BIN)/%: %.c | $(BIN)
	$(XCRUN) clang $(TARGET) -o $@ $< $(LIBS)

# adid serves a client that is signed as entitled to it. The signature is ad hoc: nothing here
# checks one, and a Mac will not let this run as the client it claims to be.
$(BIN)/adi: adi.c adi.entitlements | $(BIN)
	$(XCRUN) clang $(TARGET) -o $@ $<
	codesign --force --sign - --entitlements adi.entitlements $@

$(BIN)/%: %.m | $(BIN)
	$(XCRUN) clang $(TARGET) -fobjc-arc -O1 -framework Foundation $(LIBS) -o $@ $<

$(BIN)/%: %.swift | $(BIN)
	$(XCRUN) swiftc -target $(SWIFT_TARGET) -O -Xlinker -no_fixup_chains -o $@ $<

# Swift concurrency is part of the system from macOS 12 and iOS 15.
$(BIN)/swiftasync: swiftasync.swift | $(BIN)
	$(XCRUN) swiftc -target $(ASYNC_TARGET) -O -parse-as-library -Xlinker -no_fixup_chains -o $@ $<

$(BIN):
	mkdir -p $@

clean:
	rm -rf bin
