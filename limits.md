# Limits

What ipswrun cannot run or do yet, beyond the short list in the [readme](readme.md#what-does-not-work), ranked by how much real-world code each limit blocks. "Fixable" means a fix keeps to the project's rules: no guessed success for unimplemented calls, nothing keyed to one build, no kext emulation, and no replacing Apple's libraries.

The biggest gap is that a guest cannot start another process, and nothing starts Apple's daemons on demand, so most frameworks quietly lose the services they lean on.

This list was made by reading the code, not by running guests against releases. The [last section](#suspicions-not-yet-tested) lists what most needs a run to confirm.

## Ranked limits

| # | Limit | What it blocks | Where in the code | Fixable? |
| --- | --- | --- | --- | --- |
| 1 | No process creation: `fork`, `posix_spawn` and `execve` have no handler and stop the run | `NSTask`, `system()`, `popen`, every command-line tool or daemon that starts a helper, XPC services embedded in frameworks | `emulator/bsd.go`, handler table | Yes. `posix_spawn` maps onto the existing multi-guest `Machine`; `fork` is harder (it copies an engine's pages) |
| 2 | No launch on demand: a Mach service exists only if it is named with `--service`, `--agent` or `--daemon` | Frameworks silently lose notifyd, distnoted, logd, trustd, securityd, tccd, fontd, opendirectoryd and cfprefsd unless each is wired by hand; daemons that need other daemons | `emulator/launchd.go` | Yes. Read the release's own LaunchDaemons and LaunchAgents plists and start the daemon on the first lookup of its service |
| 3 | No signal delivery: `sigaction`, `sigprocmask` and `sigaltstack` are kept and read back, but a handler never runs; `kill` and `pthread_kill` end the run, as does any CPU fault (a guard page included); `setitimer` that starts a timer stops as unimplemented | Crash handlers, `SIGALRM` timeouts, guard-page tricks, Mach exception ports; `abort()` makes ipswrun exit 70 instead of the guest exiting 134 | `emulator/bsd.go` (`sysSigaction`, `sysSetitimer`, `sysKill`), `emulator/process.go` (`interrupt`) | Yes. Build a signal frame and return through `__sigtramp` and `sigreturn` |
| 4 | No descriptor I/O beyond files: no `pipe`, `socketpair` or sockets (`socket` answers EACCES); `kevent` and `kevent64` on a guest's own kqueue take changes and answer `EV_RECEIPT`, but a wait for events stops as unimplemented; `select` finds nothing ready; `EVFILT_READ`, `EVFILT_WRITE`, `EVFILT_VNODE` and `EVFILT_PROC` never fire | `NSPipe`, `dispatch_io`, `NSFileHandle` readability, `CFSocket`, Unix-socket clients (syslog, mDNSResponder), any network code | `emulator/files.go` (descriptor kinds), `emulator/kevent.go` (`change`) | Pipes and socketpairs, yes. A real network is a policy call, since it breaks reproducibility |
| 5 | Accelerate's BLAS and LAPACK: every release's libBLAS runs `cblas_sgemm` and the like on Apple's matrix coprocessor (AMX), whatever chip the guest is told it has, and Unicorn has no AMX; the run stops at the first AMX instruction and names it | Linear algebra through Accelerate, and what is built on it (CoreML's and BNNS's CPU paths, likely vDSP's matrix calls) | `emulator/process.go` (`amx`, `interrupt`) | Yes, [below](#amx-emulation): emulate the coprocessor in the invalid-instruction hook |
| 6 | No hardware at all: IOKit finds nothing, and no IORegistry properties are served | MobileGestalt, serial number and platform UUID, power, HID, and on iOS most facts about the device | `emulator/iokit.go` | Partly: a read-only IODeviceTree from the restore image's own DeviceTree file, without drivers |
| 7 | Shared file memory: `MAP_SHARED` of a real file is a private copy, and there is no `msync`. Protections are kept per region (`mmap`, `mprotect`, `mach_vm_protect`, the `mach_vm_map` trap, the shared cache's own) and a forbidden access faults, but memory from the MIG `vm_map` and `mach_vm_remap` is mapped with every protection | SQLite WAL `-shm` files shared by two connections or processes; anything that writes a mapped file | `emulator/bsd.go` (`sysMmap`), `emulator/mach.go` | Shared file mappings, yes, through the `shm_open` path |
| 8 | Threads take turns on one CPU, with no preemption, and `hw.ncpu` is 1 | Spin-waits without a system call hang; nothing runs in parallel, so compute-heavy guests are slow | `emulator/threads.go` | Hard: Unicorn does not resume reliably after a stop from outside a hook |
| 9 | The system volume is closed by default: only feature flags, `SystemVersion.plist`, ICU data and the Objective-C trampolines can be read | Frameworks' `Resources` (Info.plist, `.strings`, `.car`), fonts and other `/System/Library` data: frameworks behave differently with no error | `emulator/build.go` (`DefaultSystem`) | Yes, it is a default; `--system` already opens more |
| 10 | Device and OS coverage: iPhones and iPads from the A12 only; no tvOS, watchOS, visionOS or bridgeOS; no macOS before 11 | iOS 14 to 16 on A9 to A11 devices; the other Apple platforms | `cmd/ipswrun` device tables | A9 to A11 (plain arm64 caches) and tvOS look reachable; watchOS is arm64_32; macOS before 11 has no arm64 code |
| 11 | Hosts: an Apple silicon Mac, or Linux on arm64 or x86-64 with glibc; guests have to be built on a Mac | Intel Macs, Windows, musl Linux, CI without a Mac | `emulator/artifact.go` (pinned libunicorn builds) | Intel Macs and musl need a libunicorn build to pin; building guests on Linux needs an SDK |

## What to fix first

These four would unlock the most code for the least work, and each keeps to the rules above.

1. **Launch on demand from the release's own plists.** On a lookup of an unknown name, find the plist under `/System/Library/LaunchDaemons` or `LaunchAgents` whose `MachServices` holds it, and start its `Program` as a daemon or agent. This makes the hand-wired `prefs` and `adi` setups the default, and shows for each release which daemon a framework actually needed. Each daemon that starts will surface missing calls of its own, so expect follow-up work.
2. **`posix_spawn` of system binaries.** A spawned child becomes another `Process` on the same `Machine`, with its own pid, its standard descriptors wired to the parent's, and `wait4` for its exit. It needs pipes (the next item) to be useful for `NSTask`. `fork` can wait.
3. **Pipes and socketpairs, with `EVFILT_READ` and `EVFILT_WRITE`.** An in-emulator byte buffer per pair, read and written through the existing descriptor table, with kevent readiness and blocking reads that wait like the other kinds of wait. This also lets `select` and plain `kevent` answer truthfully instead of returning nothing.
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
| `sigaction`, `sigprocmask`, `sigaltstack`, `setitimer` | See limit 3 |
| `kevent`, `kevent64` | See limit 4 |

Left as they are, deliberately: `setrlimit`, `fsync` and `sync` answer 0, which is harmless, and so does `__mac_syscall` for Sandbox, so `sandbox_init` succeeds and confines nothing. That is fine for running guests, but sandbox behaviour can't be studied.

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
- **IORegistry calls.** `emulator/iokit.go` answers only matching, iterators, opening, method calls and closing. A guest that reads the root entry or a property directly probably stops as unimplemented instead of getting an empty answer.
- **SQLite WAL and Core Data.** Core Data opens WAL stores and may hold two connections to one. With `-shm` mapped privately, each connection sees its own copy.
- **os_log output.** Without logd, logs go nowhere. `--env OS_ACTIVITY_DT_MODE=1` may copy them to standard error, which would help when debugging daemons.
- **Speed.** Only one guest thread runs at a time, on one host core. A CPU-bound benchmark, such as 10 MB through CommonCrypto's SHA-256, would put a number on the slowdown against native.
