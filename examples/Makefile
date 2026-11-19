# Guests for ipswrun. Chained fixups are turned off so that every release's dyld can load them.
CFLAGS = -arch arm64 -mmacosx-version-min=11.0 -fobjc-arc -O1 -Wl,-no_fixup_chains
GUESTS = foundation objcinfo crypto swiftrt

all: $(addprefix bin/,$(GUESTS))

bin/foundation bin/objcinfo: bin/%: %.m
	@mkdir -p bin
	clang $(CFLAGS) -framework Foundation -o $@ $<

bin/crypto: crypto.m
	@mkdir -p bin
	clang $(CFLAGS) -framework Foundation -framework Security -o $@ $<

bin/swiftrt: swiftrt.swift
	@mkdir -p bin
	swiftc -target arm64-apple-macos11.0 -O -Xlinker -no_fixup_chains -o $@ $<

clean:
	rm -rf bin
