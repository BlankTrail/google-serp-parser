// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/api"
	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/run"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
	"github.com/blanktrail/google-serp-parser/internal/web"
)

// defaultServeAddr is where the interface listens when the caller names no
// address. It is loopback: the history is one machine's own, and a default that
// answered the network would put it on every one this machine is attached to.
const defaultServeAddr = "127.0.0.1:8080"

// lanServeAddr is where it listens instead when the settings open it to the
// network: every address this machine has, on the same port.
const lanServeAddr = "0.0.0.0:8080"

// listenOn is the address to bind, given what the caller asked for and what
// this machine's settings say.
//
// A caller who named an address gets it. Nothing in a settings file should
// quietly overrule a word on the command line — somebody who typed --addr said
// where they wanted it, and a program that listened somewhere else would be
// lying to them about their own instruction.
//
// The switch only reaches the default, and only with a password behind it. An
// interface open to a network with nothing to ask for is the settings, the
// queue and every result offered to whoever is on that network, so the switch
// alone is not enough to open it: without a password the program stays on
// loopback and says so.
func listenOn(asked string, saved settings.Settings, out io.Writer) string {
	if asked != defaultServeAddr {
		return asked
	}
	if !saved.LANAccess {
		return defaultServeAddr
	}
	if saved.LANPassword == "" {
		_, _ = fmt.Fprintln(out,
			"gserp: the settings open the interface to the network but hold no password for it, so it is listening on this machine only")
		return defaultServeAddr
	}
	return lanServeAddr
}

// The pool a job runs on when neither the caller nor this machine's own
// settings say otherwise. They are the run command's defaults, so a job set up
// in the browser costs what the same job costs from the command line.
const (
	defaultThreads = 2
	defaultPorts   = 3
)

// settingsName is what the file the interface saves its settings in is called.
// It sits beside the history rather than inside it, so a history that is
// copied, handed over or backed up does not carry the connection with it.
const settingsName = "gserp-settings.json"

// translationsName is the directory a language is added in, beside the program
// rather than beside the history: it is part of what this copy of the program
// says, and a history carried to another machine should not bring somebody's
// half-finished translation along with it.
const translationsName = "gserp-translations"

// serveOptions is everything the command was asked to do.
type serveOptions struct {
	Addr    string
	DB      string
	Threads int
	Ports   int
	// Trace writes a line for every request every identity makes: where it was
	// when it stopped waiting, and what it got back. It is off by default and is
	// meant to be turned on for as long as it takes to answer a question — a run
	// at a hundred threads writes a line per request per page, which is a great
	// deal of reading and a little slower than not writing it.
	Trace bool
	// Rests is where addresses found dead are written down, so a restart does
	// not walk back into every one of them. Nil keeps no such record, which is
	// what a command exercised without a history has.
	Rests rests
	// onListen is told the address the system actually gave, once it is bound
	// and before anything is served on it. It is how the tray icon knows where
	// to send a browser: a caller who asked for port zero has no other way to
	// learn it, and one who asked for a port somebody else holds must not be
	// sent to a page that is not there.
	onListen func(at string)
}

// serveFlags declares the flags. It is separate from the parsing so the help
// text can be checked against the flags themselves rather than against a list
// somebody has to remember to keep up.
//
// The two numbers are the run command's own defaults, so a job set up in the
// browser costs what the same job costs from the command line.
func serveFlags(opts *serveOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&opts.Addr, "addr", defaultServeAddr, "address to listen on")
	fs.StringVar(&opts.DB, "db", "gserp.db", "history database to open")
	fs.IntVar(&opts.Threads, "threads", defaultThreads, "queries taken at once")
	fs.IntVar(&opts.Ports, "ports", defaultPorts, "ports per thread")
	fs.BoolVar(&opts.Trace, "trace", false,
		"log every request an identity makes: where it waited and what came back")
	return fs
}

// serveCommand serves the browser interface until it is stopped.
func serveCommand(ctx context.Context, args []string, out io.Writer) error {
	var opts serveOptions
	fs := serveFlags(&opts)
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The interruption is heard here rather than in main, as it is for a run:
	// a test ends the context this one is built on and gets the same path a
	// user gets, without raising a signal at the process running the tests.
	ctx, stop := interruptible(ctx, out)
	defer stop()
	return serveInterface(ctx, out, opts)
}

