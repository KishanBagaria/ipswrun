package emulator

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

// Machine is the one computer the guests of a run are processes of. They share its file system
// (the home, the mounts, what the build's system volume gives), its launchd and its clock, and
// take turns on it: each process has an engine of its own, and one thread of one process runs
// at a time (see threads.go). The first guest started is the one the run is for. The others
// are there to serve it, as the daemons of a real system are, and the run ends when the first
// exits.
type Machine struct {
	cfg      Config
	user     user      // whom the guests run as, and who owns the home: the platform's (see userOf)
	dyld     []byte    // the build's dyld, as the emulated CPU runs it
	mounts   []mounted // the home among them; longest guest path first
	procs    []*Process
	running  *Process                       // the process whose thread has, or last had, the turn
	services map[string]*service            // what launchd knows (see launchd.go)
	shared   []*sharedMemory                // every memory of the host's that mappings share (see shared.go)
	named    map[string]*sharedMemory       // those shm_open knows by a name
	xattrs   map[hostFile]map[string][]byte // the extended attributes of the home's and mounts' files (see metadata.go)
	ioMain   *port                          // what IOKit's searches are sent to, once a process has asked for it
	stepping bool                           // a thread of the running process is on the CPU now, and not between two turns
	asked    map[string]bool                // the services that were looked for among the build's own, once each
	replaced []*Process                     // processes that are no longer among procs, to be closed with them
	deferred []func()                       // what a system call left to be done between stretches of emulation (see spawn.go)
	// The sandbox profiles the kernel has by name, once its kernelcache has been read for them
	// (see kernelprofiles.go), or why they could not be.
	profiles     []string
	profilesErr  error
	fifos        map[hostFile]bool        // the named pipes guests have made under the home and mounts (see sysMkfifo)
	owners       map[hostFile]Owner       // the owners of the home's and mounts' files that are not the guests' user's (see metadata.go)
	said         map[hostFile]uint16      // the permissions the data volume's template has for a file, where the host's file has others (see data.go)
	flags        map[hostFile]uint32      // the flags a file carries beside its mode, where it has any (see chflags)
	resolver     uint32                   // the process that made itself the one the kernel asks who a user or a group is (identitysvc), if one has
	caches       [2]uint32                // how many users and how many groups' members the kernel is to keep answers for: 500 and 250 at first, as a Mac's kernel says
	polls        []carried                // the rights launchd keeps to answer those who asked to be told of a change (see launchd.go)
	pipes        uint64                   // how many pipes were ever made: the next one's number
	host, clocks *port                    // the host's port and its clock's, while any process has a right to them (see kernelports.go)
	seen         map[fileID]string        // where each file a guest has looked at is, by its number (see saw)
	ranges       map[hostFile][]rangeLock // the locks open files hold on parts of files (see rangelocks.go)
	uniques      uint64                   // how many processes were given a number of their own, after launchd's
	versions     uint32                   // how many versions were given, after launchd's (see lineage)
	launchdUUID  *[16]byte                // the UUID of the release's launchd, once a process has asked for it
	started      uint32                   // how many processes were ever made: a pid is not given twice, whatever has ended since
	ioRoot       *ioEntry                 // the root of the I/O Registry, once a process has asked for it (see registry.go)
	clock        *clock
	deadline     time.Time // of the run, as every time here by the machine's clock
	settling     time.Time // once the main guest has exited: when the daemons are no longer waited for
}

// Guest is one program to run as a process.
type Guest struct {
	Executable string   // host path of the Mach-O (arm64, linked against the cache); for RunBuild, with SystemPrefix its path in the build
	Image      []byte   // the Mach-O itself, if it is one of the build's and not a file of the host's: Executable is then its path in the guest
	Args       []string // after argv[0]
	Env        []string // its whole environment
	// Services are the Mach services it provides: the names it may check in with launchd, which
	// keeps what is sent to them until it does.
	Services []string
	Root     bool // it runs as root, not as the guests' user
	// Owner, if not nil, is the user and group it runs as instead: a daemon's own.
	Owner *Owner
}

// Owner is a user and its group, by their numbers.
type Owner struct{ UID, GID uint32 }

