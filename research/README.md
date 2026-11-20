# Research: adid's wire protocol and provisioning

Working notes and throwaway tooling from finding out how `adid` (the daemon behind an Apple
ID's one-time passwords) is asked things. None of this is product code. It lives on this branch
so that it is not lost, and is not meant to be merged.

Everything below was observed on **macOS 13.0 (22A380)** unless it says otherwise, by running the
release's own `adid` under ipswrun as a second guest. On 13.0 it has been provisioned against
Apple's service and gives one-time passwords: see [Provisioning 13.0's adid](#provisioning-130s-adid).

## What adid is, by release

| Releases | Service | Driver calls (IOKit imports) | Asks about its client |
| --- | --- | --- | --- |
| 11.0.1 – 13.0 | plain Mach service, message 1200 answered by 1300 | none | nothing |
| 13.6 – 27.2.1 | XPC listener, dictionary with a `data` key | 7 (`IOServiceOpen`, `IOConnectCallStructMethod`, …) | code-signing status, DER entitlements, identity, path |

- From 27.2.1's strings and behaviour: it serves a client whose entitlement `adi-client` is a
  string it knows. `3184554750` (commerce's) and `347420559` (akd's) are accepted; other values,
  a boolean, or no entitlement get status -45070.
- State is kept in `/var/db/fpsd/adi/adi.pb` (13.0, running as root: it makes the directory and
  writes `adi.pb`, 646 bytes once provisioned, beside `adi-gb.lck` and `adi.pb.lck`). At start it
  also looks for `/Users/Shared/adi/adi.pb`, and while serving a client whose user is 501 for
  `/Users/Shared/adi/adi-000001F5.pb`, but it wrote nothing there. Under ipswrun the directory has
  to be mounted (`--mount /var/db/fpsd=<dir>`): without it `mkdir` fails and nothing is kept.
- 13.6's adid traps inside libxpc under ipswrun; not diagnosed.
- There is no CoreADI library to load on macOS: the framework holds only the daemon. The client
  code is built into AOSKit and akd, with nothing exported.

## The request

The same bytes travel either way: out of line in Mach message 1200 (11.0.1 – 13.0), or as the
`data` of an XPC dictionary (13.6 on). All fields are big-endian.

    [u32 1] [u32 code] [u32 1] [payload]

The answer starts with a big-endian `i32` status. A failure is those 4 bytes and nothing else. A
success carries what the call returns after it, each blob as a `u32` length and its bytes.

| Status | Seen when |
| --- | --- |
| 0 | success |
| -45001 (`ffff5037`) | an argument is not what the call wants: blobs in the wrong order, an account id where a session belongs, an account id other than -2 |
| -45019 (`ffff5025`) | unknown code |
| -45020 (`ffff5024`) | third word is not 1 |
| -45025 (`ffff501f`) | no such session |
| -45031 (`ffff5019`), -45032 (`ffff5018`) | start provisioning with a blob that is not a server challenge (200 and 16 zero bytes) |
| -45034 (`ffff5016`) | payload shorter than the call needs, or a length field larger than the data |
| -45054 (`ffff5002`) | `85fe63b0`, `b0eda7af` with no state |
| -45061 (`ffff4ffb`) | not provisioned: no state, state of another machine, or another account id |
| -45062 (`ffff4ffa`) | erase with nothing to erase |
| -45070 (`ffff4ff2`) | client not entitled (27.2.1) |

## The codes (13.0)

adid adds `0x3949f873` to the code and walks a comparison tree. Sending a made-up code and
logging what it was compared against, then probing each constant found, gave twelve codes.

The first four rows are confirmed: with them 13.0's adid was provisioned against Apple's
provisioning service and gave a one-time password (see below). The one-time password request
is also what Apple's own client sends (AOSKit's `+[AOSUtilities retrieveOTPHeadersForDSID:]`, on
11.0.1 and 27.2.1 alike). The reading of the rest is inference from argument shapes, answers and
what happened to the state. The account id is a `u64`; -2 is the machine's own, the one Apple's
clients use, and the only one that worked.