// serveInterface opens the history, takes the address and serves until the
// context ends.
func serveInterface(ctx context.Context, out io.Writer, opts serveOptions) error {
	st, err := store.Open(opts.DB)
	if err != nil {
		return opts.scrubbed(err)
	}
	// Closed only after Serve has returned, which it does not do while a page is
	// still going out — so no request is ever left reading a history that has
	// been shut underneath it.
	defer func() { _ = st.Close() }()

	// The address is taken here rather than inside the server so that the line
	// printed below names the port the system actually gave. A caller who asked
	// for port zero has no other way to learn it. It is also taken before the
	// ports below are opened, which take a while: a browser opened alongside
	// this command waits on a socket that is already there rather than being
	// refused and shown an error page.
	// Read before the address is taken: where to listen is one of the things it
	// says, and a listener already bound cannot be moved.
	saved, hadSettings := opts.saved(io.Discard)

	// The exits this machine was already set up with, carried into the profile
	// a job now names. It writes on one start and does nothing on every one
	// after it; a failure here is not worth refusing to serve over, because the
	// screen that lists the profiles can make one.
	if made, err := st.CarryProxySettings(ctx, theFirstProfile(saved, hadSettings)); err != nil {
		_, _ = fmt.Fprintf(out, "the proxy settings could not be carried into a profile: %s\n",
			opts.clean(err.Error()))
	} else if made {
		_, _ = fmt.Fprintf(out, "the proxy settings were carried into a profile called %q\n",
			firstProfileName)
	}

	// Which developer's work brought the user here, told to the service once a
	// run. In the background and on its own timer: it has nothing to do with
	// parsing, the service may be slow to answer or not there at all, and a
	// program that made somebody wait on its own attribution would deserve
	// none.
	if saved.ControlURL != "" && saved.APIKey != "" {
		if client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey); err == nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), stampPatience)
				defer cancel()
				stampIntegration(ctx, client, opts.logger(os.Stderr))
			}()
		}
	}

	ln, err := net.Listen("tcp", listenOn(opts.Addr, saved, out))
	if err != nil {
		return opts.scrubbed(err)
	}

	// Read before the server is built, because the languages it finds are the
	// ones the switcher offers on the first page that goes out. Where this
	// program is, is something only the system can say; without that there is
	// no directory to look in, and both built-in languages are there already.
	if exe, err := os.Executable(); err == nil {
		opts.translate(out, filepath.Join(filepath.Dir(exe), translationsName))
	}

	// The history is also where the addresses found dead are kept, so a run
	// begins knowing what the last one learned.
	opts.Rests = st

	// Closed before the history, because a job it ends is written down as it
	// lets go of it. Close waits for that.
	sup, warm := opts.jobs(ctx, out, st)
	// What the standing set could not know when it was built: whether a job is
	// working through it. Everything that takes identities away waits on this
	// answer, so a number saved mid-run is brought about when the run lets go
	// rather than out from under it.
	warm.mu.Lock()
	warm.running = func() bool { _, running := sup.Running(); return running }
	warm.mu.Unlock()
	defer func() { _ = sup.Close() }()
	defer func() { _ = warm.Close() }()

	log := opts.logger(os.Stderr)
	pages, err := web.New(web.Config{
		Store:        st,
		Logger:       log,
		Supervisor:   sup,
		SettingsPath: opts.settingsPath(),
		Connect:      opts.connect,
		// What a saved number of warm identities does, at the moment it is saved
		// rather than at the next start.
		Standing: func(ctx context.Context, saved settings.Settings) error {
			return warm.bring(ctx, saved.HotPorts, deviceOr(saved.HotDevice))
		},
	})
	if err != nil {
		_ = ln.Close()
		return err
	}
	programs, err := opts.programmable(st, log, sup, warm)
	if err != nil {
		_ = ln.Close()
		return err
	}
	_, _ = fmt.Fprintf(out, "gserp is listening on http://%s — open that in a browser\n", ln.Addr())
	// Told here and not sooner: everything above can still end this without a
	// server, and a caller that opened a browser on the strength of an earlier
	// word would have sent it to a socket nothing ever answered on.
	if opts.onListen != nil {
		opts.onListen(ln.Addr().String())
	}
	// Everything the browser reaches goes behind the password when the interface
	// answers the network. The programmable half carries keys of its own and is
	// wrapped as well: a second door into the same house is a door.
	guarded := web.Locked(mount(pages.Handler(), programs.Handler()), lockFor(saved))
	return pages.ServeHandler(ctx, ln, guarded)
}

// programmable builds the interface a program reads, on the history, the queue
// and the log the pages already have.
//
// Sharing those is the whole of it. One history is why a job set up by a program
// is the job the browser lists, and one queue is why the machine runs one job at
// a time however that job was asked for. Two of either would be two products in
// one process, agreeing on nothing but the port.
func (o serveOptions) programmable(st *store.Store, log *slog.Logger,
	sup *web.Supervisor, warm *warmSet) (*api.Server, error) {
	cfg := api.Config{Store: st, Logger: log}
	// A supervisor that was never built is left out rather than passed on. The
	// interface asks whether it has a queue at all, and a pointer that is nil put
	// into an interface answers that question yes: it would take a job and fail
	// on it instead of saying plainly that it can run nothing.
	if sup != nil {
		cfg.Supervisor = sup
	}
	// A search answered inside the request goes to the identities directly rather
	// than through the queue, so it is handed a pool and not the queue in front of
	// it. That pool is this server's own and is not the one any job runs on: a job
	// raises its own and takes it down again, and an address that answered only
	// while a job happened to be running would be an address nobody could build
	// anything on. How large it is belongs in the settings and is not there yet.
	// The identities kept warm are what a search answered inside a request goes
	// through. A machine keeping none answers that address with a refusal rather
	// than opening a pool per request, which is what that address was never for.
	if pool := warm.Pool(); pool != nil {
		cfg.Search = api.SearchConfig{
			Searcher: &run.Attempt{Pool: pool},
			Ports:    pool.Stats().Ports,
		}
	}
	return api.New(cfg)
}

// mount puts the two interfaces on one address: the pages at the root, and the
// programmable interface under the two prefixes it is documented at.
//
// An address under /api/ that neither of them serves is still the programmable
// interface's to refuse. A program that mistyped one is answered in the single
// shape it parses every refusal in, rather than with a page written for somebody
// reading a screen.
//
// A pattern naming a whole path beats a pattern naming a prefix, whichever was
// registered first, so the three above stay with the pages.
func mount(pages, programs http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", pages)
	mux.Handle("/api/", programs)
	mux.Handle("/search", programs)
	for _, at := range web.BrowserPolls() {
		mux.Handle(at, pages)
	}
	return mux
}

