# Guests for ipswrun. Chained fixups are turned off so that every release's dyld can load them.
TARGET = -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains
GUESTS = guest foundation objcinfo crypto swiftrt threads swiftasync ipc service client prefs adi
# What a guest is run beside, and not by itself.
DAEMONS = server

all: $(addprefix bin/,$(GUESTS) $(DAEMONS))

bin/guest: LIBS = -lsqlite3
bin/crypto: LIBS = -framework Security
bin/ipc: LIBS = -framework CoreFoundation
bin/prefs: LIBS = -framework CoreFoundation
bin/server bin/client: service.h
bin/client: | bin/server

bin/%: %.c | bin
	clang $(TARGET) -o $@ $< $(LIBS)

# adid serves a client that is signed as entitled to it. The signature is ad hoc: nothing here
# checks one, and a Mac will not let this run as the client it claims to be.
bin/adi: adi.c adi.entitlements | bin
	clang $(TARGET) -o $@ $<
	codesign --force --sign - --entitlements adi.entitlements $@

bin/%: %.m | bin
	clang $(TARGET) -fobjc-arc -O1 -framework Foundation $(LIBS) -o $@ $<

bin/%: %.swift | bin
	swiftc -target arm64-apple-macos11.0 -O -Xlinker -no_fixup_chains -o $@ $<

# Swift concurrency is part of the system from macOS 12.
bin/swiftasync: swiftasync.swift | bin
	swiftc -target arm64-apple-macos12.0 -O -parse-as-library -Xlinker -no_fixup_chains -o $@ $<

bin:
	mkdir -p $@

clean:
	rm -rf bin