| Code | Payload | Answer after the status | Reading |
| --- | --- | --- | --- |
| `716bd86c` | account id, server challenge (`spim`) as length and bytes | client challenge (`cpim`, 276 bytes) as length and bytes, then a `u32` session | start provisioning |
| `0b2b3196` | `u32` session, `ptm` as length and bytes, `tk` as length and bytes | nothing; `adi.pb` is written | end provisioning |
| `cfe0b46a` | account id | machine id (60 bytes) as length and bytes, password (28 bytes) as length and bytes | one-time password |
| `55a637c0` | `u32` session | nothing; the same session again is -45025 | destroy session |
| `7715488c` | account id | nothing; `adi.pb` shrinks to about 80 bytes and every call is -45061 again | erase provisioning |
| `385b2ce8` | account id | a `u32` that changes with the clock | login code? |
| `85fe63b0` | account id | a `u64`, 0 after provisioning | routing info? |
| `b0eda7af` | account id | nothing, state unchanged | unknown |
| `a54951ec` | account id, `u32` length, blob | not seen (-45061 with no state; not tried with state) | synchronize? |
| `632b8d6e` | account id, `u32` session, two blobs as length and bytes | nothing, and nothing is stored: 0 with a live session and the server's two blobs, -45025 with a session that does not exist | unknown. It was taken for end provisioning from its shape, which is why the first live exchange failed with -45001 |
| `72096fe6` | nothing | nothing | succeeds unconditionally |
| `9679d4b5` | account id | adid traps (`brk`) | unknown |

The machine id is the same every time. The password depends on the clock: three clocks gave
three passwords, and two requests seconds apart the same one. Whether Apple accepts the
password is not verified: only a sign-in would show it, and none was made.

Not known: whether the codes are the same on other releases, and what `632b8d6e`, `b0eda7af`
and `9679d4b5` are.

## Provisioning 13.0's adid

Done once, live, then replayed from the kept responses. No account is involved at any point.

1. The host asks `https://gsa.apple.com/grandslam/GsService2/lookup` for the service's URLs and
   posts an empty request to `midStartProvisioning`. The answer has `spim`, the server challenge
   (347 bytes).
2. A guest sends it to adid as `716bd86c` and gets `cpim` and a session.
3. The host posts `cpim` to `midFinishProvisioning`. The answer has `ptm` (503 bytes), `tk` (16
   bytes) and `X-Apple-I-MD-RINFO`, the routing info a client later sends as a header.
4. The guest sends `0b2b3196` with the session, `ptm` and `tk`. adid writes `adi.pb`.
5. From then on, in that run or a later one with the same state directory, `cfe0b46a` answers
   with the machine id and a password.

Steps 2 and 4 have to be one run: the session is in adid's memory and a run ends its guests. So
the guest waits between them for files the host puts in its home.

The requests are plists, `{Header: {}, Request: {}}` and `{Header: {}, Request: {cpim: <base64>}}`,
sent as `text/x-xml-plist` with `User-Agent: akd/1.0 CFNetwork/1399.4 Darwin/22.1.0` and
`X-Mme-Device-Id` (the machine's UUID), `X-Apple-I-MD-LU` (SHA-256 of its 16 bytes, in hex),
`X-MMe-Client-Info` (`<Macmini9,1> <Mac OS X;13.0;22A380> <com.apple.AuthKit/1 (com.apple.akd/1.0)>`),
`X-Apple-I-SRL-NO: 0`, `X-Apple-I-Client-Time`, `X-Apple-I-TimeZone` and `X-Apple-Locale`. The
header names and client info format are swift-anisette's (`Headers.swift`, `SystemInfo.swift`);
the versions are 13.0's own, read from its `Info.plist` files. swift-anisette makes no request
itself (it leaves the exchange to its caller), so the endpoints and bodies are the ones other
open implementations of this exchange use. The service's certificate is under Apple Root CA,
which public trust stores do not have: the tool trusts that one certificate by its hash.

The service was asked four times in all: the lookup, one start, and two finishes (the first
finish was spent on the run that sent the wrong end-provisioning code). It answered a second
finish for the same server challenge without complaint, a few minutes later. It never refused
or asked for more.

### What adid takes from the machine

- `gethostuuid`: once at start and once while starting provisioning. The state belongs to that
  identifier: with another one the same `adi.pb` answers -45061. ipswrun gives every guest the
  same fixed UUID (`emulator/bsd.go`), so every ipswrun is the same machine to adid, and the
  tool sent that UUID as `X-Mme-Device-Id`. Whether the service or adid compares the two is not
  known: they were never different.
- The clock, for the client challenge, the session number and the password.
- `getentropy`, which ipswrun answers reproducibly.
- Nothing else that showed: the trace has `sysctl` only for `kern.osversion`, `kern.bootargs`,
  `kern.osvariant_status`, `kern.secure_kernel` and `hw.pagesize`, all from the system
  libraries' start. No model, no serial number, no IOKit.

### What a product version would look like

The request codes are data a guest sends, and adid is the release's own binary loaded by the
release's own dyld, so nothing here calls into Apple's code by address or depends on a build's
layout. The codes themselves were found on one build and have to be found again (or shown
equal) for any other.

- **The HTTP belongs on the host.** Guests have no network and should not get one for this. The
  shape that worked: a host-side step fetches `spim`, a guest beside adid turns it into `cpim`
  and waits, the host exchanges it for `ptm` and `tk`, the guest finishes. The blobs cross
  through the home directory. This is an example (a guest and a host program in `examples`),
  not a feature of `ipswrun` itself: ipswrun would only have to keep doing what it does.
- **A second mount is the only setup**: `--mount /var/db/fpsd=<dir>` keeps the state, and an
  `examples/adi.c` that provisions needs it. Nothing in the emulator had to change for 13.0.
- **`chdir` is answered 0 and does nothing** (it is in `trivial` in `emulator/bsd.go`). A guest
  that changes directory and opens a relative path gets "no such file". That is the kind of
  quiet wrong answer master's rules are against: it should work or stop the run.
- **The machine's identifier is a constant.** Every user of ipswrun would provision the same
  machine UUID. For a product the identifier should be the user's to choose (a flag, kept with
  the state), since the state is useless under another one.