// jobs hands the interface the way to open ports for a job, or says why it has
// none yet.
//
// There is always a supervisor. Neither a missing key nor ports that would not
// open is a reason to refuse to start: half of what this interface does is read
// a history and that half needs nothing opened. What such a server has is a
// supervisor with nothing to run on, which is what lets a connection set up in
// the browser be taken into use without the process being started again.
//
// No job runs on the pool that comes back. Each job raises one of its own, at
// the sizes it named, and gives it up when it lets go — so the pool here is
// opened for one thing only: the search answered inside a request, which cannot
// wait behind a queue and must go on answering between jobs as well as during
// them. It is opened at startup so that whether it answers does not depend on
// whether anybody has started a job. A server with nothing to run on hands back
// no pool, and the programmable side says so rather than waiting.
//
// Opening it is also the check that this connection works at all, which is why a
// failure here is reported the same way it always was: the alternative is a
// server that comes up looking healthy and refuses the first job an hour later.
//
// The key is read from the environment and is not a flag, for the reason it is
// not one anywhere else in this command: a key on a command line is a key in the
// shell history and in the process list of everyone on the machine. It is also
// read from the settings, because a connection set up in a browser that had to
// be set up again after every restart is one nobody would trust.
func (o serveOptions) jobs(ctx context.Context, out io.Writer, st *store.Store) (*web.Supervisor, *warmSet) {
	saved, configured := o.saved(out)
	threads, ports := o.runOn(saved, configured)

	// What this process was started with wins. Somebody who put a key in the
	// environment for this run meant it for this run, and a job raising its pool
	// an hour from now has to read the connection the same way this does or the
	// interface would be running on one connection and reporting another.
	fromEnv := os.Getenv(envAPIKey) != ""

	warm := &warmSet{log: o.logger(os.Stderr)}
	warm.dial = func(ctx context.Context, want int, device string) (*blanktrail.Pool, error) {
		if fromEnv {
			cfg := poolConfig(1, want)
			cfg.Specs = blanktrail.SpecsFor(device)
			cfg.Trace = o.tracer()
			cfg.OnLease = o.leaseTracer()
			return openPool(ctx, io.Discard, cfg)
		}
		return o.dial(ctx, saved, 1, want, device, 0)
	}

	if saved.APIKey == "" && !fromEnv {
		_, _ = fmt.Fprintf(out,
			"%s is not set and no connection has been saved: this interface shows the history and runs nothing until the connection is set up in it\n",
			envAPIKey)
		return web.NewSupervisorWithoutAPool(st), warm
	}

	// The identities this machine keeps open and warm. They are what the search
	// answered inside a request goes through, and what every job of the same kind
	// of result page runs on, grown to that job's size and shrunk back when it
	// ends. None is a machine that opens a pool for each job from cold, which is
	// what this program did until there was a choice.
	if err := warm.bring(ctx, saved.HotPorts, deviceOr(saved.HotDevice)); err != nil {
		_, _ = fmt.Fprintf(out,
			"the identities to keep warm could not be opened, so this interface shows the history and runs nothing until the connection is set up in it: %s\n",
			o.clean(err.Error()))
		_, _ = fmt.Fprintln(out, "gserp doctor prints the whole report")
		return web.NewSupervisorWithoutAPool(st), warm
	}
	if saved.HotPorts > 0 {
		_, _ = fmt.Fprintf(out, "%d identities are being kept warm for %s results\n",
			saved.HotPorts, deviceOr(saved.HotDevice))
	}

	// The two numbers are what a job that named no size runs at, and this is
	// where that stands in: they are the sizes this server was started with — the
	// -threads and -ports flags, or what a settings file the caller left the flags
	// alone for overrode them with. Every job written down before jobs carried
	// sizes reads back as nought, and the size this machine was started at is the
	// only one anybody on it has actually chosen.
	return web.NewSupervisorWatching(st, o.raise(saved, fromEnv, warm), ports, threads,
		o.threadTracer()), warm
}

// deviceOr is the kind of result page asked for, and a desktop where nobody
// asked. It is one function because the answer to an empty choice has to be the
// same everywhere it is read.
func deviceOr(device string) string {
	if device == "" {
		return blanktrail.DeviceDesktop
	}
	return device
}

// raise is how one job's own pool is opened.
//
// It is handed to the queue rather than called here, because a job's pool goes
// up when that job's turn comes and comes down when the job lets go. The sizes
// are the job's own, already stood in for where it named none, so nothing here
// decides how large anything is.
//
// The connection is this server's, read the same way it was read at startup, and
// captured rather than reread: the settings file may be rewritten while the
// server runs, and a job that raised its pool against a half-written file would
// fail for a reason nobody could reconstruct afterwards. Changing what later jobs
// connect through is the settings page's, and it does it by handing the queue
// something new.
//
// Nothing is printed. What openPool writes is written for somebody watching a
// command start, and a pool going up hours later for one job of many would print
// into the middle of whatever the operator is reading — and would be a second
// goroutine writing to a stream this command's caller owns. The refusal comes
// back as an error, and the queue puts it in the log against the job it belongs
// to.
func (o serveOptions) raise(saved settings.Settings, fromEnv bool, warm *warmSet) web.OpenPool {
	return func(ctx context.Context, ports, threads int, device string,
		cooldown time.Duration) (*blanktrail.Pool, error) {
		// A job of the kind the standing identities were opened for runs on them,
		// grown to its own size. It gives the growth back when it ends and the
		// standing ones stay warm for the next.
		//
		// A job of the other kind opens its own from cold: a phone's results are
		// not a desktop's, and an identity cannot be both.
		if pool, mine, err := warm.raiseFor(ctx, ports, threads, device); err != nil {
			return nil, err
		} else if mine {
			// The job's own pause, and nothing else's.
			//
			// A pool outlives what runs on it: the standing identities are opened
			// once, as one thread's worth of however many this machine keeps, and
			// every job afterwards runs on that same pool grown to its size. The
			// gap between two requests on one identity was worked out at that
			// opening and never revisited, so a job inherited a number describing
			// something else — ten identities opened for one thread came out at
			// thirty-five seconds, and a job of fifty threads on a hundred of them
			// could be handed a hundred divided by thirty-five identities a second,
			// a hundred and seventy-one requests a minute, whatever it asked for.
			// Traced on that job: the shortest gap between two leases of one
			// identity was 35.0 seconds across all hundred, and its threads spent
			// three fifths of the run queueing for a pool that was idle.
			//
			// Nought is nought. A job that names no pause is a job that wants none,
			// and deriving one for it is this program pacing a run nobody asked to
			// have paced.
			pool.PaceAt(cooldown)
			return pool, nil
		}
		if fromEnv {
			cfg := poolConfig(threads, ports)
			cfg.Specs = blanktrail.SpecsFor(device)
			cfg.Trace = o.tracer()
			cfg.OnLease = o.leaseTracer()
			pool, err := openPool(ctx, io.Discard, cfg)
			if err != nil {
				return nil, err
			}
			pool.PaceAt(cooldown)
			return pool, nil
		}
		pool, err := o.dial(ctx, saved, threads, ports, device, cooldown)
		if err != nil {
			return nil, err
		}
		pool.PaceAt(cooldown)
		return pool, nil
	}
}