// NewMachine makes a machine with no process yet.
func NewMachine(cfg Config) (*Machine, error) {
	if err := loadUnicorn(cfg.Unicorn); err != nil {
		return nil, err
	}
	dyld, err := thin(cfg.Dyld, cpuSubtypeARM64E)
	if err != nil {
		return nil, fmt.Errorf("dyld: %w", err)
	}
	u := userOf(cfg.Cache.Platform)
	if err := CheckMounts(cfg.Mounts, u.home); err != nil {
		return nil, err
	}
	mounts, err := openMounts(append([]Mount{{Guest: u.home, Host: cfg.Home, Data: true}}, cfg.Mounts...))
	if err != nil {
		return nil, err
	}
	m := &Machine{cfg: cfg, user: u, dyld: dyld, mounts: mounts, services: map[string]*service{}, asked: map[string]bool{}, named: map[string]*sharedMemory{}, xattrs: map[hostFile]map[string][]byte{}, owners: map[hostFile]Owner{}, said: map[hostFile]uint16{}, flags: map[hostFile]uint32{}, caches: [2]uint32{500, 250}, fifos: map[hostFile]bool{}, clock: newClock()}
	for i := range m.mounts {
		m.claim(&m.mounts[i])
	}
	return m, nil
}

// Spawn makes a process of a guest, ready to run from dyld's entry point. Two guests may not
// provide the same service.
func (m *Machine) Spawn(g Guest) (p *Process, err error) {
	for _, name := range g.Services {
		if s := m.services[name]; s != nil && s.provider != nil {
			return nil, fmt.Errorf("the service %s is provided by both %s and %s", name, s.provider.exePath, g.Executable)
		}
	}
	return m.start(g)
}

// start makes the process of a guest, and makes it the provider of its services.
func (m *Machine) start(g Guest) (p *Process, err error) {
	image := g.Image
	if image == nil {
		if image, err = os.ReadFile(g.Executable); err != nil {
			return nil, err
		}
	}
	path := GuestExe
	if g.Image != nil {
		path = canonical(g.Executable)
	}
	if p, err = m.newProcess(g.Executable, image, path); err != nil {
		return nil, err
	}
	p.lineage = m.descend(launchdLineage, p.uuid)
	if g.Root {
		p.uid, p.gid = 0, 0
	}
	if g.Owner != nil {
		p.uid, p.gid = g.Owner.UID, g.Owner.GID
	}
	for _, name := range g.Services {
		if s := m.services[name]; s != nil { // looked up already, and waited for (see onDemand)
			s.provider = p
		} else {
			m.services[name] = &service{provider: p}
		}
	}
	defer guard(&err)
	return p, p.load(append([]string{p.path}, g.Args...), g.Env)
}

// Main is the guest the run is for: the first one started.
func (m *Machine) Main() *Process { return m.procs[0] }

// Processes are every guest that was started, the main one first.
func (m *Machine) Processes() []*Process { return m.procs }

// Close releases every process, the memory they shared, and the mount roots.
func (m *Machine) Close() {
	for _, p := range append(m.procs, m.replaced...) {
		p.Close()
	}
	for _, s := range m.shared {
		s.free()
	}
	m.shared = nil
	closeMounts(m.mounts)
	m.mounts = nil
}

// Run runs the guests until the main one exits. Any other end (a fault, something unimplemented,
// the timeout, nothing left that could run) is a *StopError, and Diagnostics describes it. The
// timeout is time the guests had: what the host spends fetching for them is not counted (see
// clock).
func (m *Machine) Run(timeout time.Duration) (err error) {
	defer guard(&err)
	m.deadline = m.clock.now().Add(timeout)
	m.running = m.procs[0]
	for {
		p := m.running
		if !p.step() {
			return nil
		}
		if p.exited {
			how := fmt.Sprintf("with status %d", p.exitCode)
			if p.endSignal != 0 {
				how = fmt.Sprintf("by signal %d", p.endSignal)
			}
			m.exit(p, how)
		}
		if (p.exited || p.reschedule()) && !m.schedule() {
			return nil
		}
	}
}

// exit is what follows a process's end, however it came to it: its rights end with it, and if
// it is the guest the run is for, the others have only a while longer.
func (m *Machine) exit(p *Process, how string) {
	p.early = p != m.procs[0] && !m.procs[0].exited && p.parent == nil
	p.exited = true
	p.note("exited %s", how)
	p.reap()
	for _, n := range slices.Sorted(maps.Keys(p.fds)) { // and its files, with the locks on them
		p.closeFD(n)
	}
	if parent := p.parent; parent != nil && !parent.exited { // told before it is given the child to wait for
		told := pendingSignal{sig: sigChild, code: codeChildExited, status: int32(p.exitCode), pid: p.pid, uid: p.uid}
		if p.endSignal != 0 {
			told.code, told.status = codeChildKilled, int32(p.endSignal)
		}
		parent.sendSignal(told)
	}
	m.childExited(p)
	m.exitWatched(p)
	if p == m.procs[0] {
		// The others are asked to end, as launchd asks its daemons when they are no longer
		// wanted, and have a while to do what they do about that.
		m.settling = m.clock.now().Add(settleTime)
		for _, other := range m.live() {
			if other.signalled(sigTERM) {
				other.note("sent SIGTERM")
			}
		}
	}
}

const sigTERM = 15

