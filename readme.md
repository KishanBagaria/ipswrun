# ipswrun

ipswrun runs an arm64 Mach-O binary against the system libraries of any macOS release since 11, fetching only the parts it needs from Apple's restore image (IPSW) for that release. You don't need a Mac running that release, a VM, or the ~26 GB ipsw image downloaded on disk.

Use it to see what a framework does or what a private API returns on every macOS release across history.

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

The first argument chooses the releases:

| Release | Runs against |
| --- | --- |
| `15.2` or `24C101` | that release: any since 11.0.1, by version or build |
| `latest` | the release Apple currently offers |
| `all` | the first and last release of every major version, and the current one |
| `15` | those of `all` under it: 15.0 and 15.6.1 |
| `https://…/UniversalMac_15.3_24D60_Restore.ipsw` | the restore image at that URL |
| `11,15.2,27.2.1` | several of the above |

Every release Apple had published when ipswrun was built is built in ([`releases.tsv`](cmd/ipswrun/releases.tsv)) and resolves with nothing looked up. A newer one is the current release in [Apple's feed](https://mesu.apple.com/assets/macos/com_apple_macOSIPSW/com_apple_macOSIPSW.xml), or, once it has been replaced, is looked up in the [ipsw.me](https://ipsw.me) index of the restore images Apple has published. The index is not Apple's, so ipswrun follows it only to an https URL on Apple's hosts, and refuses an image that turns out not to be the build it was listed as. A version no release has, such as `11.0`, stands for the releases under it (11.0.1). macOS 13 and 14 are skipped unless you pass [`--stream-compressed`](#what-does-not-work).

## Usage

```sh
ipswrun [flags] <release> <guest executable> [guest arguments...]
```

The program you run, the guest, is an ordinary executable built on a Mac:

```sh
clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains -o guest guest.c
```

`-no_fixup_chains` is what lets every release's dyld load it. A binary with only arm64e code, as Apple's own are, is loaded too. [`examples/Makefile`](examples/Makefile) has the flags for Objective-C and Swift too.

An executable can also be one of the release's own: `system:` and its path there, as in `system:/usr/sbin/cfprefsd`, takes it from the release that is being run.

What the guest gets:

- **That release's code.** It is loaded by the release's own dyld against the release's own dyld shared cache, so `dlopen` of any system framework, private ones included, gets the code that shipped.
- **A home directory**, `/Users/emu`. By default it is a temporary directory that is deleted after the run. Pass `--home <dir>` to keep what the guest writes there, which is the usual way to get results out. With several releases, give each its own: `--home 'out/{build}'`.
- **Standard output and error**, printed when the guest has finished rather than as it runs.
- **Its exit status**, which becomes ipswrun's. ipswrun itself exits 2 for a bad command line and 70 for a run that failed. With several releases it exits 70 if any run failed, otherwise 1 if any guest exited nonzero.

## Several guests

A guest can be given other guests to talk to: each is a process of its own on the same emulated machine, started beside it to provide Mach services, as launchd starts an agent for a user or a daemon for the system. [`examples/client.c`](examples/client.c) uses two services of [`examples/server.c`](examples/server.c), an XPC service and a plain Mach port:

```sh
./ipswrun --agent com.example.ipswrun.echo,com.example.ipswrun.reverse=examples/bin/server 27.2.1 examples/bin/client
```

```
echo 1: "HELLO, SERVER", the server's request 1; it is another process: 1, and it knows this one: 1
echo 2: "AND AGAIN", the server's request 2; it is another process: 1, and it knows this one: 1
reverse: answer 101 with 70000 bytes, reversed: 1, in memory of its own: 1; the server knows this process: 1
```

The other guest can be the release's own daemon. Preferences are kept by `cfprefsd`, which runs once as root for the system's and once for the user's. [`examples/prefs.c`](examples/prefs.c) then has its preferences from the daemon that shipped with that release:

```sh
./ipswrun --agent 'com.apple.cfprefsd.agent=system:/usr/sbin/cfprefsd agent' \
          --daemon 'com.apple.cfprefsd.daemon=system:/usr/sbin/cfprefsd daemon' 27.2.1 examples/bin/prefs
```

The run is the first guest's: its output, its exit status, and it ends when that guest exits, once the others have finished what they were doing or five seconds have passed. What another guest writes to its standard output or error is shown on standard error under its name. `--trace` shows the calls of every guest, each under its name, and what the XPC messages between them say.

## Options

| Flag | What it does |
| --- | --- |
| `--home <dir>` | host directory the guest sees as its home, `/Users/emu` (default: a temporary one, deleted afterwards). `{build}` and `{version}` are filled in per release, and one is required with several |
| `--mount <guest dir>=<host dir>` | another host directory the guest sees and may change, at that guest path, such as `--mount /var/db=out/db` (repeatable). `{build}` and `{version}` in the host directory as in `--home` |
| `--service <name>` | a Mach service the guest provides, as a daemon started by launchd does: it can check that name in, and look it up as a client (repeatable). Without it no name can be checked in, as for any program run by hand |
| `--agent <service>[,<service>…]=<executable> [arguments]` | another guest, started beside the first as the same user, that provides these Mach services to it (repeatable). The executable is a file, or `system:` and a path in the release. Arguments are separated by spaces |
| `--daemon <service>[,<service>…]=<executable> [arguments]` | like `--agent`, for a guest that runs as root, as a daemon of the system does (repeatable) |
| `--env KEY=VALUE` | added to the environment of every guest (repeatable) |
| `--system <path>` | system-volume file, or directory ending in `/`, the guest may read (repeatable). The default is feature flags, `SystemVersion.plist`, ICU data in `/usr/share/icu/` and `/usr/lib/libobjc-trampolines.dylib`; giving any replaces the default |
| `--cache <dir>` | where downloads are kept between runs (default: `ipswrun-<uid>` in the system temporary directory, so cleared on reboot) |
| `--stream-compressed` | allow downloading one compressed disk image member front to back, for macOS 13 and 14 |
| `--report <file>` | write a JSON summary: version, build, shared cache UUID, exit code, pages mapped, requests, bytes and seconds. `{build}` and `{version}` as in `--home` |
| `--timeout <seconds>` | how long the guest may run on each release, counted from when it starts (default 900) |
| `--trace` | print every emulated system call |
| `--missing` | answer a system call or Mach trap that is not implemented with an error instead of stopping, and list them all, with how often each was made and from where, when the guest ends |
| `--device <product>` | Mac model the disk images and the releases looked up are picked for (default `Macmini9,1`) |
| `--unicorn <dir>` | where the pinned `libunicorn` is kept (default: the user cache directory) |
| `--releases` | print the releases `all` stands for (version, build, URL), then exit |
| `--setup` | only fetch and verify `libunicorn`, then exit |

## Requirements

- **Host:** macOS on Apple silicon, or Linux on arm64 or x86-64 with glibc.
- **To build:** Go and a C compiler, since dependencies use cgo.
- **On Linux:** bring a guest built on a Mac. It is given a commpage recorded on an Apple silicon Mac.
- **Network:** the first run downloads a pinned, hash-verified `libunicorn` into the user cache directory. macOS 15 and later images are encrypted, and their key comes from Apple's key service.

## What does not work

- **Threads that never wait.** Threads take turns, those of every guest together: one runs until it waits for something (a lock, a condition, a semaphore, a queue, a timer), yields or ends, and only then does another. POSIX threads and libdispatch queues, groups, semaphores and timers work this way. A thread that spins without a system call until another thread does something waits forever, and a run where no thread can go on stops and lists what each was waiting for. Read-write locks that have to wait, signals between threads and thread cancellation are not there.
- **Processes nobody started, and the network.** The processes are the guests the command line names: a guest cannot start another (`fork`, `posix_spawn`). Between them, and inside each, Mach messages are real: ports and port sets, messages that wait to be received and receives that wait for them, memory and rights sent along, the notifications of a port losing its senders or its receiver, and on top of those libdispatch's Mach sources, `CFRunLoop` with its timers and ports, and XPC connections between a listener and a client. A service looked up with launchd exists if a guest provides it (`--service`, `--agent`, `--daemon`), and no other does. A guest is told who sent a message (its pid, and its user: root or the one user there is), and what that process's code signature says of it: the name it was signed under and its entitlements. Nothing checks a signature, so a guest is whatever it was signed as. Of the events libdispatch can wait for, timers and Mach messages happen: not files, sockets, signals or processes. `select` finds nothing ready.
- **Drivers.** A guest can ask IOKit for the kernel's services, open one and call its methods (`IOServiceGetMatchingServices`, `IOServiceOpen`, `IOConnectCallMethod` and what is built on them), but the kernel here has none: every search finds nothing, as on a Mac where the driver is not loaded. A program that uses the [`emulator`](emulator) package can give the machine drivers of its own (`Driver`); `ipswrun` gives it none. What needs a kernel extension to answer, as `fairplayd` needs FairPlayIOKit, stops there.
- **Memory sent in a message.** Memory mapped a second time is one memory: opened by name (`shm_open`), named by a port (`mach_make_memory_entry`) or remapped (`mach_vm_remap`), in one guest or in several. Memory sent out of line in a Mach message arrives as a copy, which is what it is on a Mac until one side writes to it.
- **Anything that needs a daemon that is not there.** Without `cfprefsd`, `NSUserDefaults` reads back nothing. With it (see [Several guests](#several-guests)), preferences are set and read back through the daemon, but it does not write them to the home: it makes the file and removes it again, for a reason not yet found. On macOS 11 to 12.0.1 the daemon also reports that synchronizing failed. `SecKeyCreateRandomKey` fails unless the key is made the iOS way (`kSecUseDataProtectionKeychain`, not permanent), because the macOS default is the legacy CDSA stack behind `securityd`.
- **Files outside the home.** What the guest writes outside `--home` and the `--mount` directories never reaches the host, and it can read system files only from the `--system` allowlist.
- **macOS 13 and 14 by range.** Their images keep the shared cache in a deflate-compressed ZIP member, which cannot be read by range. `--stream-compressed` downloads that one member (3.6–4.5 GB).
- **`mach_vm_remap` of a file's memory.** It copies instead of sharing: enough for code (the Objective-C block trampolines), wrong for a mapped file that is written afterwards.
- **Some CPU features.** The emulated core has a 44-bit address space and reports no DIT, SHA-3 or SHA-512 instructions.
- **System calls nobody has needed yet.** An unimplemented call stops the run with a message and a guest backtrace instead of guessing. `--missing` carries on past them, to find in one run everything a guest needs.

## What is tested

Restore images from macOS 11.0.1 to 27.2.1, with the guests in [`examples`](examples): a private framework, SQLite, Foundation, the Swift and Objective-C runtimes, CommonCrypto and `SecKey`, POSIX threads, libdispatch and Swift concurrency, Mach messages with what is built on them (libdispatch sources, `CFRunLoop`, XPC), a Mach service with its client in one process and in two, and preferences kept by the release's `cfprefsd`. [`examples/matrix.sh`](examples/matrix.sh) repeats each guest across the releases of `all` and diffs every run against [`examples/expected`](examples/expected).

## How it works

Three packages, one per layer:

- [`rangefs`](rangefs) opens the restore image over HTTP ranges: ZIP directory → disk image (AEA-encrypted or UDIF) → GPT → APFS → file. A read fetches exactly the bytes it needs, and any response that is not exactly the requested range is refused.
- [`dyldcache`](dyldcache) parses the shared cache and produces pages rebased the way the kernel's shared-region pager would (slide info v2, v3, v5).
- [`emulator`](emulator) is a small user-mode Darwin "kernel" on [Unicorn](https://www.unicorn-engine.org), with an engine for each guest. It starts the release's `/usr/lib/dyld` the way XNU does, lets it map its own shared cache, and pages contents in on first touch. BSD system calls, Mach traps and a few MIG routines are implemented just far enough for libSystem, CoreFoundation, Foundation, the Objective-C and Swift runtimes, Security and SQLite.