// connect opens the ports a connection just saved in the browser describes.
//
// It is what the interface is handed to build a pool with, and it is written
// here rather than there because how many ports a run opens, how long they rest
// and what they are opened as is this command's to decide — the same decision
// the estimate on the form is worked out from.
//
// The numbers are the ones just saved rather than the ones this process was
// started with: somebody who has this moment typed a port count into the
// settings has said which they want, and a flag from last week that quietly won
// would make the box on the screen a box that does nothing.
func (o serveOptions) connect(ctx context.Context, saved settings.Settings,
	ports, threads int, device string, cooldown time.Duration) (*blanktrail.Pool, error) {
	// The size and the kind of result page come from the job, through the
	// supervisor, and the connection from the settings. Neither knows the other's
	// half, and this is where the two are put together.
	return o.dial(ctx, saved, threads, ports, device, cooldown)
}

// dial opens a pool against the connection described, after the check that says
// whether it would get through at all.
//
// The check is the one gserp doctor runs, for the reason it is run before a run
// from the command line: the most common way a job is dead on arrival is one a
// single request would have shown.
func (o serveOptions) dial(ctx context.Context, saved settings.Settings, threads, ports int,
	device string, cooldown time.Duration) (*blanktrail.Pool, error) {
	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		return nil, o.scrubbed(err)
	}
	if threads < 1 {
		threads = o.Threads
	}
	if ports < 1 {
		ports = o.Ports
	}
	cfg := poolConfig(threads, ports)
	cfg.Spec.Protocol = saved.PortProtocol
	cfg.MaxPerUpstream = saved.ThreadsPerUpstream
	cfg.RenewAfterInterval = saved.RenewEvery
	// Which kind of result page this job asked for. Desktop opens every port
	// under the one default template, as this program always has; mobile hands
	// the pool two named templates and it spreads the ports over both, so a run
	// on phones is a run on more than one phone.
	cfg.Specs = blanktrail.SpecsFor(device)
	for i := range cfg.Specs {
		cfg.Specs[i].Spec.Protocol = saved.PortProtocol
	}
	cfg.Trace = o.tracer()
	cfg.OnLease = o.leaseTracer()
	// The gap between two requests on one identity is the job's. Nought is a job
	// that named none, and the pool then derives it from the ports and the pause
	// range — which is the one place that number is worked out.
	if cooldown > 0 {
		cfg.Cooldown = cooldown
	}

	report, err := o.checked(ctx, client, cfg.Size())
	if err != nil {
		return nil, err
	}
	cfg.Client = client
	cfg.CA = report.CA

	if saved.Proxy.Kind == settings.ProxyGateways {
		// The gateways are asked for now rather than kept: what is behind a name
		// lives in BlankTrail and can be changed there between one job and the
		// next.
		ch, err := o.gatewayChannel(ctx, client, saved)
		if err != nil {
			return nil, o.scrubbed(err)
		}
		cfg.Channels = []blanktrail.Channel{ch}
	} else if saved.Proxy.Kind != "" {
		// The list is loaded here and reloaded on its own interval afterwards, so a
		// list that changes during a run is a list this pool follows.
		// How long a failed address is left out is the operator's, because how
		// fast a gateway's exits turn over is a property of the list they bought
		// and not of this program. Nought is the documented default.
		bench := []blanktrail.RotorOption{blanktrail.WithOnBench(o.remember)}
		// Passed as it stands, nought included: nought is an operator saying no
		// address is ever put away, not an operator saying nothing.
		bench = append(bench, blanktrail.WithRest(saved.Proxy.Ban))
		rotor, err := blanktrail.NewRotor(ctx, listFrom(saved.Proxy), bench...)
		if err != nil {
			return nil, o.scrubbed(err)
		}
		// What this machine already knows about the list. Without it every start
		// walks back into the addresses the last run spent its time finding dead,
		// and on a large cheap list that is most of them.
		o.recall(ctx, rotor)
		cfg.Channels = []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)}
	}

	pool, err := blanktrail.NewPool(ctx, cfg)
	if err != nil {
		return nil, o.scrubbed(err)
	}
	return pool, nil
}

