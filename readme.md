# ipswrun

Run an arm64 Mach-O inside the userland of any macOS build, taken straight from that build's restore image (IPSW) over HTTP range requests. No Mac running that release, no VM, and no download of the 13–26 GB image.

## Try it

On an Apple silicon Mac with Go 1.27 or later and the Xcode command line tools:

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

[`examples/builds.txt`](examples/builds.txt) has the URL of the first and last release of every major version, and https://api.ipsw.me lists the rest.

## Usage

```sh
ipswrun [flags] <https IPSW url> <guest executable> [guest arguments...]
```

The guest is an ordinary executable built on a Mac (`clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains …`; [`examples/Makefile`](examples/Makefile) has the flags for C, Objective-C and Swift). It is loaded by the build's own dyld against the build's own dyld shared cache, so `dlopen` of any system framework gets that release's code. Its exit status becomes ipswrun's; ipswrun itself exits 2 for a bad command line and 70 for a run that failed.

Runs on macOS on Apple silicon and on Linux (arm64 or x86-64, glibc). Building needs Go and a C compiler, since dependencies use cgo. The first run downloads a pinned, hash-verified `libunicorn` into the user cache directory. On Linux, bring a guest built on a Mac; it is given a commpage recorded on an Apple silicon Mac. macOS 15 and later images also need Apple's image key service.

## Options

| Flag | |
| --- | --- |
| `--home <dir>` | host directory the guest sees as its home, `/Users/emu`; the usual way to get results out |
| `--env KEY=VALUE` | added to the guest environment (repeatable) |
| `--system <path>` | system-volume file, or directory ending in `/`, the guest may read (repeatable; default: feature flags, `SystemVersion.plist`, ICU data in `/usr/share/icu/` and `/usr/lib/libobjc-trampolines.dylib`; giving any replaces the default) |
| `--cache <dir>` | where downloads are kept between runs (default: `ipswrun-<uid>` in the system temporary directory) |
| `--stream-compressed` | allow downloading one compressed disk image member front to back (see [Limits](#limits)) |
| `--report <file>` | JSON summary: version, build, shared cache UUID, exit code, transfer |
| `--timeout <seconds>` | how long the guest may run (default 900) |
| `--device`, `--trace`, `--unicorn`, `--setup` | see `--help` |

A run typically fetches 150–600 MB and takes from twenty seconds to a few minutes, nearly all of it download.

## How it works

- `rangefs` opens the restore image over HTTP ranges: ZIP directory → disk image (AEA-encrypted or UDIF) → GPT → APFS → file. A read fetches exactly the bytes it needs, and any response that is not exactly the requested range is refused.
- `dyldcache` parses the shared cache and produces pages rebased the way the kernel's shared-region pager would (slide info v2, v3, v5).
- `emulator` is a small user-mode Darwin "kernel" on [Unicorn](https://www.unicorn-engine.org). It starts the build's `/usr/lib/dyld` the way XNU does, lets it map its own shared cache, and pages contents in on first touch. BSD system calls, Mach traps and a few MIG routines are implemented just far enough for libSystem, CoreFoundation, Foundation, the Objective-C and Swift runtimes, Security and SQLite. Anything unimplemented stops the run with a message and a guest backtrace instead of guessing.

## Limits

- One thread, no daemons and no network: work queued to other threads never runs, and a blocking Mach receive is answered with a dead-reply notification, which callers see as an interrupted XPC connection.
- Guest files outside `--home` never reach the host. System files come only from the `--system` allowlist.
- Whatever needs a daemon does not work. `NSUserDefaults` reads back nothing (no `cfprefsd`), and `SecKeyCreateRandomKey` fails unless the key is made the iOS way (`kSecUseDataProtectionKeychain`, not permanent), because the macOS default is the legacy CDSA stack behind `securityd`.
- `mach_vm_remap` copies instead of sharing, and only file-backed memory: enough for code (the Objective-C block trampolines), wrong for memory that is written afterwards.
- The emulated core has a 44-bit address space, and reports no DIT, SHA-3 or SHA-512 instructions.
- macOS 13 and 14 images keep the shared cache in a deflate-compressed ZIP member, which cannot be read by range. `--stream-compressed` downloads that one member (3.6–4.5 GB); without the flag the run stops instead.
- Verified on restore images from macOS 11.0.1 to 27.2.1 with the guests in [`examples`](examples), covering a private framework, SQLite, Foundation, the Swift and Objective-C runtimes, CommonCrypto and `SecKey`. [`examples/matrix.sh`](examples/matrix.sh) repeats each across the releases and diffs every run against `examples/expected`. Other guests will find system calls that are not implemented yet.
