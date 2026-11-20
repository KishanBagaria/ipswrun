package emulator

import (
	"fmt"
	"os"
	"path/filepath"
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
	dyld     []byte  // the build's dyld, as the emulated CPU runs it
	mounts   []Mount // the home among them; longest guest path first
	procs    []*Process
	running  *Process                 // the process whose thread has, or last had, the turn
	services map[string]*service      // what launchd knows (see launchd.go)
	shared   []*sharedMemory          // every memory of the host's that mappings share (see shared.go)
	named    map[string]*sharedMemory // those shm_open knows by a name
	ioMain   *port                    // what IOKit's searches are sent to, once a process has asked for it
	deadline time.Time
	settling time.Time // once the main guest has exited: when the daemons are no longer waited for
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
}

// NewMachine makes a machine with no process yet.
func NewMachine(cfg Config) (*Machine, error) {
	if err := loadUnicorn(cfg.Unicorn); err != nil {
		return nil, err
	}
	dyld, err := thin(cfg.Dyld, cpuSubtypeARM64E)
	if err != nil {
		return nil, fmt.Errorf("dyld: %w", err)
	}
	if err := CheckMounts(cfg.Mounts); err != nil {
		return nil, err
	}
	mounts := append([]Mount{{GuestHome, cfg.Home}}, cfg.Mounts...)
	for i, m := range mounts {
		host, err := filepath.EvalSymlinks(m.Host)
		if err != nil {
			return nil, err
		}
		mounts[i] = Mount{canonical(m.Guest), host}
	}
	slices.SortStableFunc(mounts, func(a, b Mount) int { return len(b.Guest) - len(a.Guest) })
	return &Machine{cfg: cfg, dyld: dyld, mounts: mounts, services: map[string]*service{}, named: map[string]*sharedMemory{}}, nil
}

// Spawn makes a process of a guest, ready to run from dyld's entry point. Two guests may not
// provide the same service.
func (m *Machine) Spawn(g Guest) (p *Process, err error) {
	for _, name := range g.Services {
		if s := m.services[name]; s != nil {
			return nil, fmt.Errorf("the service %s is provided by both %s and %s", name, s.provider.exePath, g.Executable)
		}
	}
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
	if g.Root {
		p.uid, p.gid = 0, 0
	}
	for _, name := range g.Services {
		m.services[name] = &service{provider: p}
	}
	defer guard(&err)
	return p, p.load(append([]string{p.path}, g.Args...), g.Env)
}

// Main is the guest the run is for: the first one started.
func (m *Machine) Main() *Process { return m.procs[0] }

// Processes are every guest that was started, the main one first.
func (m *Machine) Processes() []*Process { return m.procs }

// Close releases every process, and then the memory they shared.
func (m *Machine) Close() {
	for _, p := range m.procs {
		p.Close()
	}
	for _, s := range m.shared {
		s.free()
	}
	m.shared = nil
}

// Run runs the guests until the main one exits. Any other end (a fault, something unimplemented,
// the timeout, nothing left that could run) is a *StopError, and Diagnostics describes it.
func (m *Machine) Run(timeout time.Duration) (err error) {
	defer guard(&err)
	m.deadline = time.Now().Add(timeout)
	m.running = m.procs[0]
	for {
		p := m.running
		if !p.step() {
			return nil
		}
		if p.exited {
			m.exit(p, fmt.Sprintf("with status %d", p.exitCode))
		}
		if (p.exited || p.reschedule()) && !m.schedule() {
			return nil
		}
	}
}

// exit is what follows a process's end, however it came to it: its rights end with it, and if
// it is the guest the run is for, the others have only a while longer.
func (m *Machine) exit(p *Process, how string) {
	p.early = p != m.procs[0] && !m.procs[0].exited
	p.exited = true
	p.note("exited %s", how)
	p.reap()
	if p == m.procs[0] {
		// The others are asked to end, as launchd asks its daemons when they are no longer
		// wanted, and have a while to do what they do about that.
		m.settling = time.Now().Add(settleTime)
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
		now := time.Now()
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
					// A wait for a message or a signal may go on for good: that is a service with no
					// client. Any other wait with no end is one that nothing can end.
					stuck = stuck || t.kind != waitPort && t.kind != waitSignal
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
		time.Sleep(time.Until(until))
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