// rests is where the addresses this machine has found dead are kept between
// runs. It is an interface rather than the history itself so that the command
// can be exercised without one, and nil is a machine that keeps no such record.
type rests interface {
	Rested(ctx context.Context) (map[string]time.Time, error)
	Rest(ctx context.Context, key string, since time.Time) error
	ForgetRestsBefore(ctx context.Context, cut time.Time) error
}

// remember writes down that an address was found dead, as the pool finds it.
//
// It is written as it happens rather than gathered at shutdown, because a
// program that is killed never reaches a shutdown — and the run that has just
// spent an hour learning which addresses are dead is exactly the one worth not
// losing. A failure to write is not worth stopping a run for: the cost is one
// address tried again after the next start.
func (o serveOptions) remember(key string, since time.Time) {
	if o.Rests == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = o.Rests.Rest(ctx, key, since)
}

// recall tells a fresh rotor what this machine already knows, and forgets what
// has gone stale while it is there.
//
// The cut is generous on purpose. How long a rest lasts is the rotor's own
// business and it drops the ones that have run out as it takes them in, so what
// is deleted here is only what could not matter to any rotor: a day is several
// times the rest and leaves nothing useful behind.
func (o serveOptions) recall(ctx context.Context, rotor *blanktrail.Rotor) {
	if o.Rests == nil {
		return
	}
	_ = o.Rests.ForgetRestsBefore(ctx, time.Now().Add(-24*time.Hour))
	kept, err := o.Rests.Rested(ctx)
	if err != nil || len(kept) == 0 {
		return
	}
	rotor.Restore(kept)
}

// listScheme is how an address is reached when its line does not say. Lists are
// mostly sold without one, and this is what the rest of this command has always
// assumed of them.
const listScheme = "socks5"

// listFrom is where the addresses come from, as the settings describe it.
//
// It is one function rather than a few lines at the place a pool is opened
// because every field of it is a box somebody filled in: a place that quietly
// read its own interval, or its own idea of file or address, would leave a box
// on the settings page that changes nothing and says nothing about it.
func listFrom(p settings.ProxySource) blanktrail.Source {
	return blanktrail.Source{
		Kind:          p.Kind,
		Location:      p.Location,
		Refresh:       p.Refresh,
		DefaultScheme: listScheme,
	}
}

// saved is what was set in the browser, and whether anything was.
//
// The file does not exist until somebody opens the settings, and a machine
// nobody has configured has to run on what it was started with: answering there
// with the settings package's own defaults would open a pool of a size nobody
// on this machine ever asked for.
//
// A file that is there and cannot be read is said out loud rather than passed
// over. It is the file somebody's connection is kept in, and a program that
// quietly ran without it would look like one that had lost their settings.
func (o serveOptions) saved(out io.Writer) (settings.Settings, bool) {
	path := o.settingsPath()
	if _, err := os.Stat(path); err != nil {
		return settings.Settings{}, false
	}
	s, err := settings.Load(path)
	if err != nil {
		_, _ = fmt.Fprintf(out,
			"the saved settings could not be read, so this interface runs on what it was started with: %s\n",
			o.clean(err.Error()))
		return settings.Settings{}, false
	}
	return s, true
}

// firstProfileName is what the profile carried out of the settings file is
// called.
//
// A word rather than a sentence, and the same word in both languages: it is
// data and not a label, it is written once and read forever after, and a name
// translated at the moment it was written would be the wrong language for
// whoever changes the interface to the other one tomorrow. It is theirs to
// rename.
const firstProfileName = "Default"

// theFirstProfile is the settings file's own exits, as a profile.
//
// A machine that has been running since before profiles existed keeps its list,
// its ban and its gateways in that file, so those are what the first profile is
// made of. A machine with no settings file at all — a fresh install — gets one
// made of the program's own defaults instead of one made of zeroes: nought ban
// and nought renewal are real answers that mean never, and a fresh install
// that quietly chose them would be a fresh install nobody set up that way.
func theFirstProfile(saved settings.Settings, hadSettings bool) store.Profile {
	if !hadSettings {
		saved = settings.Defaults()
	}
	return store.Profile{
		Name:               firstProfileName,
		Kind:               saved.Proxy.Kind,
		Location:           saved.Proxy.Location,
		Refresh:            saved.Proxy.Refresh,
		Ban:                saved.Proxy.Ban,
		ThreadsPerUpstream: saved.ThreadsPerUpstream,
		RenewEvery:         saved.RenewEvery,
		Protocol:           saved.PortProtocol,
		Gateways:           saved.Proxy.Gateways,
		Default:            true,
	}
}

// settingsPath is the file the connection is kept in: beside the history rather
// than inside it, so a history that is copied, handed over or backed up does not
// carry the connection with it.
func (o serveOptions) settingsPath() string {
	return filepath.Join(filepath.Dir(o.DB), settingsName)
}

// translate reads the languages kept beside this program and says what came of
// it.
//
// Every line here is a report about somebody else's files, so none of them
// stops the command: a translation that could not be read leaves the interface
// speaking what it was built to speak, which is what it does on the machines
// that have no such directory at all — nearly all of them.
//
// A language short of phrases is named out loud with the count. It is the one
// thing about a translation that is invisible from inside the browser: the page
// renders, in English, and the reader who asked for the other language has no
// way of telling a missing phrase from a decision.
func (o serveOptions) translate(out io.Writer, dir string) {
	added, incomplete, err := web.LoadTranslations(dir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "a translation beside this program could not be read, and the rest are in use: %s\n",
			o.clean(err.Error()))
	}
	for _, lang := range added {
		_, _ = fmt.Fprintf(out, "this interface also answers in %s, from a file beside the program\n", lang)
	}
	for _, lang := range slices.Sorted(maps.Keys(incomplete)) {
		_, _ = fmt.Fprintf(out, "the %s translation is short of %d phrases, which are shown in English: %s\n",
			lang, len(incomplete[lang]), strings.Join(incomplete[lang], " "))
	}
}

