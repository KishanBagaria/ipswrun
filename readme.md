# ipswrun

ipswrun runs an arm64 Mach-O binary against the system libraries of any macOS release since 11 or iOS release since 14, fetching only the parts it needs from Apple's restore image (IPSW) for that release. You don't need a device running that release, a VM, or the multi-gigabyte image downloaded on disk.

Use it to see what a framework does or what a private API returns on every release across history.

## Try it

On an Apple silicon Mac with Go 1.27+:

```sh
go build -o ipswrun ./cmd/ipswrun
make -C examples bin/guest

./ipswrun 11.0.1,27.2.1 examples/bin/guest
```

The same executable, [`examples/guest.c`](examples/guest.c), reports the system it found itself in, six years apart:

```
==> macOS 11.0.1 (20B29)
macOS 11.0.1 (20B29)
loaded /System/Library/PrivateFrameworks/SoftLinking.framework/SoftLinking
sqlite 3.32.3: 1 row(s) in /Users/emu/guest.sqlite
==> macOS 27.2.1 (26B131)
macOS 27.2.1 (26B131)
loaded /System/Library/PrivateFrameworks/SoftLinking.framework/SoftLinking
sqlite 3.54.0: 1 row(s) in /Users/emu/guest.sqlite
```

The `--device` decides the system. A Mac (the default, `Macmini9,1`) runs macOS, and an iPhone or iPad runs iOS. The same source built for iOS (this needs Xcode, not only its command line tools):

```sh
make -C examples PLATFORM=ios bin/ios/guest

./ipswrun --device iPhone11,2 14.0,18.7.12 examples/bin/ios/guest
```

```
==> iOS 14.0 (18A373)
iOS 14.0 (18A373)
loaded /System/Library/PrivateFrameworks/SoftLinking.framework/SoftLinking
sqlite 3.32.3: 1 row(s) in /var/mobile/guest.sqlite
==> iOS 18.7.12 (22H390)
iOS 18.7.12 (22H390)
loaded /System/Library/PrivateFrameworks/SoftLinking.framework/SoftLinking
sqlite 3.43.2: 1 row(s) in /var/mobile/guest.sqlite
```

A release typically fetches 150–600 MB and takes from twenty seconds to a few minutes, nearly all of it download. Running the same release again reuses what was fetched.

## Every release

`all` runs the guest against the first and last release of every major version:

```sh
./ipswrun all ./zlib-version
```

```
==> macOS 11.0.1 (20B29)
zlib 1.2.11
==> macOS 11.6 (20G165): same as macOS 11.0.1 (20B29)
==> macOS 12.0.1 (21A559): same as macOS 11.0.1 (20B29)
==> macOS 12.6 (21G115): same as macOS 11.0.1 (20B29)
==> macOS 15.0 (24A335)
zlib 1.2.12
==> macOS 15.6.1 (24G90): same as macOS 15.0 (24A335)
...
```

A release whose output and exit status repeat an earlier one says `same as`, so the output shows where behaviour changed. A release that fails does not stop the ones after it.

The first argument chooses the releases, of macOS or iOS as the `--device` runs:

| Release | Runs against |
| --- | --- |
| `15.2` or `24C101` | that release, by version or build |
| `26C5047e` | a macOS beta build from the bundled AppleDB snapshot, resolved locally for the selected device. For newer betas, pass the Apple IPSW URL |
| `latest` | the newest release |
| `all` | the first and last release of every major version, and the newest |
| `15` | those of `all` under it: 15.0 and 15.6.1 |
| `15.*` | every release of version 15 |
| `15.0..15.6.1` or `24A335..24G90` | every release between the two inclusive, oldest first (also works with `latest` or beta builds) |
| `https://…/UniversalMac_15.3_24D60_Restore.ipsw` | the restore image at that URL |
| `/path/to/UniversalMac_Restore.ipsw` | a local restore image on disk |
| `11,15.2,27.2.1` | several of the above |

Every macOS release Apple had published when ipswrun was built is built in ([`releases.tsv`](cmd/ipswrun/releases.tsv)), as is every iOS and iPadOS release since 14 for each iPhone and iPad it runs ([`ios.tsv`](cmd/ipswrun/ios.tsv)), and these resolve with nothing looked up. A newer one is the current release in Apple's feed ([macOS](https://mesu.apple.com/assets/macos/com_apple_macOSIPSW/com_apple_macOSIPSW.xml), [iOS](https://itunes.apple.com/WebObjects/MZStore.woa/wa/com.apple.jingle.appserver.client.MZITunesClientCheck/version)), or, once it has been replaced, is looked up in the [ipsw.me](https://ipsw.me) index of the restore images Apple has published. The index is not Apple's, so ipswrun follows it only to an https URL on Apple's hosts, and refuses an image that turns out not to be the build it was listed as. A version no release has, such as `11.0`, stands for the releases under it (11.0.1). macOS 13 and 14 and iOS 16 and 17 are skipped unless you pass [`--stream-compressed`](#what-does-not-work).

