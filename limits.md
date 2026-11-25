# Limits

What ipswrun cannot run or do yet, beyond the short list in the [readme](readme.md#what-does-not-work), ranked by how much real-world code each limit blocks. "Fixable" means a fix keeps to the project's rules: no guessed success for unimplemented calls, nothing keyed to one build, no kext emulation, and no replacing Apple's libraries.

The biggest gap is that a guest cannot start another process, and nothing starts Apple's daemons on demand, so most frameworks quietly lose the services they lean on.

This list was made by reading the code, and then checked by one [survey](#survey-of-a-releases-own-binaries) of a release's own binaries. The [last section](#suspicions-not-yet-tested) lists what most needs a run to confirm.

## Ranked limits

| # | Limit | What it blocks | Where in the code | Fixable? |
| --- | --- | --- | --- | --- |
| 1 | No process creation: `fork`, `posix_spawn` and `execve` have no handler and stop the run | `NSTask`, `system()`, `popen`, every command-line tool or daemon that starts a helper, XPC services embedded in frameworks | `emulator/bsd.go`, handler table | Yes. `posix_spawn` maps onto the existing multi-guest `Machine`; `fork` is harder (it copies an engine's pages) |
| 2 | No launch on demand: a Mach service exists only if it is named with `--service`, `--agent` or `--daemon` | Frameworks silently lose notifyd, distnoted, logd, trustd, securityd, tccd, fontd, opendirectoryd and cfprefsd unless each is wired by hand; daemons that need other daemons | `emulator/launchd.go` | Yes. Read the release's own LaunchDaemons and LaunchAgents plists and start the daemon on the first lookup of its service |
| 3 | No signal delivery: `sigaction`, `sigprocmask` and `sigaltstack` are kept and read back, but a handler never runs; `kill` and `pthread_kill` end the run, as does any CPU fault (a guard page included); `setitimer` that starts a timer stops as unimplemented | Crash handlers, `SIGALRM` timeouts, guard-page tricks, Mach exception ports; `abort()` makes ipswrun exit 70 instead of the guest exiting 134 | `emulator/bsd.go` (`sysSigaction`, `sysSetitimer`, `sysKill`), `emulator/process.go` (`interrupt`) | Yes. Build a signal frame and return through `__sigtramp` and `sigreturn` |
| 4 | Descriptor I/O beyond files and pipes: no `socketpair` or sockets (`socket` answers EACCES), and no `poll`. Pipes are there ([below](#pipes)): `select` answers for them, and libdispatch is told when one can be read or written. A kqueue a guest makes itself is still not emulated: `kevent` and `kevent64` on it take changes and answer `EV_RECEIPT`, but a wait for events stops as unimplemented. `EVFILT_READ` and `EVFILT_WRITE` of a descriptor that is not a pipe, `EVFILT_VNODE` and `EVFILT_PROC` never fire, and of such a descriptor `select` finds nothing ready | `CFSocket`, Unix-socket clients (syslog, mDNSResponder), any network code, programs with an event loop of their own on `kqueue` or `poll`, `dispatch_io` on a file | `emulator/pipes.go`, `emulator/kevent.go` (`change`) | Socketpairs and a guest's own kqueue, yes. A real network is a policy call, since it breaks reproducibility |
| 5 | Accelerate's BLAS and LAPACK: every release's libBLAS runs `cblas_sgemm` and the like on Apple's matrix coprocessor (AMX), whatever chip the guest is told it has, and Unicorn has no AMX; the run stops at the first AMX instruction and names it | Linear algebra through Accelerate, and what is built on it (CoreML's and BNNS's CPU paths, likely vDSP's matrix calls) | `emulator/process.go` (`amx`, `interrupt`) | Yes, [below](#amx-emulation): emulate the coprocessor in the invalid-instruction hook |
| 6 | No hardware beyond the device tree: the I/O Registry holds the restore image's device tree and nothing a driver or the boot loader adds ([below](#the-io-registry)), and a search finds no service but the device itself | Serial number and platform UUID, NVRAM, power, HID, most of MobileGestalt (it answers the marketing name, and no product type) | `emulator/registry.go`, `emulator/iokit.go` | Partly: each further fact needs a source in the restore image |
| 7 | Shared file memory: `MAP_SHARED` of a real file is a private copy, and there is no `msync`. Protections are kept per region (`mmap`, `mprotect`, `mach_vm_protect`, the `mach_vm_map` trap, the shared cache's own) and a forbidden access faults, but memory from the MIG `vm_map` and `mach_vm_remap` is mapped with every protection | SQLite WAL `-shm` files shared by two connections or processes; anything that writes a mapped file | `emulator/bsd.go` (`sysMmap`), `emulator/mach.go` | Shared file mappings, yes, through the `shm_open` path |
| 8 | Threads take turns on one CPU, with no preemption, and `hw.ncpu` is 1 | Spin-waits without a system call hang; nothing runs in parallel, so compute-heavy guests are slow | `emulator/threads.go` | Hard: Unicorn does not resume reliably after a stop from outside a hook |
| 9 | The system volume is closed by default: only feature flags, `SystemVersion.plist`, ICU data, the Objective-C trampolines and the sandbox profiles can be read, and the directories (and the symbolic links among them) on the way to the shared cache's images, which dyld walks, listing only what leads there | Frameworks' `Resources` (Info.plist, `.strings`, `.car`), fonts and other `/System/Library` data: frameworks behave differently with no error | `emulator/build.go` (`DefaultSystem`) | Yes, it is a default; `--system` already opens more |
| 10 | Device and OS coverage: iPhones and iPads from the A12 only; no tvOS, watchOS, visionOS or bridgeOS; no macOS before 11 | iOS 14 to 16 on A9 to A11 devices; the other Apple platforms | `cmd/ipswrun` device tables | A9 to A11 (plain arm64 caches) and tvOS look reachable; watchOS is arm64_32; macOS before 11 has no arm64 code |
| 11 | Hosts: an Apple silicon Mac, or Linux on arm64 or x86-64 with glibc; guests have to be built on a Mac | Intel Macs, Windows, musl Linux, CI without a Mac | `emulator/artifact.go` (pinned libunicorn builds) | Intel Macs and musl need a libunicorn build to pin; building guests on Linux needs an SDK |
| 12 | What the system says of itself is left out where it isn't known for sure ([below](#sysctls-left-out)) | A guest that needs one of those sysctls gets ENOENT, as from a kernel without it | `emulator/bsd.go` (`sysctlValue`, `cpuFamilies`) | Yes, one value at a time, from a real device or the restore image |

## What to fix first

These four would unlock the most code for the least work, and each keeps to the rules above.

1. **Launch on demand from the release's own plists.** On a lookup of an unknown name, find the plist under `/System/Library/LaunchDaemons` or `LaunchAgents` whose `MachServices` holds it, and start its `Program` as a daemon or agent. This makes the hand-wired `prefs` and `adi` setups the default, and shows for each release which daemon a framework actually needed. Each daemon that starts will surface missing calls of its own, so expect follow-up work.
2. **`posix_spawn` of system binaries.** A spawned child becomes another `Process` on the same `Machine`, with its own pid, its standard descriptors wired to the parent's, and `wait4` for its exit. It needs pipes (the next item) to be useful for `NSTask`. `fork` can wait.
3. **Socketpairs, `poll`, and a kqueue of the guest's own.** Pipes are there, with `select` and with libdispatch's sources, `dispatch_io`, `NSPipe` and `NSFileHandle` on top. What a pipe has, a socketpair needs again, and plain `kevent` still cannot wait.
4. **Signal delivery.** Run the handlers `sigaction` now keeps, deliver `kill` and `pthread_kill` to the target thread, and turn a CPU fault into SIGSEGV or SIGBUS when a handler is installed. Without a handler the run still ends, but with the exit status a real process gets (134 for `abort`).

## Stubs that answer success

The `trivial` map in `emulator/bsd.go` used to answer 0 for calls that did nothing. These now do what they say, or stop as unimplemented where they cannot:

| Call | Now |
| --- | --- |
| `chmod`, `fchmod`, `chown`, `lchown`, `fchown` | Change the host file under the home or a mount. A file of the system volume is refused as a real one is: EPERM for the user, who does not own it, EROFS for root. A guest's executable and devices are EPERM. `chown` to the owner a file already has succeeds, to another is EPERM for the user, and stops as unimplemented for root |
| `setxattr`, `getxattr`, `listxattr`, `removexattr` and their `f` forms | Kept in a table in the `Machine`, keyed by the host file, so they read back alike on macOS and Linux hosts and between guests; dropped with the file's last link. System files have none and take none |
| `utimes`, `futimes`, `setattrlist`, `fsetattrlist` | Set the host file's dates; `setattrlist` also owner, group and mode, and stops as unimplemented for any other attribute |
| `flock` | The host's `flock`, so guests sharing a mount lock each other, and so do host processes. A blocking lock waits like the other kinds of wait |
| `mprotect`, `vm_protect` | Tracked per region; a forbidden access faults (see limit 7). It costs 2–4% on the example guests |
| `setrlimit`, `getrlimit` | Start as launchd gives them (no core files; an 8 MB stack, less a guard page, of at most 64 MB; 256 descriptors), and change as the kernel allows: a soft limit no higher than the hard one, a hard one raised only by root. The descriptors' limit is kept to (EMFILE). RLIMIT_NPROC's real value depends on the device's memory and is not known, so it stays unlimited |
| `fsync`, `sync` | The host's, for host files and the standard descriptors (EINVAL for a pipe, as on a Mac); 0 for what is read from the system volume. `fsync` of shared memory or a kqueue stops as unimplemented |
| `umask` | Kept, from launchd's 022, and applied to the files and directories a guest creates in place of the host's own umask |
| `getpriority`, `setpriority` | The process's nice value; a user may raise it but not lower it again. Darwin's own kinds (`PRIO_DARWIN_THREAD` and the like) stop as unimplemented |
| `iopolicysys` | Kept and read back by scope and type. Setting a policy that would change what files show (access times, skipped modification times, ignored permissions) stops as unimplemented |
| `memorystatus_get_level` | 50, as `kern.memorystatus_level` and `host_statistics` say |
| `setsid` | EPERM, as for a process launchd started, which already leads its session and group |
| `sigaction`, `sigprocmask`, `sigaltstack`, `setitimer` | See limit 3 |
| `kevent`, `kevent64` | See limit 4 |

The rest of what the `trivial` map answered 0 now has no handler, and stops the run: `memorystatus_control`, `proc_rlimit_control`, `thread_selfusage`, `kqueue_workloop_ctl`, `audit_session_self`, `debug_syscall_reject_config`, `debug_syscall_reject`, `record_system_event`, `log_data`, `oslog_coproc_reg` and `oslog_coproc`. None of the example guests calls them on the releases tested. `objc_bp_assist_cfg_np`, which libobjc of iOS 16.0 calls as it starts, fails with ENOTSUP, as on a core without the hardware that assists Objective-C breakpoints.

Still to check against a real kernel: the map's constant errors (`necp_open` and `getaudit_addr`, `coalition_info` and `persona` answer ENOSYS, `socket` EACCES, `work_interval_ctl` and `crossarch_trap` ENOTSUP), which fail but perhaps not as a Mac does..

## `__mac_syscall`

The calls guests make are answered as a Mac's kernel answers a process that is not sandboxed, as [examples/macsyscall.c](examples/macsyscall.c) measures them (run natively on macOS 27.2.1, with the arguments the guests pass). How cfprefsd uses them was checked by running a re-signed copy of this Mac's own cfprefsd under lldb.

| Call | Answer |
| --- | --- |
| AMFI 90, `amfi_check_dyld_policy_self` | 0, with the flags 0x1df |
| Sandbox 2, a check, of any process (even one that is not there) | 0, with a result of 0, allowed, for the filter types the guests use (none, a path, 5, 6, 0x1c, 0x41, a descriptor), except a path that does not exist, whose result is 2. That 2 was measured on this Mac only. A descriptor (0xf0) is EBADF, and the result left as it was, if it is not open |
| Sandbox 4, a query of a process | ENOTSUP for its container (kind 0), ESRCH for kind 1; the buffer is not written |
| Sandbox 12, `sandbox_passthrough_access`, as CFPreferences calls it for a plist it opened | 0 for a regular file with the flags 0x2 and 0x100 or neither; EACCES for one with 0x1 and for a directory; EBADF for a descriptor that is not open |
| Sandbox 22, a profile the kernel has by name | ENOENT, as on this Mac for cfprefsd's: libsandbox then compiles the profile itself |
| Sandbox 0, applying a compiled profile | 0, and nothing is confined (below) |
| Sandbox 67, `sandbox_get_container_expected` (iOS 18 and later, as every process starts) | 0, with both words it is given set to 0: no container is expected |
| Quarantine 87, `_qtn_proc_apply_to_pid` | 0, nothing written |

**A deliberate exception: a profile that is applied confines nothing.** cfprefsd enters its sandbox as it starts, and a real kernel would hold it to its profile from then on. Here applying the profile succeeds and changes nothing, so a guest can do whatever its profile forbids, and sandbox behaviour can't be studied with ipswrun. This was chosen over stopping, so that cfprefsd, and the prefs guest beside it, run.

Everything else stops as unimplemented: applying a profile by any other call (Sandbox 1, the kernel's own profiles), issuing or consuming a sandbox extension (Sandbox 5 and 6 on this Mac, whose token is the kernel's), and any other policy, call, filter type, flags, descriptor or process not measured.

A guest may read the sandbox profiles (`/System/Library/Sandbox/Profiles/` and `/usr/share/sandbox/`, among the defaults of `--system`), so cfprefsd compiles its own and applies it (Sandbox 0) as on a Mac. Without them a daemon's `sandbox_init` fails with "profile not found": cfprefsd then cannot synchronize on macOS 15 and later, and in the survey 56 programs said so as they started.

The answers were measured on a Mac; iOS guests get the same ones, which is not known to be what an iPhone's kernel answers.

## Preferences on iOS 16 and later

The prefs guest, run beside the release's own cfprefsd, prints `synchronized: 0` on every iOS release from 16.0 that was run (16.0, 18.0, 18.7.12, 26.0, 26.6.2, 27.0, 27.2.1): CoreFoundation never looks up `com.apple.cfprefsd.daemon`, so `CFPreferencesAppSynchronize` returns false, while what was set still reads back from the process itself. On iOS 14 and 15 it looks the daemon up (launchd's routine 804) and synchronizes. Why it doesn't is not known yet; it was so before the sandbox calls were answered.

## The I/O Registry

The registry is the device tree the restore image has for the `--device`, shown as the plane `IODeviceTree`, with the registry's root above it. It is read from the image when a guest first asks. [examples/registry.c](examples/registry.c) prints the same run natively on a Mac15,9 with macOS 27.2.1 as under ipswrun for that device and release ([native.txt](examples/expected/registry/native.txt)), and the release's own `ioreg -p IODeviceTree` prints the tree.

The image holds the tree as the boot loader is given it, not as the boot loader leaves it. What is left out, so as not to be invented:

- **A value the image marks as one for the boot loader to fill,** and **a value that is all zeros**, which is how the image holds the place of most of the others. Measured against that Mac's live registry, over the 42 entries `ioreg` shows of its tree: of the 505 unmarked values the image has, 312 are not zeros, and all but one of those are what the Mac has (the one, `pmap-io-ranges` of `/defaults`, is longer on the Mac); 189 are zeros, of which the Mac has 140 as they are and 49 filled in (`chip-id`, `board-id`, `firmware-version` and the like in `/chosen`). So a property that really is zero (`has-exclaves`) is not there to read either.
- **What only a running device has:** its serial number and platform UUID, everything in `/options` (the NVRAM, which is there as an entry with no properties), the entries the boot loader adds under `/chosen`, and the properties drivers add.
- **Classes, locations and identifiers.** An entry's class is its driver's, so only the device's own (`IOPlatformExpertDevice`, which the kernel itself makes of the tree) is answered; a search for that class finds it, and for any other class finds nothing. `io_object_get_class` of another entry, an entry's location, its registry ID and its path are not answered: the guest gets a send error, and the request is listed when the run fails or under `--missing`. A path with a location in it (`cpu0@0`) stops as unimplemented.
- **The properties of the root, and of an entry the kernel keeps to itself** (`kernel-only`, such as `/chosen/memory-map`), which a Mac shows emptied: they stop as unimplemented.
- **The order of the entries under one.** They come in the tree's order. A Mac's kernel has the device's in another (`chosen`, `aliases`, `memory`, `options`, `cpus`, where the tree has `chosen`, `options`, `cpus`, `aliases`, `memory`).
- **Every plane but the tree's.** Among the services there is the root and the device and nothing under it, as no driver is here: `IOService:/IOResources/AppleKeyStore`, which is there on every device, is not. A search or walk that would leave the device tree's plane from any other entry stops as unimplemented.

To be told of services arriving (`io_service_add_notification_bin`) is to be handed those already there, and no message after: the services of a machine here are all it will have.

Not known to hold on an iPhone: the rule for zeros and the device's name (the first of its `compatible` strings) were measured on a Mac. The iOS runs were read, not compared with a device.

## Survey of a release's own binaries

Every Mach-O executable in `/bin`, `/sbin`, `/usr/bin`, `/usr/sbin` and `/usr/libexec` of macOS 27.2.1 (26B131), 1,293 of them, was run once with no arguments under `--missing` and a 60-second limit, before and after the registry, the sandbox profiles and the calls below were added. With no arguments most only start up, or print their usage (344 did), so this counts what a program needs to start, not to do its work. It was not run on iOS or on another release.

| | Before | After |
| --- | --- | --- |
| Made no call that is not implemented | 1,098 | 1,134 |
| Made at least one | 195 | 157 |
| Sent the registry or IOKit a request nothing answered | 48 | 2 (`ioreg`, asking for classes, and one asking for a driver's connection) |
| Said its sandbox profile was not found | 56 | 1 |
| Ran until it was a service waiting for a client, which is how a daemon run alone ends | 45 | 63 |
| Ended with a crash of its own (an abort or a trap) | 71 | 68 |

What was asked for most that was not there, by programs asking: `getrusage` (21; [there now](#what-a-process-has-used)), launchd's routine in message 1073742225 (16), the kqueue filter for user events (13), `pipe` (9), the properties of the registry's root (9, the kext tools), `kqueue_workloop_ctl`, `memorystatus_control` and `mach_port_get_attributes` (5 each; the last is [there now](#a-ports-status)).

One run of it filled the disk: `/usr/bin/yes` writes without end, and a guest's output was kept until it ended, with no limit on how much (27 GB in its 60 seconds). Since then the output of a run against one release is passed on as it is written, and where it has to be kept (several releases, whose outputs are compared, and the guests started beside the first) a guest that writes more than 64 MB stops the run and says so.

What it did not confirm from the ranked list: pipes (9 programs), and `fork`, `execve` and `posix_spawn` (6 between them), which a program asked for nothing does not reach.

Added after it, each answered as macOS 27.2.1 answers a process that is not root: `getlogin` (the user's name, with the zeros after it for as much of the kernel's 255 bytes as there is room for; it stops as unimplemented for a daemon and on iOS, where the name of launchd's own session is not known), `getdtablesize` (the soft limit on descriptors; unimplemented above 10240, where `kern.maxfilesperproc`, which is not known, could be the lower), and `setuid`, `seteuid`, `setgid` and `setegid` (0 to become what the process already is, EPERM for anything else; root becoming another user or group stops as unimplemented, as a process here has one of each).

Left as they were, still stopping or listed by name:

- **`confstr(_CS_DARWIN_USER_CACHE_DIR)` and the like**, which 15 programs end on. The directory is made by the release's `dirhelper`, and that was followed as far as it goes. Started by hand (`--daemon com.apple.bsd.dirhelper=system:/usr/libexec/dirhelper`) it checks in and takes the request. It needs `/var/folders`, which is on a device's data volume and so in no restore image (`--mount /var/folders=<dir>` gives one). It then makes `/var/folders/zz` and marks it with a call of the sandbox's (number 261 on macOS 27.2.1, given the directory and the word `folders`), which on a Mac leaves the directory with the extended attribute `com.apple.rootless` set to `folders`. That call is one of System Integrity Protection's: its number is a release's own, and what it answers can only be measured by a process that is root and entitled by Apple, so it stays unimplemented and dirhelper gives up there. Starting dirhelper when it is looked up would not change that.

## What a process has used

`getrusage` answers with the machine's own account of the process, as its clock is its own: the time its threads had the emulated CPU, as the user and in the kernel (a system call, a trap or a fault), by the machine's clock, which stands still while the host fetches; the most pages it has had, as its memory; the faults that brought pages in, a major one being one that read a file of the build; and how often a thread of its gave up the CPU to wait. Twenty-one of the release's programs asked, the shells among them. What a Mac's kernel was measured to keep at zero whatever a process does is zero (the sizes of text, data and stack, swaps, block input and output, messages), and so are signals, as none is delivered, and the times a thread was made to give up the CPU, as none is. Of its children, which a process here has none of, it is all zeros.

The numbers are not a Mac's, and cannot be: the emulated CPU is many times slower, and brings pages in sixteen at a time. [examples/usage.c](examples/usage.c) prints what holds of them on any machine (work adds to the time, waiting does not, nothing goes down) and prints the same natively as under ipswrun. `thread_selfusage`, `proc_pid_rusage` and the times in `task_info` and `thread_info` are still not answered.

## Pipes

`pipe` makes one, and its two ends are read, written, duplicated and closed as on a Mac: [examples/pipes.c](examples/pipes.c) prints the same natively on macOS 27.2.1 as under ipswrun ([native.txt](examples/expected/pipes/native.txt)). A pipe holds 16384 bytes, and 65536 once more than that is asked of it; a write of 512 bytes or fewer goes in whole or not at all; a reader with nothing to read waits for a writer, and a writer with no room for a reader, each until the other has done its part or its end is closed. `select` says which ends are ready, and waits for one to be. `fstat`, `fcntl` (the flags, with `O_NONBLOCK`, and `F_SETNOSIGPIPE`) and `ioctl` (`FIONREAD`, `FIONBIO`) answer as measured. With the release's own `/bin/zsh`, `-c 'echo hi'` now runs, and a command that is not built into the shell stops at `fork`.

libdispatch watches a descriptor through its work queue's kqueue, with the filters `EVFILT_READ` and `EVFILT_WRITE`, and for a pipe these now happen: an event when there is something to read, with how much, or room to write, with how much, and one with `EV_EOF` when the other end has gone. [examples/pipeevents.c](examples/pipeevents.c) prints the same natively as under ipswrun ([native.txt](examples/expected/pipeevents/native.txt)): a source for reading, one for writing, `dispatch_read` and `dispatch_write`, and a `dispatch_io` channel. `NSPipe` with a `readabilityHandler` and `readDataToEndOfFile` was run once and did the same as on the Mac.

What is not there, or not as a Mac has it:

- **A write with nobody to read it** is EPIPE where the process ignores SIGPIPE or the descriptor asked for none. Otherwise the signal ends a process, and here the run, saying why; a process with a handler for it stops as unimplemented, as no handler is ever run.
- **The number `fstat` gives each end** is made up by a Mac's kernel, differently every time, and is another made-up number here.
- **A pipe is one process's.** No descriptor passes to another process, as none is started (`fork`, `posix_spawn`) and none is sent in a message.
- **A kqueue of the guest's own, `poll`, `O_ASYNC`, and the size of a pipe on a system short of memory for pipes**, where a Mac's kernel gives them less. What a kqueue is told of the end of a pipe that is not the one read, or written, was not measured, and stops as unimplemented.

## A port's status

`mach_port_get_attributes` answers a receive right's status and its queue's limit as macOS 27.2.1 does: [examples/portstatus.c](examples/portstatus.c) prints the same natively on a Mac as under ipswrun ([native.txt](examples/expected/portstatus/native.txt)), through messages sent and received, rights to send once made, used, given up and kept by the kernel for a notification, limits, sets and guards. For it the kernel here now keeps, of every port, the rights to send once that exist for it, how many messages were received from it (which a message's trailer says too: it said 0), the limit of its queue, and its guard. A reply now arrives through the right it was given to answer with, where it used to arrive as if through a right to send once whatever it was given.

What is not known for sure stops as unimplemented: the other flavors (the owner, importance, the guard's own info, throttling), and the status of a name that is no port's here, which may be a name that is gone (an invalid name to a Mac) or a right the kernel keeps of another kind, such as a thread's (an invalid right). Not checked against a Mac: unguarding a port with a value that is not its guard, for which a Mac's kernel ends the process, also stops as unimplemented. The limit of a queue is kept and read back, and holds no sender to it. `mach_port_type` still answers that every name is a send right.

## Sysctls left out

A sysctl whose value is not known for sure for the release and device is not answered (ENOENT), rather than answered with another chip's or release's value:

- `hw.cpufamily` of a device not in `cpuFamilies`: of the Macs, only those with an M1, M1 Pro, Max or Ultra are there. The commpage's family is then 0 too.
- `hw.l1icachesize`, `hw.l1dcachesize` and `hw.l2cachesize`, except on an M1 Mac: an iPhone's or iPad's chip has its own.
- `hw.cpusubfamily` and the `hw.perflevel0` fields other than its core counts (`name`, `l2cachesize`, `cpusperl2`, `physicalcpu_max`), which libBLAS asks for and does without.
- `kern.version`, which names the build's kernel and when it was built; `kern.iossupportversion`, which is a Mac release's own (in its `SystemVersion.plist`); `kern.osproductversioncompat`, which a Mac of macOS 27 leaves empty; `kern.maxfilesperproc`, which follows the device's memory.
- `hw.cpubrand`, which no kernel has (a Mac's brand is `machdep.cpu.brand_string`).

What describes the emulated machine itself stays: one CPU (`hw.ncpu` and the `perflevel0` counts), 8 GB, half of it free (`kern.memorystatus_level`).

## AMX emulation

Apple's matrix coprocessor is reached by instructions in an encoding Arm leaves undefined (`0x00201000`, an operation in bits 5–9 and a register in bits 0–4): set and clear, loads and stores of its X, Y and Z registers, extracts, and the multiply-accumulate, vector and matrix operations in 16-, 32- and 64-bit floating point and integers. Reverse-engineered descriptions of all of them are public. Emulating them in Go, from the hook that now stops the run, would let Accelerate run: a register file per thread, kept across context switches, and each instruction checked against the host's own results on an Apple silicon Mac. Every AMX instruction would leave Unicorn for Go, so it would be correct but slow.

## Out of reach under the current rules

Each of these needs a kernel driver, so with no kext emulation they stay out. Offscreen work that is pure CPU should still run.

- **GUI.** AppKit's `NSApplication` needs WindowServer, and WindowServer needs the framebuffer, IOSurface and GPU drivers. UIKit needs SpringBoard and backboardd on top of that. CoreGraphics bitmap contexts and ImageIO should work. CoreText also needs `/System/Library/Fonts` opened with `--system`, and probably fontd.
- **Metal and the GPU.** With no GPU driver, `MTLCreateSystemDefaultDevice` returns nil. CoreImage and CoreML fall back to CPU paths where they have them, and anything built on IOSurface fails.
- **Keychain and data protection.** The data-protection keychain sits on AppleKeyStore. Only non-permanent, iOS-style keys work today, as the readme says.
- **Sandbox, AMFI and TCC enforcement.** Nothing is checked, so sandbox profiles and privacy prompts can't be observed. tccd could run as a daemon, but most of what it guards leads to more drivers.
- **macOS before 11.** Those releases have no arm64 code.

## Suspicions not yet tested

These come from reading the code and are the ones most worth a run on a Mac.

- **JavaScriptCore's JIT.** `MAP_JIT` and `pthread_jit_write_protect_np` use system registers that only Apple's cores have, which is probably an undefined instruction here. `JSC_useJIT=0` should still run JavaScriptCore as an interpreter.
- **SQLite WAL and Core Data.** Core Data opens WAL stores and may hold two connections to one. With `-shm` mapped privately, each connection sees its own copy.
- **os_log output.** Without logd, logs go nowhere. `--env OS_ACTIVITY_DT_MODE=1` may copy them to standard error, which would help when debugging daemons.
- **Speed.** Only one guest thread runs at a time, on one host core. A CPU-bound benchmark, such as 10 MB through CommonCrypto's SHA-256, would put a number on the slowdown against native.
