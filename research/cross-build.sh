#!/bin/bash
# cross-build.sh <source.c> <output> [linker flags...]: builds an arm64 macOS guest on Linux.
# Needs clang, ld64.lld (LLVM 18) and a macOS SDK unpacked at $SDK (usr/include, usr/lib and the
# frameworks the guest links are enough), e.g. MacOSX14.5.sdk from
# https://github.com/joseluisq/macosx-sdks/releases. The linker signs ad hoc with no
# entitlements; research/sign adds one.
set -e
: "${SDK:?set SDK to an unpacked macOS SDK}"
src=$1 out=$2; shift 2
clang -target arm64-apple-macos11.0 -isysroot "$SDK" -fblocks -Wall -c -o "$out.o" "$src"
ld64.lld-18 -arch arm64 -platform_version macos 11.0 14.5 -syslibroot "$SDK" -no_fixup_chains -lSystem "$@" -o "$out" "$out.o"
rm -f "$out.o"
