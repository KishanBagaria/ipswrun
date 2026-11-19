# Guests for ipswrun. Chained fixups are turned off so that every release's dyld can load them.
TARGET = -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains
GUESTS = guest foundation objcinfo crypto swiftrt threads swiftasync ipc service

all: $(addprefix bin/,$(GUESTS))

bin/guest: LIBS = -lsqlite3
bin/crypto: LIBS = -framework Security
bin/ipc: LIBS = -framework CoreFoundation

bin/%: %.c | bin
	clang $(TARGET) -o $@ $< $(LIBS)

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