- **Waiting on the host is a poll.** The guest sleeps and stats a file, because file events are
  not delivered. It works, and a run's `--timeout` bounds it.
- 13.6 and later need more: adid is an XPC listener there, checks its client's entitlement, and
  calls a driver. None of that was attempted here.

## Tooling

- `probe/probe-hook.patch` and `probe/probe_tmp.go.txt`: an instruction hook for the emulator.
  Apply the patch, copy the `.txt` to `emulator/probe_tmp.go`, build. With `PROBECMP=<process
  label>` and `PROBEMAGIC=<value>` set, once that process receives message 1200 it follows the
  value through arithmetic and prints what it is compared against. Never commit it to master.
- `probe/walk_codes.py`, `probe_layout.py`, `probe_len.py`: the scripts that walked the
  comparison tree and probed payload shapes. They expect the patched binary as `ipswrun-probe`,
  `guests/adiq.c` built as `adiq`, and a cache directory, beside the `tools` directory they are
  run from; adjust the paths at the top.
- `guests/adiq.c`: sends a request given as hex to adid by message 1200 and prints the answer.
- `guests/adiseq.c`: several requests in one run, so that a session from start provisioning can
  be used by the next (`S`), with blobs read from files in the home (`F:/Users/emu/ptm.bin`).
- `guests/provision.c`: the guest's half of provisioning. With `spim.bin` in the home it starts,
  writes `cpim.bin`, waits for `ptm.bin`, `tk.bin` and `done`, ends, and asks for a one-time
  password (`mid.bin`, `otp.bin`). `provision otp` only asks.
- `provision`: the host's half (`go build ./research/provision`): `start` and `finish <cpim
  file>`. Every response is kept under `research/live` (ignored by git: never commit it, nor a
  provisioned `adi.pb`) and a request already answered is not made again.
  `provision/exchange.sh` runs the whole exchange.
- `probe/fixed-clock.patch` and `probe/fixedclock_tmp.go.txt`: applied like the instruction hook.
  With `FIXEDCLOCK=<unix seconds>` each guest's clocks are a counter of its own, so adid gives
  the same client challenge and session for the same server challenge on every run, and a
  finished exchange replays from the kept responses with no request. This is what let the end
  of provisioning be worked out offline. `HOSTUUID=<32 hex digits>` changes the machine's UUID.
- `guests/otp.c`: asks AOSKit for the anisette headers (the real client's path to adid).
- `guests/mirror.c`: checks that memory mapped twice is one memory.
- `extract`: copies files out of a restore image's system volume (`go run ./research/extract <ipsw url> <cache dir> <out dir> <path>...`).
- `sign`: ad hoc signs a Mach-O with one string entitlement (`go run ./research/sign <in> <out> <identity> <key> <value>`).
- `cross-build.sh`: builds a C guest for macOS arm64 on Linux.

Run with a daemon beside the guest (add `--mount /var/db/fpsd=<dir>` to keep its state):

    ipswrun --stream-compressed --daemon com.apple.adid=system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid 13.0 ./adiq 00000001cfe0b46a00000001fffffffffffffffe