## Usage

```sh
ipswrun [flags] <release> <guest executable or source> [guest arguments...]
```

On macOS with Xcode, pass a single C, Objective-C, or Swift file directly:

```sh
./ipswrun all examples/foundation.m
```

ipswrun compiles the file once into a temporary arm64 executable for the system the `--device` runs, macOS 11 or iOS 14 and later, with the flags of [`examples/Makefile`](examples/Makefile): Objective-C guests get ARC and Foundation. The executable of an `--agent` or `--daemon` may be a source file too. For extra libraries, custom compiler flags, or entitlements, pass a prebuilt executable.

The program you run, the guest, is an ordinary executable built on a Mac:

```sh
clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains -o guest guest.c
xcrun --sdk iphoneos clang -arch arm64 -mios-version-min=14.0 -Wl,-no_fixup_chains -o guest guest.c
```

`-no_fixup_chains` is what lets every release's dyld load it. A binary with only arm64e code, as Apple's own are, is loaded too. [`examples/Makefile`](examples/Makefile) has the flags for Objective-C and Swift too; `make -C examples PLATFORM=ios` builds the examples for iOS into `examples/bin/ios`.

An executable can also be the release's own, by `system:` and its path there: `system:/usr/libexec/dirhelper`.

What the guest gets:

- **That release's code.** It is loaded by the release's own dyld against the release's own dyld shared cache, so `dlopen` of any system framework, private ones included, gets the code that shipped.
- **A home directory**: `/Users/emu` on macOS, as the user `emu`, and `/var/mobile` on iOS, as the user `mobile`. By default it is a temporary directory that is deleted after the run. Pass `--home <dir>` to keep what the guest writes there, which is the usual way to get results out. With several releases, give each its own: `--home 'out/{build}'`.
- **The device it asked for.** `hw.machine` and `hw.model` name the `--device`, and on iOS its board.
- **Standard output and error.** Run against one release, what the guest writes is passed on as it writes it. With several releases, and for a guest started beside the first, it is kept until the run ends, which is what lets a release that repeats an earlier one say `same as`; a guest that writes more than 64 MB then stops the run.
- **Its exit status**, which becomes ipswrun's. ipswrun itself exits 2 for a bad command line and 70 for a run that failed. With several releases it exits 70 if any run failed, otherwise 1 if any guest exited nonzero.

## Several guests

Other guests can be started beside the first to provide Mach services to it, each a process of its own: `--agent` as the same user, `--daemon` as root. [`examples/client.c`](examples/client.c) uses an XPC service and a plain Mach port of [`examples/server.c`](examples/server.c):

```sh
./ipswrun --agent com.example.ipswrun.echo,com.example.ipswrun.reverse=examples/bin/server 27.2.1 examples/bin/client
```

```
echo 1: "HELLO, SERVER", the server's request 1; it is another process: 1, and it knows this one: 1
echo 2: "AND AGAIN", the server's request 2; it is another process: 1, and it knows this one: 1
reverse: answer 101 with 70000 bytes, reversed: 1, in memory of its own: 1; the server knows this process: 1
```

The other guest can be one of the release's own daemons, which decides whom it serves by what the client's code signature says: [`examples/prefs.c`](examples/prefs.c) and [`examples/adi.c`](examples/adi.c) each run beside one.

The run is the first guest's: its output and its exit status. When it exits, the others are sent SIGTERM if they watch for it and have five seconds to finish. What they wrote is shown on standard error under their names, and `--trace` shows every guest's calls and what the messages between them say.

## Options

