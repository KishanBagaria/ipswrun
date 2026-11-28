# How the guests are built, for the Makefile of each folder that has some (examples, tests/kernel),
# which says where it is from the top (WHERE) and the way back there (ROOT) and then includes
# this. Which guests a folder has, and for which platforms, is in tests/guests.txt.
#
# Chained fixups are turned off so that every release's dyld can load them. ipswrun builds a
# source file it is given with these flags too: see compiler in cmd/ipswrun/main.go.
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

# The guests of this folder that the platform has: a line of the list has the folder second and
# the platforms third.
GUESTS := $(shell awk -F'\t' -v where=$(WHERE) -v platform=$(PLATFORM) '$$2 == where && index($$3, platform) { print $$1 }' $(ROOT)/tests/guests.txt | sort -u)

all: $(addprefix $(BIN)/,$(GUESTS))

# The headers the guests share are with the examples.
$(BIN)/%: %.c | $(BIN)
	$(XCRUN) clang $(TARGET) -I$(ROOT)/examples -o $@ $< $(LIBS)

$(BIN)/%: %.m | $(BIN)
	$(XCRUN) clang $(TARGET) -fobjc-arc -O1 -framework Foundation $(LIBS) -o $@ $<

$(BIN)/%: %.swift | $(BIN)
	$(XCRUN) swiftc -target $(SWIFT_TARGET) -O -Xlinker -no_fixup_chains -o $@ $<

$(BIN):
	mkdir -p $@