// runOn is what a job runs on when the job itself named nothing.
//
// It is the flags this command was started with, and nothing else. The settings
// file used to have a say here, and it no longer does: how wide a job runs is
// the job's own, saved with it and edited on its page, and a machine-wide
// answer standing behind that would be a second opinion nobody asked for. What
// the file still decides is the connection every pool is opened through.
func (o serveOptions) runOn(settings.Settings, bool) (threads, ports int) {
	return o.Threads, o.Ports
}

// logger is where the server says what went wrong.
//
// It goes to standard error, stamped and levelled, rather than onto standard
// output among the lines written for a person to read: the two are read by
// different things, and only one of them can be redirected away.
// tracer turns the -trace flag into what a pool wants: a function told how each
// request went, or nothing at all.
//
// Nothing at all rather than a function that returns early, because the pool
// installs its timing hooks only where somebody is listening, and a listener
// that discards what it hears is the same cost as one that does not.
func (o serveOptions) tracer() func(blanktrail.RequestTrace) {
	if !o.Trace {
		return nil
	}
	log := o.logger(os.Stderr)
	return func(tr blanktrail.RequestTrace) {
		// The stages are cumulative from the start of the request, so each one
		// reads as "by when", and a stage that never happened is absent rather
		// than nought — which is the difference between "instant" and "never".
		fields := []any{"port", tr.Port, "attempt", tr.Attempt, "reused", tr.Reused,
			"total", tr.Total.Round(time.Millisecond)}
		for _, stage := range []struct {
			name string
			at   time.Duration
		}{
			{"connected", tr.Connect}, {"tls", tr.TLS},
			{"sent", tr.Wrote}, {"first byte", tr.FirstByte},
		} {
			if stage.at > 0 {
				fields = append(fields, stage.name, stage.at.Round(time.Millisecond))
			}
		}
		switch {
		case tr.Err != nil:
			fields = append(fields, "error", o.clean(tr.Err.Error()))
		default:
			fields = append(fields, "status", tr.Status)
		}
		log.Info("a request through an identity", fields...)
	}
}

// threadTracer turns the -trace flag into what a run wants: a line for every
// stage a thread passes through and how long it took.
//
// It is what makes a slow job readable. A thread has one place where waiting is
// the work — waiting on an answer from the far end — so a line saying it spent
// four seconds paused, or nine waiting for an identity, is a line pointing at
// something to remove.
func (o serveOptions) threadTracer() run.Watch {
	if !o.Trace {
		return nil
	}
	log := o.logger(os.Stderr)
	return func(st run.Step) {
		fields := []any{"thread", st.Thread, "stage", string(st.Stage),
			"took", st.Took.Round(time.Millisecond)}
		if st.Query != "" {
			fields = append(fields, "query", st.Query)
		}
		if st.Err != nil {
			fields = append(fields, "error", o.clean(st.Err.Error()))
		}
		log.Info("a thread of the run", fields...)
	}
}

// leaseTracer turns the -trace flag into what a pool wants when it hands out an
// identity: a line saying which one, and how long the caller stood waiting.
//
// A trace of requests alone cannot say why a wide pool is quiet — a port with
// nothing going through it reads the same whether every thread is busy
// elsewhere or every thread is queueing for it.
func (o serveOptions) leaseTracer() func(blanktrail.LeaseTrace) {
	if !o.Trace {
		return nil
	}
	log := o.logger(os.Stderr)
	return func(l blanktrail.LeaseTrace) {
		// warm and served are the two the measurement turns on: a lease to an
		// identity that has never answered is about to pay one to three minutes,
		// and one to an identity that has answered is about to cost a second or
		// two. Counting them apart is how "what does another port per thread
		// buy" stops being an argument and becomes a number.
		log.Info("an identity was handed out",
			"port", l.Port,
			"waited", l.Waited.Round(time.Millisecond),
			"warm", l.Warm,
			"served", l.Requests,
			"of", l.Ports)
	}
}

func (o serveOptions) logger(w io.Writer) *slog.Logger {
	return slog.New(&scrubbing{Handler: slog.NewTextHandler(w, nil), clean: o.clean})
}

// clean takes out of a line everything this command must not print: the values
// it was given in the environment, and where on this machine the history is
// kept.
func (o serveOptions) clean(text string) string {
	return scrubDB(text, o.DB)
}

// scrubbed rewrites an error so it can be printed.
//
// The chain goes with the path. Nothing above this command tells these errors
// apart, and a wrapped error would carry the original text — the very thing
// being taken out — along inside it.
func (o serveOptions) scrubbed(err error) error {
	return errors.New(o.clean(err.Error()))
}

// scrubbing is a log handler that puts every line through the filter the rest
// of the command prints through. The server behind it is handed a history and a
// logger and knows neither what is secret nor where anything on this machine
// lives, so the filtering belongs on this side of it.
type scrubbing struct {
	slog.Handler
	clean func(string) string
}

func (h *scrubbing) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.clean(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.cleanAttr(a))
		return true
	})
	return h.Handler.Handle(ctx, out)
}

func (h *scrubbing) WithAttrs(attrs []slog.Attr) slog.Handler {
	cleaned := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		cleaned[i] = h.cleanAttr(a)
	}
	return &scrubbing{Handler: h.Handler.WithAttrs(cleaned), clean: h.clean}
}

func (h *scrubbing) WithGroup(name string) slog.Handler {
	return &scrubbing{Handler: h.Handler.WithGroup(name), clean: h.clean}
}