| Flag | What it does |
| --- | --- |
| `--home <dir>` | host directory the guest sees as its home, `/Users/emu` or `/var/mobile` (default: a temporary one, deleted afterwards). `{build}` and `{version}` are filled in per release, and one is required with several |
| `--mount <guest dir>=<host dir>` | another host directory the guest sees and may change, at that guest path, such as `--mount /var/db=out/db` (repeatable). `{build}` and `{version}` in the host directory as in `--home` |
| `--service <name>` | a Mach service the guest provides, as a daemon started by launchd does: it can check that name in, and look it up as a client (repeatable). Without it no name can be checked in, as for any program run by hand |
| `--agent <service>[,<service>…]=<executable> [arguments]` | another guest, started beside the first as the same user, that provides these Mach services (repeatable). Arguments are separated by spaces |
| `--daemon <service>[,<service>…]=<executable> [arguments]` | the same, as root |
| `--env KEY=VALUE` | added to the environment of every guest (repeatable). `--env KEY` takes a variable out of it |
| `--system <path>` | system-volume file, or directory ending in `/`, the guest may read (repeatable). The default is feature flags, `SystemVersion.plist`, ICU data in `/usr/share/icu/`, `/usr/lib/libobjc-trampolines.dylib` and the sandbox profiles; giving any replaces the default |
| `--cache <dir>` | where downloads are kept between runs (default: `ipswrun/cache` in the user cache directory) |
| `--prune-cache` | remove cached restore image downloads from `--cache`, skipping any a running ipswrun has open, then exit |
| `--stream-compressed` | allow downloading one compressed disk image member front to back, for macOS 13 and 14 and iOS 16 and 17 |
| `--report <file>` | write a JSON summary: version, build, shared cache UUID, exit code, pages mapped, requests, bytes and seconds. `{build}` and `{version}` as in `--home` |
| `--timeout <seconds>` | how long the guest may run on each release, counted from when it starts (default 900). Time the host spends fetching is not counted, here or on any clock the guests see |
| `--bisect` | binary-search the selected releases for where the guest's output or exit status changed (exits 0 when found, 1 when both endpoints match, 70 on failure) |
| `--jobs <n>` | releases to run concurrently when running several (default 1); output is still printed in release order |
| `--trace` | print every emulated system call |
| `--missing` | answer a system call or Mach trap that is not implemented with an error instead of stopping, and list them all, with how often each was made and from where, when the guest ends |
| `--device <product>` | the device whose releases run (default `Macmini9,1`): a Mac runs macOS, an iPhone or iPad (from the A12: `iPhone11,2`, `iPad8,1` and later) runs iOS |
| `--unicorn <dir>` | where the pinned `libunicorn` is kept (default: the user cache directory) |
| `--releases` | print the releases `all` stands for (version, build, URL), then exit |
| `--setup` | only fetch and verify `libunicorn`, then exit |

## Requirements

- **Host:** macOS on Apple silicon, or Linux on arm64 or x86-64 with glibc.
- **To build:** Go and a C compiler, since dependencies use cgo.
- **On Linux:** bring a guest built on a Mac. It is given a commpage recorded on an Apple silicon Mac.
- **Network:** the first run downloads a pinned, hash-verified `libunicorn` into the user cache directory. macOS 15 and later and iOS 18 and later images are encrypted, and their key comes from Apple's key service.

## What does not work

[`limits.md`](limits.md) ranks more of what does not work yet, and which of it looks fixable.

