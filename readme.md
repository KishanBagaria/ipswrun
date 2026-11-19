# ipswrun

ipswrun runs an arm64 Mach-O binary against the system libraries of any macOS release since 11, fetching only the parts it needs from Apple's restore image (IPSW) for that release. You don't need a Mac running that release, a VM, or the ~26 GB ipsw image downloaded on disk.

Use it to see what a framework does or what a private API returns on every macOS release across history.

## Try it

On an Apple silicon Mac with Go 1.27+:

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

A run typically fetches 150–600 MB and takes from twenty seconds to a few minutes, nearly all of it download. Running the same release again reuses what was fetched.

[`examples/builds.txt`](examples/builds.txt) has the URL of the first and last release of every major version, and https://api.ipsw.me lists the rest. macOS 13 and 14 need [`--stream-compressed`](#what-does-not-work).

## Usage

```sh
ipswrun [flags] <https IPSW url> <guest executable> [guest arguments...]
```

The program you run, the guest, is an ordinary executable built on a Mac:

```sh
clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains -o guest guest.c
```

`-no_fixup_chains` is what lets every release's dyld load it. [`examples/Makefile`](examples/Makefile) has the flags for Objective-C and Swift too.

What the guest gets:

- **That release's code.** It is loaded by the release's own dyld against the release's own dyld shared cache, so `dlopen` of any system framework, private ones included, gets the code that shipped.
- **A home directory**, `/Users/emu`. By default it is a temporary directory that is deleted after the run. Pass `--home <dir>` to keep what the guest writes there, which is the usual way to get results out.
- **Standard output and error**, printed when the guest has finished rather than as it runs.
- **Its exit status**, which becomes ipswrun's. ipswrun itself exits 2 for a bad command line and 70 for a run that failed.

## Options

| Flag | What it does |
| --- | --- |
| `--home <dir>` | host directory the guest sees as its home, `/Users/emu` (default: a temporary one, deleted afterwards) |
| `--env KEY=VALUE` | added to the guest environment (repeatable) |
| `--system <path>` | system-volume file, or directory ending in `/`, the guest may read (repeatable). The default is feature flags, `SystemVersion.plist`, ICU data in `/usr/share/icu/` and `/usr/lib/libobjc-trampolines.dylib`; giving any replaces the default |
| `--cache <dir>` | where downloads are kept between runs (default: `ipswrun-<uid>` in the system temporary directory, so cleared on reboot) |
| `--stream-compressed` | allow downloading one compressed disk image member front to back, for macOS 13 and 14 |
| `--report <file>` | write a JSON summary: version, build, shared cache UUID, exit code, pages mapped, requests, bytes and seconds |
| `--timeout <seconds>` | how long the guest may run, counted from when it starts (default 900) |
| `--trace` | print every emulated system call |
| `--device <product>` | BuildManifest product used to pick disk images (default `Macmini9,1`) |
| `--unicorn <dir>` | where the pinned `libunicorn` is kept (default: the user cache directory) |
| `--setup` | only fetch and verify `libunicorn`, then exit |

## Requirements

- **Host:** macOS on Apple silicon, or Linux on arm64 or x86-64 with glibc.
- **To build:** Go and a C compiler, since dependencies use cgo.
- **On Linux:** bring a guest built on a Mac. It is given a commpage recorded on an Apple silicon Mac.
- **Network:** the first run downloads a pinned, hash-verified `libunicorn` into the user cache directory. macOS 15 and later images are encrypted, and their key comes from Apple's key service.

## What does not work

- **Threads, daemons and the network.** There is one thread: work queued to other threads never runs. A blocking Mach receive is answered with a dead-reply notification, which callers see as an interrupted XPC connection.
- **Anything that needs a daemon.** `NSUserDefaults` reads back nothing (no `cfprefsd`). `SecKeyCreateRandomKey` fails unless the key is made the iOS way (`kSecUseDataProtectionKeychain`, not permanent), because the macOS default is the legacy CDSA stack behind `securityd`.
- **Files outside the home.** What the guest writes outside `--home` never reaches the host, and it can read system files only from the `--system` allowlist.
- **macOS 13 and 14 by range.** Their images keep the shared cache in a deflate-compressed ZIP member, which cannot be read by range. `--stream-compressed` downloads that one member (3.6–4.5 GB); without the flag the run stops and says so.
- **Shared memory through `mach_vm_remap`.** It copies instead of sharing, and only file-backed memory: enough for code (the Objective-C block trampolines), wrong for memory that is written afterwards.
- **Some CPU features.** The emulated core has a 44-bit address space and reports no DIT, SHA-3 or SHA-512 instructions.
- **System calls nobody has needed yet.** An unimplemented call stops the run with a message and a guest backtrace instead of guessing.

## What is tested

Restore images from macOS 11.0.1 to 27.2.1, with the guests in [`examples`](examples): a private framework, SQLite, Foundation, the Swift and Objective-C runtimes, CommonCrypto and `SecKey`. [`examples/matrix.sh`](examples/matrix.sh) repeats each guest across the releases and diffs every run against [`examples/expected`](examples/expected). Other guests will find system calls that are not implemented yet.

## How it works

Three packages, one per layer:

- [`rangefs`](rangefs) opens the restore image over HTTP ranges: ZIP directory → disk image (AEA-encrypted or UDIF) → GPT → APFS → file. A read fetches exactly the bytes it needs, and any response that is not exactly the requested range is refused.
- [`dyldcache`](dyldcache) parses the shared cache and produces pages rebased the way the kernel's shared-region pager would (slide info v2, v3, v5).
- [`emulator`](emulator) is a small user-mode Darwin "kernel" on [Unicorn](https://www.unicorn-engine.org). It starts the release's `/usr/lib/dyld` the way XNU does, lets it map its own shared cache, and pages contents in on first touch. BSD system calls, Mach traps and a few MIG routines are implemented just far enough for libSystem, CoreFoundation, Foundation, the Objective-C and Swift runtimes, Security and SQLite.
