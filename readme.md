# ipswrun

Run an arm64 Mach-O inside the userland of any macOS build, taken straight from that build's restore image (IPSW) over HTTP range requests. No Mac running that release, no VM, and no download of the 13–26 GB image.

## Try it

On an Apple silicon Mac with Go 1.27 or later and the Xcode command line tools, from a clone of this repository:

```sh
go build -o ipswrun ./cmd/ipswrun
make -C examples bin/guest

./ipswrun 'https://updates.cdn-apple.com/2020FallFCS/fullrestores/001-79706/93A4C43C-2B45-47AF-84EA-794F8886B85C/UniversalMac_11.0.1_20B29_Restore.ipsw' examples/bin/guest
./ipswrun 'https://updates.cdn-apple.com/2026FallFCS/c202a724-db7e-4d56-917b-110aa0226507/UniversalMac_27.2.1_26B131_Restore.ipsw' examples/bin/guest
```

The same executable, [`examples/guest.c`](examples/guest.c), reports the system it found itself in, six years apart:

```
macOS 11.0.1 (20B29)
loaded /System/Library/PrivateFrameworks/SoftLinking.framework/SoftLinking
sqlite 3.32.3: 1 row(s) in /Users/emu/guest.sqlite
```

```
macOS 27.2.1 (26B131)
loaded /System/Library/PrivateFrameworks/SoftLinking.framework/SoftLinking
sqlite 3.54.0: 1 row(s) in /Users/emu/guest.sqlite
```

The first `go build` downloads the modules it depends on, which can take several minutes; each run then fetches what it needs of the image and shows its progress while a terminal is watching. [`examples/builds.txt`](examples/builds.txt) has the URL of the first and last release of every major version, and https://api.ipsw.me lists the rest.

## Usage

```sh
ipswrun [flags] <https IPSW url> <guest executable> [guest arguments...]
```

The guest is an ordinary executable built on a Mac (`clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains …`; [`examples/Makefile`](examples/Makefile) has the flags for C, Objective-C and Swift). It is loaded by the build's own dyld against the build's own dyld shared cache, so `dlopen` of any system framework gets that release's code. Its stdout and stderr are passed through and its exit status becomes ipswrun's. ipswrun's own statuses are 2 for a command line it cannot run (nothing is fetched for one) and 70 for a run that failed, whose message ends with what to do about it where that is known.

Runs on macOS on Apple silicon and on Linux (arm64 or x86-64, glibc). Building needs Go and a C compiler (the Xcode command line tools, or gcc): the image-format libraries it builds on (`go-apfs` and `ipsw`, for their LZFSE and disassembler code) use cgo, so `CGO_ENABLED=0` does not build. ipswrun's own code has no C; Unicorn is bound at run time through [purego](https://github.com/ebitengine/purego). The first run downloads a pinned, hash-verified `libunicorn` into the user cache directory. On Linux there is no Apple toolchain to build a guest with, so bring one built on a Mac; and since Linux has no commpage, the guest is given a recorded one from an Apple silicon Mac. macOS 15 and later images also need Apple's image key service.

## Options

| Flag | |
| --- | --- |
| `--home <dir>` | host directory the guest sees as its home, `/Users/emu`; files the guest creates there are the usual way to get results out |
| `--env KEY=VALUE` | added to the guest environment (repeatable) |
| `--system <path>` | system-volume file, or directory ending in `/`, the guest may read (repeatable; default: feature flags, `SystemVersion.plist`, ICU data in `/usr/share/icu/` and `/usr/lib/libobjc-trampolines.dylib`; giving any replaces the default) |
| `--cache <dir>` | where downloads are kept between runs (default: `ipswrun-<uid>` in the system temporary directory, which is cleared on reboot) |
| `--stream-compressed` | allow downloading one compressed disk image member front to back (see below) |
| `--report <file>` | JSON summary: version, build, shared cache UUID, exit code, transfer |
| `--timeout <seconds>` | how long the guest may run, counted from when it starts (default 900) |
| `--device`, `--trace`, `--unicorn`, `--setup` | see `--help` |

A run typically fetches 150–600 MB and takes from twenty seconds to a few minutes, nearly all of it download.

## How it works

- `rangefs` opens the restore image over HTTP ranges: ZIP directory → disk image (AEA-encrypted or UDIF) → GPT → APFS → file. A read fetches exactly the bytes it needs; only a run of small consecutive reads is read ahead (256 KB). A response body that breaks is continued from the byte it reached. Any response that is not exactly the requested range is refused. `--cache` keeps each fetched range as a file named by its offset and length; directories written by earlier versions (fixed 128 KB blocks) are still read.
- `dyldcache` parses the shared cache and produces pages rebased the way the kernel's shared-region pager would (slide info v2, v3, v5).
- `emulator` is a small user-mode Darwin "kernel" on [Unicorn](https://www.unicorn-engine.org). It starts the build's `/usr/lib/dyld` the way XNU does, lets it map its own shared cache, and pages contents in on first touch. BSD system calls, Mach traps and a few MIG routines are implemented just far enough for libSystem, CoreFoundation, Foundation, the Objective-C and Swift runtimes, Security and SQLite. Anything unimplemented stops the run with a message and a guest backtrace instead of guessing. So does `--timeout`: Unicorn is asked whether its timer fired, and a run it interrupted is never resumed.

## Limits

- One thread. There are no other threads, no daemons and no network: work queued to other threads never runs, and a blocking Mach receive is answered with a dead-reply notification, which callers see as an interrupted XPC connection.
- Guest files outside `--home` never reach the host. System files come only from the `--system` allowlist.
- Whatever needs a daemon does not work. `NSUserDefaults` reads back nothing (no `cfprefsd`), and `SecKeyCreateRandomKey` fails unless the key is made the iOS way (`kSecUseDataProtectionKeychain`, not permanent), because the macOS default is the legacy CDSA stack behind `securityd`.
- `mach_vm_remap` copies instead of sharing, and only file-backed memory: enough for code (the Objective-C block trampolines), wrong for memory that is written afterwards.
- The emulated core has a 44-bit address space, and reports no DIT, SHA-3 or SHA-512 instructions.
- macOS 13 and 14 images keep the shared cache in a deflate-compressed ZIP member. Deflate has no exact entry point other than its start, so it cannot be read by range; `--stream-compressed` downloads that one member (3.6–4.5 GB), checks its size and CRC-32, and reads from the inflated copy. Without the flag the run stops instead.
- Verified on restore images from macOS 11.0.1 to 27.2.1 with the guests in [`examples`](examples): [`guest.c`](examples/guest.c) (a private framework and a SQLite database), [`foundation.m`](examples/foundation.m) (collections, JSON, property lists, dates, number formatting, regular expressions, URLs, files), [`swiftrt.swift`](examples/swiftrt.swift) (generics, existentials, reflection, `Codable`, Unicode strings, bridging), [`objcinfo.m`](examples/objcinfo.m) (class, method and protocol lists of the cache and of a framework it loads, classes built at run time, block trampolines, `NSInvocation`) and [`crypto.m`](examples/crypto.m) (CommonCrypto digests, HMAC, AES and PBKDF2; `SecKey` ECDSA, ECDH and RSA; certificate parsing). [`examples/matrix.sh`](examples/matrix.sh) repeats each across the releases and diffs every run against what is recorded in `examples/expected`. Other guests will find system calls that are not implemented yet.