// settleTime is how long the daemons may go on after the guest has exited. What a daemon was
// asked to do it may do later, as a daemon that keeps something writes it a moment after it is given,
// and on a real system it is still there to do it. Here the run goes on until no daemon has
// anything left to do or to wait for, or until this much time has passed.
const settleTime = 5 * time.Second

// process is the process with a pid, if it has not exited.
func (m *Machine) process(pid uint32) *Process {
	for _, p := range m.procs {
		if p.pid == pid && !p.exited {
			return p
		}
	}
	return nil
}

// live are the processes that have not exited.
func (m *Machine) live() []*Process {
	var out []*Process
	for _, p := range m.procs {
		if !p.exited {
			out = append(out, p)
		}
	}
	return out
}

// next is the thread whose turn it is once the running one gave its turn up: the next that can
// run in its process, else in the processes after it, and last itself again.
func (m *Machine) next() (*Process, *thread) {
	at := slices.Index(m.procs, m.running)
	for k := range m.procs {
		p := m.procs[(at+k)%len(m.procs)]
		if p.exited {
			continue
		}
		current := slices.Index(p.threads, p.current)
		for i := range p.threads {
			if t := p.threads[(current+1+i)%len(p.threads)]; t.state == runnable && (p != m.running || t != p.current) {
				return p, t
			}
		}
	}
	if p := m.running; !p.exited && p.current.state == runnable {
		return p, p.current
	}
	return nil, nil
}

// earlier is the one of two times that comes first, a zero time being none.
func earlier(a, b time.Time) time.Time {
	if b.IsZero() || !a.IsZero() && !b.Before(a) {
		return a
	}
	return b
}

// schedule is called between stretches of emulation once a thread gave up its turn: it brings in
// the next thread that can run, of any process, sleeping until the nearest deadline if none can.
// False is the end of the run: the guest has exited, and the others are at rest or out of time.
func (m *Machine) schedule() bool {
	m.running.yield = false
	main := m.procs[0]
	for {
		// What a system call left to be done. The thread that made it has not given up its
		// turn for that: it goes on, as a parent does before the child it started first runs.
		did := len(m.deferred) > 0
		for len(m.deferred) > 0 {
			do := m.deferred[0]
			m.deferred = m.deferred[1:]
			do()
		}
		if p := m.running; did && !p.exited && p.current.state == runnable {
			p.switchTo(p.current)
			return true
		}
		now := m.clock.now()
		var until time.Time
		live, stuck := false, false
		for _, p := range m.procs {
			if p.exited {
				continue
			}
			until = earlier(until, p.serve(now))
			ended := true
			for _, t := range p.threads {
				ended = ended && t.state == dead
				if t.state != waiting {
					continue
				}
				switch {
				case t.deadline.IsZero():
					// A wait for a message or a signal, or for the kernel to ask the resolver of
					// users something, may go on for good: that is a service with no client. Any other wait with no end is one that nothing can end.
					stuck = stuck || t.kind != waitPort && t.kind != waitSignal && t.kind != waitResolver
				case !t.deadline.After(now):
					late := t.late
					t.answer = &late
					t.wake()
				default:
					until = earlier(until, t.deadline)
				}
			}
			if ended {
				m.exit(p, "with its last thread") // as if main had returned 0
			}
			live = live || !p.exited
		}
		if len(m.deferred) > 0 {
			continue // what serving the processes left to be done, such as the end of one a timer's signal ended
		}
		if main.exited && (!live || !now.Before(m.settling)) {
			return false
		}
		if p, t := m.next(); t != nil {
			p.switchTo(t)
			m.running = p
			return true
		}
		switch {
		case main.exited && (until.IsZero() || until.After(m.settling)):
			return false
		case until.IsZero() && !stuck:
			stopKind(StopIdle, "nothing left to run: every thread waits for a Mach message or for work, and nothing here will send one. A service waiting for a client ends this way")
		case until.IsZero():
			stopf("deadlock: no thread can run, and nothing is due that would let one")
		case !until.Before(m.deadline):
			stopKind(StopTimeout, "timed out with every thread waiting")
		}
		time.Sleep(until.Sub(m.clock.now()))
	}
}

// Diagnostics describes a run that stopped: what the process that had the turn was doing, and
// where the threads of the others are.
func (m *Machine) Diagnostics() string {
	if m.running == nil {
		return ""
	}
	out := []string{m.running.Diagnostics()}
	for _, p := range m.live() {
		if p != m.running {
			out = append(out, fmt.Sprintf("  threads of %s (pid %d):", p.label, p.pid), p.threadList())
		}
	}
	if len(m.procs) > 1 {
		out = append([]string{fmt.Sprintf("  in %s (pid %d):", m.running.label, m.running.pid)}, out...)
	}
	return strings.Join(out, "\n")
}