// cleanAttr rewrites the values a line can carry a path or a key inside. A
// number, a time or a boolean cannot hold one, and rewriting those would turn
// them into text for nothing.
func (h *scrubbing) cleanAttr(a slog.Attr) slog.Attr {
	switch v := a.Value.Resolve(); v.Kind() {
	case slog.KindString, slog.KindAny:
		return slog.String(a.Key, h.clean(v.String()))
	case slog.KindGroup:
		inner := make([]any, 0, len(v.Group()))
		for _, g := range v.Group() {
			inner = append(inner, h.cleanAttr(g))
		}
		return slog.Group(a.Key, inner...)
	default:
		return a
	}
}

// checkRemembered is how long the connection check is believed.
//
// It is three seconds of asking the service the same four questions, and it
// runs before every pool goes up. A machine running ten jobs one after another
// spent half a minute of somebody's waiting on answers that had not changed. A
// minute is short enough that a connection which breaks is noticed by the job
// after next rather than the one after that, and long enough that a queue of
// jobs pays for it once.
const checkRemembered = time.Minute

// lastCheck is the last check that passed, and when.
//
// It is one value for the process rather than one per caller, because there is
// one service being asked about: two callers checking it a second apart are
// asking the same question, and the second of them is waiting for an answer the
// first already has.
var lastCheck struct {
	mu   sync.Mutex
	at   time.Time
	ca   *x509.CertPool
	size int
}

// checked is the connection check, remembered for a minute.
//
// A check that failed is never remembered: a machine whose connection is broken
// has to be able to fix it and try again at once, and a refusal held for a
// minute would be a minute of a page that says the same wrong thing however
// many times it is pressed.
//
// The size is part of what is remembered because it is part of what was asked:
// the check reports what a pool of that many ports would run into, and a larger
// pool asked against a smaller answer would be told nothing about the ports it
// added.
func (o serveOptions) checked(ctx context.Context, client *blanktrail.Client, size int) (blanktrail.Report, error) {
	lastCheck.mu.Lock()
	fresh := lastCheck.ca != nil &&
		time.Since(lastCheck.at) < checkRemembered &&
		size <= lastCheck.size
	ca := lastCheck.ca
	lastCheck.mu.Unlock()
	if fresh {
		return blanktrail.Report{CA: ca}, nil
	}

	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: checkedDomains,
		Ports:   size,
	})
	if !report.OK() {
		return blanktrail.Report{}, errors.New(
			"this connection would not get through; the settings page says what the check found")
	}
	lastCheck.mu.Lock()
	lastCheck.at, lastCheck.ca, lastCheck.size = time.Now(), report.CA, size
	lastCheck.mu.Unlock()
	return report, nil
}

// warmSet is the identities this machine keeps open and warm, and the one thing
// that knows how many there are.
//
// It is a value the server holds rather than a pool passed about, because the
// number can change while the program runs: somebody types ten into the
// settings and presses save, and what has to happen then is opening ten ports —
// not writing ten down for the next start.
//
// The pool inside it is never replaced, only grown and shrunk. The synchronous
// search holds it from the moment the server was built, and a pool swapped
// underneath that would leave the search answering through identities nobody
// keeps warm.
type warmSet struct {
	mu     sync.Mutex
	pool   *blanktrail.Pool
	device string
	// stop ends the warmer that is keeping this set warm, and is nil when none
	// is running.
	stop context.CancelFunc
	// dial opens a pool of a given size and kind, and log is where a warming
	// that did not get through is reported.
	dial func(ctx context.Context, ports int, device string) (*blanktrail.Pool, error)
	log  *slog.Logger
	// running says whether a job is working through these identities. It is the
	// supervisor's answer, wired in once the server is built, and a set nobody
	// has wired it into is a set no job runs on.
	running func() bool
	// owed is a number and a kind somebody saved while a job was running, to be
	// brought about when the job lets go. Nil is nothing owed.
	owed *standing
}

// standing is a number of identities of one kind: what the settings page last
// asked this machine to keep.
type standing struct {
	want   int
	device string
}

// busy says whether a job is working through these identities right now.
func (w *warmSet) busy() bool {
	w.mu.Lock()
	running := w.running
	w.mu.Unlock()
	return running != nil && running()
}

// Pool is what the set holds, for the search that answers inside a request.
func (w *warmSet) Pool() *blanktrail.Pool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pool
}

// raiseFor hands a job the standing pool grown to its size, or nothing when the
// job asks for the other kind of result page.
func (w *warmSet) raiseFor(ctx context.Context, ports, threads int, device string) (*blanktrail.Pool, bool, error) {
	w.mu.Lock()
	pool, kind := w.pool, w.device
	w.mu.Unlock()
	if pool == nil || device != kind {
		return nil, false, nil
	}
	if extra := ports*threads - pool.Stats().Ports; extra > 0 {
		if err := pool.Grow(ctx, extra); err != nil {
			return nil, false, err
		}
	}
	return pool, true, nil
}