- **Threads that never wait.** Threads take turns, those of every guest together: one runs until it waits for something (a lock, a condition, a semaphore, a queue, a timer), yields or ends, and only then does another. POSIX threads and libdispatch queues, groups, semaphores and timers work this way. A thread that spins without a system call until another thread does something waits forever, and a run where no thread can go on stops and lists what each was waiting for. Signals between threads and thread cancellation are not there.
- **Signals.** What a guest says about them (`sigaction`, `sigprocmask`, `sigaltstack`) is kept and read back, but none is ever delivered: `kill`, `pthread_kill` and any CPU fault, a guard page's included, end the run, and a `setitimer` that would start a timer stops it as unimplemented.
- **Processes nobody started, and the network.** The processes are the guests the command line names: a guest cannot start another (`fork`, `posix_spawn`). Between them, and inside each, Mach messages are real: ports and port sets, messages that wait to be received and receives that wait for them, memory and rights sent along, the notifications of a port losing its senders or its receiver, and on top of those libdispatch's Mach sources, `CFRunLoop` with its timers and ports, and XPC connections. A service exists if a guest provides it (`--service`, `--agent`, `--daemon`). A guest is told the sender's pid and user (root, or the one user there is) and what its code signature says: nothing checks a signature, so a guest is whatever it was signed as. Of the events libdispatch can wait for, timers, Mach messages, pipes that can be read or written and the SIGTERM at the end of a run happen: not files, sockets or processes. Pipes work within a guest, with `select` for them and `NSPipe` and `dispatch_io` on them; of any other descriptor `select` finds nothing ready, and a `kevent` that waits on a kqueue of the guest's own stops as unimplemented, since what would have happened cannot be known.
- **What only an iPhone has.** There is no SpringBoard or backboardd, so no UIKit app, and MobileGestalt answers little: the marketing name, and no product type.
- **Hardware.** The I/O Registry holds the device tree the restore image has for the `--device` (`IODeviceTree:/product` and the rest, which the release's own `ioreg -p IODeviceTree` prints), without what the boot loader or a driver adds on a device: no serial number, no NVRAM, and no class or location for an entry ([`limits.md`](limits.md#the-io-registry)). A guest can search IOKit, open a service and call its methods, but the kernel here has no services but the device itself: every other search finds nothing, as where the driver is not loaded. A program using the [`emulator`](emulator) package can give the machine a `Driver`; `ipswrun` gives it none.
- **Memory sent in a message** arrives as a copy. Memory mapped a second time is one memory (`shm_open`, `mach_make_memory_entry`, `mach_vm_remap`), in one guest or several, except memory of the shared cache or already shared in part.
- **Anything that needs a daemon that is not there.** Without `cfprefsd`, `NSUserDefaults` reads back nothing. With it, preferences are set and read back but never reach the home, for a reason not yet found. `SecKeyCreateRandomKey` fails unless the key is made the iOS way (`kSecUseDataProtectionKeychain`, not permanent), because the macOS default is the legacy CDSA stack behind `securityd`.
- **Files outside the home.** What the guest writes outside `--home` and the `--mount` directories never reaches the host, and it can read system files only from the `--system` allowlist.
- **macOS 13 and 14, and iOS 16 and 17, by range.** Their images keep the shared cache in a deflate-compressed ZIP member, which cannot be read by range. `--stream-compressed` downloads that one member (3.6–4.5 GB for macOS, 1.1–1.4 GB for iOS).
- **`mach_vm_remap` of a file's memory.** It copies instead of sharing: enough for code (the Objective-C block trampolines), wrong for a mapped file that is written afterwards.
- **Some CPU features.** The emulated core has a 44-bit address space and reports no DIT, SHA-3 or SHA-512 instructions.
- **Accelerate's BLAS and LAPACK.** Every release's libBLAS does matrix work such as `cblas_sgemm` on Apple's matrix coprocessor (AMX), whatever chip it is told it runs on, and the emulated core has none: the run stops at the first AMX instruction and says so.
- **iOS 12 and 13.** Their system volume cannot be read by range yet, so they are left out of an iPhone's or iPad's releases.
- **On iOS 14.0, the files `foundation` writes in the home.** They are not written, for a reason not yet found.
- **System calls nobody has needed yet.** An unimplemented call stops the run with a message and a guest backtrace instead of guessing. `--missing` carries on past them, to find in one run everything a guest needs.

## What is tested

Restore images from macOS 11.0.1 to 27.2.1 and from iOS 14.0 to 27.2.1, with the guests in [`examples`](examples): a private framework, SQLite, Foundation, the Swift and Objective-C runtimes, CommonCrypto and `SecKey`, POSIX threads, libdispatch and Swift concurrency, a file's mode, dates, extended attributes and locks with signal state and `mprotect`, Mach messages with what is built on them (libdispatch sources, `CFRunLoop`, XPC), a Mach service with its client in one process and in two, daemons of the release's own serving a client, the I/O Registry's device tree, the status of a port, and pipes, by themselves and as libdispatch watches them. [`examples/matrix.sh`](examples/matrix.sh) repeats each guest across the releases of `all` and diffs every run against [`examples/expected`](examples/expected); `--device=iPhone11,2` and `--device=iPhone15,2` do the same for iOS. `adi` is not run on iOS, where the only `adid` is FairPlay's.

## How it works

Three packages, one per layer:

- [`rangefs`](rangefs) opens the restore image over HTTP ranges or from disk: ZIP directory → disk image (AEA-encrypted or UDIF) → GPT → APFS → file. A read fetches exactly the bytes it needs, and any response that is not exactly the requested range is refused.
- [`dyldcache`](dyldcache) parses the shared cache and produces pages rebased the way the kernel's shared-region pager would (slide info v2, v3, v5). It also reads which platform the cache was built for.
- [`emulator`](emulator) is a small user-mode Darwin "kernel" on [Unicorn](https://www.unicorn-engine.org), with an engine for each guest. It starts the release's `/usr/lib/dyld` the way XNU does, lets it map its own shared cache, and pages contents in on first touch. What macOS and iOS expect differently of it (where memory goes, what the system says it is) follows the cache's platform. BSD system calls, Mach traps and a few MIG routines are implemented just far enough for libSystem, CoreFoundation, Foundation, the Objective-C and Swift runtimes, Security and SQLite.