// bring makes the set what the settings now say: so many identities of such a
// kind, warmed and kept warm.
//
// Every case is one of four, and each is what it sounds like: none wanted and
// none held does nothing; none wanted and some held gives them back; a kind that
// has changed is a different set of identities and the old ones go; and a number
// that has changed grows or shrinks what is there.
func (w *warmSet) bring(ctx context.Context, want int, device string) error {
	w.mu.Lock()
	pool, kind, stop := w.pool, w.device, w.stop
	w.mu.Unlock()

	// A job is working through these identities, so nothing here happens now.
	//
	// Everything below takes ports away: a smaller number closes them, a
	// different kind closes them all. Done underneath a running job it took a
	// third of its identities away and left it there — measured on a live run,
	// a job raised on a hundred went on at five and eight tenths queries a
	// minute on the thirty it was still holding, with no way back, because
	// nothing grows a pool a second time. What was asked for is remembered and
	// brought about when the job lets go.
	if pool != nil && w.busy() {
		w.mu.Lock()
		w.owed = &standing{want: want, device: device}
		w.mu.Unlock()
		return nil
	}

	if want <= 0 || (pool != nil && device != kind) {
		if stop != nil {
			stop()
		}
		if pool != nil {
			// Closed whole, standing marks and all: these are no longer the
			// identities this machine keeps, and a shrink would leave them open
			// with nothing warming them.
			_ = pool.Close()
		}
		w.mu.Lock()
		w.pool, w.device, w.stop = nil, "", nil
		w.mu.Unlock()
		if want <= 0 {
			return nil
		}
		pool = nil
	}

	if pool == nil {
		opened, err := w.dial(ctx, want, device)
		if err != nil {
			return err
		}
		opened.KeepWarm()
		w.start(opened, device)
		return nil
	}

	// The same kind, a different number. Grown ports are standing ones too: the
	// set is what this machine keeps, and half of it kept and half given back to
	// the next job would be a number nobody could read off the screen.
	switch have := pool.Stats().Ports; {
	case want > have:
		if err := pool.Grow(ctx, want-have); err != nil {
			return err
		}
		// Grown ports are standing ones too: the set is what this machine keeps,
		// and half of it kept and half given back to the next job would be a
		// number nobody could read off the screen.
		pool.KeepWarm()
	case want < have:
		if _, err := pool.ReduceTo(ctx, want); err != nil {
			return err
		}
	}
	return nil
}

// start puts a pool in place and sets a warmer on it, ending whatever warmer was
// there before.
func (w *warmSet) start(pool *blanktrail.Pool, device string) {
	ctx, stop := context.WithCancel(context.Background())
	w.mu.Lock()
	if w.stop != nil {
		w.stop()
	}
	w.pool, w.device, w.stop = pool, device, stop
	log := w.log
	w.mu.Unlock()

	go (&run.Warmer{Pool: pool, Log: log}).Run(ctx)
	go w.keep(ctx)
}

// tendRound is how often the set looks to see whether it owes somebody a
// change. It is short against how long a job runs and long against nothing.
const tendRound = 20 * time.Second

// keep brings about what a settings page asked for while a job was running.
//
// It is a loop rather than a hook on the job ending, because what it applies is
// the last thing somebody asked for, and somebody may ask twice before any job
// finishes. Reading it here means the newest answer wins and none of the older
// ones is applied on the way.
func (w *warmSet) keep(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(tendRound):
		}
		w.mu.Lock()
		owed := w.owed
		w.mu.Unlock()
		if owed == nil || w.busy() {
			continue
		}
		w.mu.Lock()
		w.owed = nil
		w.mu.Unlock()
		if err := w.bring(ctx, owed.want, owed.device); err != nil && w.log != nil {
			w.log.Info("the identities kept warm could not be brought to what was saved",
				"want", owed.want, "kind", owed.device, "err", err)
		}
	}
}

// Close gives up the identities and stops warming them.
func (w *warmSet) Close() error {
	w.mu.Lock()
	pool, stop := w.pool, w.stop
	w.pool, w.stop = nil, nil
	w.mu.Unlock()
	if stop != nil {
		stop()
	}
	if pool == nil {
		return nil
	}
	return pool.Close()
}

// lockFor is the password the pages ask for, or nothing when they ask for none.
//
// It is empty unless the interface is open to the network. On loopback the
// person who can reach the port is the person at the machine, who has the
// settings file and the history already.
func lockFor(saved settings.Settings) string {
	if !saved.LANAccess {
		return ""
	}
	return saved.LANPassword
}

// gatewayChannel builds the egress channel for a job that runs on stored VPN
// gateways.
//
// The names chosen are checked against what the service holds now. A name that
// has gone is passed over and said out loud: a job started on nine of the ten
// gateways somebody ticked is a job that runs, and one refused over the tenth is
// an evening lost to a configuration deleted last week. Nothing to run on at all
// is a different matter and is an error.
func (o serveOptions) gatewayChannel(ctx context.Context, client *blanktrail.Client,
	saved settings.Settings) (blanktrail.Channel, error) {
	list, err := client.Gateways(ctx)
	if err != nil {
		return nil, err
	}
	if !list.Available {
		return nil, fmt.Errorf("blanktrail: the gateways are not usable: %s", list.Reason)
	}
	have := map[string]bool{}
	for _, g := range list.Gateways {
		have[g.Name] = true
	}
	var chosen []string
	gone := 0
	for _, name := range saved.Proxy.Gateways {
		if have[name] {
			chosen = append(chosen, name)
			continue
		}
		gone++
	}
	if gone > 0 {
		o.logger(os.Stderr).Info("some chosen gateways are no longer on the service",
			"missing", gone, "running on", len(chosen))
	}
	if len(chosen) == 0 {
		return nil, errors.New("blanktrail: none of the chosen gateways are on the service any more")
	}
	opts := []blanktrail.RotorOption{}
	opts = append(opts, blanktrail.WithRest(saved.Proxy.Ban))
	rotor := blanktrail.NewStaticRotor(blanktrail.GatewayUpstreams(chosen), opts...)
	return blanktrail.NewGatewayListChannel("gateways", rotor), nil
}

// stampPatience is how long the one attribution request is given. It is short
// on purpose: nothing waits on it, and a service that is not answering has
// something more pressing wrong with it than this.
const stampPatience = 10 * time.Second
