//go:build live

// SPDX-License-Identifier: MIT

package run

// Where a thread of a run spends its time.
//
// A run of a hundred threads reported seventeen pages a minute against a
// thousand and more from the arrangement before it, with the challenge solver
// not obviously to blame: the screen showed no queue for it. Seventeen pages a
// minute across ninety-one sessions in hand is five minutes for one page, and
// nothing in a request costs five minutes — a request that meets Google's check
// waits tens of seconds for it, and one that does not is over in a few even
// through a poor address.
//
// So the time is going somewhere nobody was measuring. This measures it: the
// census says where the threads of the run stand and how much of the run's time
// each place has taken between them, and the trace underneath says what one
// request is made of — the tunnel, the handshake, the wait for the first byte,
// and how many attempts it took.
//
// It runs narrow on purpose. If a thread already spends minutes on a page with
// twenty of them, the width is not what does it; if it does not, the width is
// where to look next.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
	"github.com/blanktrail/google-serp-parser/internal/store"
	_ "modernc.org/sqlite"
)

// The run measured here is the shape of the job that is slow: as many threads,
// as deep a walk, the same rest between two requests on one session and the
// same number of sessions an address may carry. A narrower measurement would
// answer a question nobody asked.
var (
	// paceThreads is how wide the measured run is.
	paceThreads = envInt("GSERP_PACE_THREADS", 100)
	// paceFor is how long it is left to run before the reading is taken.
	paceFor = time.Duration(envInt("GSERP_PACE_SECONDS", 720)) * time.Second
	// paceEvery is how often the reading is printed while it runs, so the
	// widening and the speed it settles at can be told apart.
	paceEvery = 30 * time.Second
	// pacePages is how deep each phrase is taken.
	pacePages = envInt("GSERP_PACE_PAGES", 10)
	// paceTries is how many identities one phrase may be carried to.
	paceTries = envInt("GSERP_PACE_TRIES", 30)
)

// pacePhrases are enough that the threads never run out of work to open: each
// is walked ten pages deep, so a hundred threads have three pages each in hand
// before one has to wait.
var pacePhrases = []string{
	"купить ноутбук", "ремонт квартир", "доставка цветов", "аренда авто",
	"пластиковые окна", "натяжные потолки", "стоматология цены", "фитнес клуб",
	"курсы английского", "детский сад частный", "грузоперевозки город",
	"установка кондиционеров", "ремонт телефонов", "изготовление мебели",
	"клининг после ремонта", "свадебный фотограф", "автосервис рядом",
	"юридические услуги", "бухгалтерское сопровождение", "строительство домов",
	"металлопрокат цена", "спецодежда оптом", "типография визитки",
	"ремонт холодильников", "сантехник вызов", "электрик на дом",
	"вскрытие замков", "эвакуатор недорого", "прокат инструмента",
	"подарочные наборы", "остекление балконов", "укладка ламината",
	"ремонт компьютеров", "установка сигнализации", "бурение скважин",
	"септик под ключ", "ландшафтный дизайн", "теплицы из поликарбоната",
	"кровельные работы", "фундамент цена", "сварочные работы",
	"шкаф купе на заказ", "кухни на заказ", "матрасы ортопедические",
	"жалюзи на окна", "двери межкомнатные", "плитка керамогранит",
	"обои под покраску", "краска фасадная", "утеплитель минвата",
	"трубы полипропиленовые", "радиаторы отопления", "котлы газовые",
	"насосы для скважин", "генераторы бензиновые", "компрессоры воздушные",
	"станки деревообрабатывающие", "инструмент профессиональный",
	"крепёж оптом", "лакокрасочные материалы", "сухие смеси строительные",
	"аренда опалубки", "бетон с доставкой", "щебень песок",
	"кирпич облицовочный", "блоки газобетонные", "профнастил оцинкованный",
	"мебельная фурнитура", "стеллажи металлические", "тележки складские",
	"весы промышленные", "холодильное оборудование", "печи для пиццы",
	"кофемашины аренда", "посуда для ресторанов", "форма для поваров",
	"вывески наружная реклама", "печать баннеров", "сувениры с логотипом",
	"пошив штор", "химчистка ковров", "мойка окон альпинисты",
	"дезинсекция помещений", "уборка снега", "спил деревьев",
	"вывоз грунта", "аренда экскаватора", "услуги автовышки",
	"перевозка пианино", "переезд офиса", "хранение вещей склад",
	"упаковочные материалы", "стрейч плёнка", "гофрокороба",
	"этикетки самоклеящиеся", "маркировка товара", "штрихкод сканеры",
	"кассовое оборудование", "терминалы оплаты", "сейфы офисные",
	"видеонаблюдение установка", "домофоны обслуживание", "шлагбаумы автоматика",
	"ворота секционные", "заборы под ключ", "теплый пол монтаж",
	"вентиляция проектирование", "кондиционер обслуживание", "тепловые завесы",
	"отопление частного дома", "водоснабжение дачи", "канализация монтаж",
	"электромонтаж под ключ", "щиты учёта", "молниезащита здания",
	"пожарная сигнализация", "огнезащитная обработка", "аттестация рабочих мест",
	"медкнижка оформление", "охрана труда обучение", "первая помощь курсы",
	"права категории в", "техосмотр авто", "страхование осаго",
	"шины зимние купить", "диски литые", "аккумуляторы для авто",
	"автозапчасти иномарки", "масло моторное", "антифриз оптом",
	"кузовной ремонт", "полировка кузова", "тонировка стёкол",
	"детейлинг цена", "химчистка салона", "установка автозвука",
	"прокат авто без водителя", "трансфер аэропорт", "такси межгород",
	"грузчики на час", "разнорабочие смена", "подсобные рабочие",
	"вахтовый метод работа", "вакансии водитель", "поиск персонала",
}

func TestLivePace_SaysWhereAThreadOfARunSpendsItsTime(t *testing.T) {
	ctx := context.Background()
	control, key, listURL := liveEnv(t)
	// A run on the service's own gateways goes out through them instead of a
	// list: GSERP_PACE_GATEWAYS names them, separated by commas, or says "all".
	onGateways := envOr("GSERP_PACE_GATEWAYS", "")
	var ups []blanktrail.Upstream
	if onGateways == "" {
		ups = paceAddresses(ctx, t, listURL)
	}
	// Who resolves the name. Read as socks5 the service resolves it and hands
	// the proxy an address; read as socks5h the proxy is handed the name and
	// resolves it itself — which is the exit's own view of where Google is, and
	// one round trip through the chain fewer.
	if scheme := envOr("GSERP_PACE_SCHEME", ""); scheme != "" {
		for i := range ups {
			ups[i].Scheme = scheme
		}
		logf(t, "MEASUREMENT the addresses are used as %s", scheme)
	}
	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		fatalf(t, "control client: %v", err)
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"}, Ports: paceThreads})
	if !pre.OK() {
		for _, f := range pre.Findings {
			logf(t, "MEASUREMENT preflight: [%s] %s — %s", f.Severity, f.Title, f.Detail)
		}
		t.Skip("preflight refused the run")
	}
	// Where the sessions are kept. A run of its own starts from none; a run that
	// is one step of several — the same list asked wider and wider — names a
	// file in GSERP_PACE_STORE and takes up the sessions the step before it
	// made, so each step measures its width rather than a cold start.
	at := envOr("GSERP_PACE_STORE", filepath.Join(t.TempDir(), "sessions.db"))
	st, err := store.Open(at)
	if err != nil {
		fatalf(t, "store.Open: %v", err)
	}
	defer st.Close()

	k := sessions.NewKeeper(st)
	// The rest a session takes between two of its requests. It decides how many
	// sessions a speed needs — at a minute and a half, a thousand pages a minute
	// wants about sixteen hundred of them — so it is a knob of its own.
	want := sessions.Want{Device: blanktrail.DeviceDesktop,
		Pause: time.Duration(envInt("GSERP_PACE_REST", int(sessions.DefaultRest/time.Second))) * time.Second,
		UpTo:  time.Duration(envInt("GSERP_PACE_REST_UPTO", int(sessions.DefaultRestUpTo/time.Second))) * time.Second}
	logf(t, "MEASUREMENT a session rests %v to %v between two of its requests", want.Pause, want.UpTo)
	spec := blanktrail.DefaultPortSpec()
	if envInt("GSERP_PACE_HOP", 1) != 0 && onGateways == "" {
		spec.FirstHop = liveFirstHop(t)
	}
	switch {
	case spec.FirstHop.Gateway != "":
		logf(t, "MEASUREMENT through a first hop: the gateway %s", spec.FirstHop.Gateway)
	case spec.FirstHop.Proxy != "":
		logf(t, "MEASUREMENT through a first hop: a SOCKS5 proxy")
	default:
		logf(t, "MEASUREMENT straight to the addresses, no first hop")
	}

	perUpstream := envInt("GSERP_PER_UPSTREAM", 1)
	ban := time.Duration(envInt("GSERP_BAN_MS", 0)) * time.Millisecond
	spec.Protocol = envOr("GSERP_PROTOCOL", spec.Protocol)
	// The service's own browser is what solves Google's check, and whether it
	// follows the port's first hop is the question this switch asks: on a list
	// reachable only through the hop, a solver that goes round it can never
	// reach anything, and every search waits for it and comes back empty.
	if envInt("GSERP_PACE_SOLVER", 1) == 0 {
		spec.JSSolver = false
		logf(t, "MEASUREMENT the service's JavaScript solver is switched off on these ports")
	}
	// How many connections one port may hold at once. The service counts the
	// connections its solver's browser opens through the port against the same
	// ceiling, so this is also how hard one challenge may hit one exit: a solve
	// opens tens of connections within seconds, and a pool that falls in the
	// first minute of every run — when every fresh session meets its first
	// challenge — is a pool that may be answering exactly that.
	if n := envInt("GSERP_PACE_MAX_CONCURRENT", 0); n > 0 {
		spec.MaxConcurrent = n
		logf(t, "MEASUREMENT a port holds at most %d connections at once, its solver's included", n)
	}
	// Where the port resolves names. The service's own answer on a chained port
	// is to resolve through the exit, and with UDP unavailable it does so over
	// TCP — a round trip through the whole chain before the request itself.
	// How long the service is given to reach the address. It makes three
	// attempts of this each, so what is set here is a third of the patience a
	// request has for a slow exit.
	if secs := envInt("GSERP_PACE_CONNECT", 0); secs > 0 {
		spec.ConnectTimeoutSeconds = secs
		logf(t, "MEASUREMENT the service is given %ds to reach an address, three attempts of it", secs)
	}
	if mode := envOr("GSERP_PACE_VDNS", ""); mode != "" {
		spec.VDNSMode = mode
		logf(t, "MEASUREMENT names are resolved with vdns_mode=%q", mode)
	}
	// The two answers a profile carries about exits that terminate TLS and
	// about who resolves a name. Left off, this measures a road the job does
	// not take: seven addresses in ten on this list answer a port that refuses
	// such exits with 526.
	spec.AllowMITMUpstream = envInt("GSERP_PACE_MITM", 1) != 0
	spec.Resolver = envOr("GSERP_PACE_RESOLVER", "")
	spec.VDNSStrictBypass = envInt("GSERP_PACE_STRICT", 0) != 0
	logf(t, "MEASUREMENT ports allow exits terminating TLS: %v; names resolved by %q, never handed to the proxy: %v",
		spec.AllowMITMUpstream, spec.Resolver, spec.VDNSStrictBypass)

	// The run is stopped by the clock rather than by the work: what is wanted is
	// a reading of a run at its steady speed, not a run to the end of a list.
	runCtx, stop := context.WithTimeout(ctx, paceFor)
	defer stop()

	// most is how many requests the run may put through its ports in all, for a
	// list paid for by what goes through it. The run is stopped once that many
	// less one per thread have gone out, so the ones still in flight end inside
	// the figure rather than past it.
	most := envInt("GSERP_PACE_MOST", 0)
	if most > 0 {
		logf(t, "MEASUREMENT the run stops before %d requests have gone through its ports", most)
	}
	var capped sync.Once
	// The list a job reads is read again on the profile's interval, and what a
	// reading does to the sessions going out through the list is part of the
	// speed: a reading that carried them off their addresses cost each one a
	// challenge. A static list never shows it, so a run may read its list again
	// the way a job does.
	rotor := blanktrail.NewStaticRotor(ups, blanktrail.WithRest(ban))
	if every := time.Duration(envInt("GSERP_PACE_REFRESH", 0)) * time.Second; every > 0 &&
		envOr("GSERP_PACE_LIST_FILE", "") == "" {
		if rotor, err = blanktrail.NewRotor(ctx, blanktrail.Source{Kind: "url", Location: listURL,
			Refresh: every, DefaultScheme: "socks5"}, blanktrail.WithRest(ban)); err != nil {
			fatalf(t, "reading the list to read it again: %v", err)
		}
		defer rotor.Close()
		logf(t, "MEASUREMENT the list is read again every %v, as a job reads it", every)
	}
	channel := blanktrail.NewListChannel("list", rotor)
	if onGateways != "" {
		names := gatewaysNamed(ctx, t, client, onGateways)
		logf(t, "MEASUREMENT on %d of the service's gateways, %.1f threads to a gateway: %s",
			len(names), float64(paceThreads)/float64(len(names)), strings.Join(names, ", "))
		channel = blanktrail.NewGatewayListChannel("gateways",
			blanktrail.NewStaticRotor(blanktrail.GatewayUpstreams(names), blanktrail.WithRest(ban)))
	}
	attempts := &attemptTally{}
	exits := &exitTally{}
	p, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: paceThreads, PortsPerThread: 1, Spec: spec, CA: pre.CA,
		Channels: []blanktrail.Channel{channel},
		Sessions: true, Choose: k.Choose, MaxPerUpstream: perUpstream,
		ReviveAfter: time.Minute, WaitForIdentity: true, NoKeepAlives: true,
		OnLease: exits.leased,
		Trace: func(tr blanktrail.RequestTrace) {
			exits.note(tr)
			if n := attempts.note(tr); most > 0 && n >= most-paceThreads {
				capped.Do(func() {
					logf(t, "MEASUREMENT %d requests have gone out: the run stops here", n)
					stop()
				})
			}
		},
	})
	if err != nil {
		fatalf(t, "opening the pool: %v", err)
	}
	defer p.Close()

	// What the ports actually came out as. The profile says what they should
	// carry and the pool says what it asked for; neither is the same question
	// as what the service opened, and on a list reachable only through a first
	// hop a port that came out without one is a port that carries nothing.
	all, withChain, withSolver, err := portsAsOpened(ctx)
	if err != nil {
		logf(t, "MEASUREMENT the ports could not be read back: %v", err)
	} else {
		logf(t, "MEASUREMENT the service holds %d ports: %d carry a first hop, %d carry the solver "+
			"(this run asked for a hop on %v and the solver on %v)",
			all, withChain, withSolver, !spec.FirstHop.IsZero(), spec.JSSolver)
	}

	census := NewWhere()
	counting := NewChallenges()
	widening := NewRamp(want.Longest())
	asks := &tookTally{}
	refusals := &asksTally{}

	qs := make([]google.Query, len(pacePhrases))
	for i, ph := range pacePhrases {
		qs[i] = google.Query{Text: ph, Country: "ru", Language: "ru"}
	}
	// A run at a thousand pages a minute holds a session per query in flight,
	// and a hundred and forty phrases cap it at a hundred and forty sessions —
	// about a hundred pages a minute at the rest above, whatever else is true.
	// So a measurement of speed takes its phrases from a real job's list.
	if job := envInt("GSERP_PACE_JOB", 0); job > 0 {
		qs = phrasesOf(t, job, envInt("GSERP_PACE_PHRASES", 3000))
		logf(t, "MEASUREMENT %d phrases taken from job %d", len(qs), job)
	}

	// What the service's solver had done before the run, so what it did for the
	// run can be read as the difference.
	solvedBefore, solverErr := solverStats(ctx)
	if solverErr != nil {
		logf(t, "MEASUREMENT the solver's counts could not be read: %v", solverErr)
	}

	pages := 0
	var mu sync.Mutex
	began := time.Now()

	// A reading a minute while it runs. A run that is widening and a run at its
	// own speed are different runs, and one total over both says neither.
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		tick := time.NewTicker(paceEvery)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				mu.Lock()
				so := pages
				mu.Unlock()
				all, held := k.Count()
				logf(t, "MEASUREMENT +%v: %d pages so far (%.1f a minute over the last stretch), "+
					"%d sessions known, %d in hand | %s",
					time.Since(began).Round(time.Second), so,
					float64(so)/time.Since(began).Minutes(), all, held,
					census.Reading().line())
			}
		}
	}()
	rep := (&Runner{Pool: p, Threads: paceThreads, Keeper: k, Want: want,
		Challenges: counting, Ramp: widening, Where: census,
		Watch: func(s Step) {
			if s.Stage != StageAsk {
				return
			}
			asks.note(s.Took, s.Err == nil)
			if s.Err != nil {
				refusals.note(s.Err)
			}
		}}).Run(runCtx, Job{Queries: qs, Pages: pacePages, Tries: paceTries,
		Captured: func(google.SERP) {
			mu.Lock()
			pages++
			mu.Unlock()
		}})
	took := time.Since(began)
	<-watching

	reading := census.Reading()
	all, held := k.Count()
	logf(t, "MEASUREMENT %d threads, %v: %d pages (%.1f a minute), %d queries settled, "+
		"%d sessions known and %d in hand",
		paceThreads, took.Round(time.Second), pages, float64(pages)/took.Minutes(),
		rep.Done+rep.Failed, all, held)
	logf(t, "MEASUREMENT a thread's time, by place:")
	for _, s := range reading.Standing {
		logf(t, "MEASUREMENT   %-28s %5.1f%% of the time, %d standing there now, longest %v",
			string(s.Doing), 100*reading.Share(s.Doing), s.Threads, s.Longest.Round(time.Second))
	}
	logf(t, "MEASUREMENT asks: %s", asks.reading())
	logf(t, "MEASUREMENT attempts inside them: %s", attempts.reading())
	logf(t, "MEASUREMENT refusals Google judged: %s", refusals.tally())
	rhythm := counting.Rhythm()
	logf(t, "MEASUREMENT checks met %d, requests between two checks of a session %.1f (over %d stretches)",
		rhythm.Met, rhythm.Between, rhythm.Intervals)
	if solverErr == nil {
		if after, err := solverStats(ctx); err == nil {
			logf(t, "MEASUREMENT the solver, for this run: %s", after.since(solvedBefore))
		}
	}
	for _, line := range exits.reading() {
		logf(t, "MEASUREMENT   %s", line)
	}

	if pages == 0 {
		fatalf(t, "nothing came back at all, so there is no speed to take apart")
	}
}

// tookTally is how long the asks took, kept whole so the shape can be read
// rather than an average that hides it.
type tookTally struct {
	mu   sync.Mutex
	ok   []time.Duration
	fail []time.Duration
}

func (a *tookTally) note(took time.Duration, answered bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if answered {
		a.ok = append(a.ok, took)
		return
	}
	a.fail = append(a.fail, took)
}

func (a *tookTally) reading() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return fmt.Sprintf("%d answered %s; %d refused %s",
		len(a.ok), spread(a.ok), len(a.fail), spread(a.fail))
}

// spread is the middle and the tail of a set of durations. A mean alone cannot
// tell a run where every request is slow from one where one request in ten
// hangs for minutes, and those two want different things done about them.
func spread(ds []time.Duration) string {
	if len(ds) == 0 {
		return "(none)"
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) time.Duration {
		i := int(q * float64(len(s)-1))
		return s[i].Round(time.Millisecond)
	}
	var total time.Duration
	for _, d := range s {
		total += d
	}
	return fmt.Sprintf("(median %v, 90th %v, longest %v, mean %v)",
		at(0.5), at(0.9), s[len(s)-1].Round(time.Millisecond),
		(total / time.Duration(len(s))).Round(time.Millisecond))
}

// attemptTally is what the requests were made of underneath: how many attempts
// each took and where the time inside one went.
type attemptTally struct {
	mu sync.Mutex
	// why counts the shapes of the failures: what a request that brought back
	// nothing actually did. A count of "never arrived" says how many, and this
	// says what happened to them, which is the difference between a road that
	// refuses and a road that goes quiet.
	why map[string]int
	// reasons counts the word the service put on each answer it composed
	// itself, and the ones it put no word on. The last is what says whether
	// there is a kind of refusal neither side has a name for.
	reasons  map[string]int
	n        int
	retries  int
	reused   int
	failed   int
	statuses map[int]int
	total    []time.Duration
	connect  []time.Duration
	first    []time.Duration
}

// note counts one attempt and says how many there have been, this one among
// them.
func (a *attemptTally) note(tr blanktrail.RequestTrace) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.statuses == nil {
		a.statuses = map[int]int{}
	}
	a.n++
	if tr.Attempt > 0 {
		a.retries++
	}
	if tr.Reused {
		a.reused++
	}
	if tr.Err != nil {
		a.failed++
		if a.why == nil {
			a.why = map[string]int{}
		}
		a.why[whyFailed(tr.Err)]++
	}
	a.statuses[tr.Status]++
	if a.reasons == nil {
		a.reasons = map[string]int{}
	}
	switch {
	case tr.Reason != "":
		a.reasons[tr.Reason]++
	case tr.Err != nil:
		a.reasons["(no tag: nothing came back)"]++
	case tr.Status/100 != 2 && tr.Status/100 != 3:
		a.reasons[fmt.Sprintf("(no tag: HTTP %d from the far end)", tr.Status)]++
	}
	a.total = append(a.total, tr.Total)
	if tr.Connect > 0 {
		a.connect = append(a.connect, tr.Connect)
	}
	if tr.FirstByte > 0 {
		a.first = append(a.first, tr.FirstByte)
	}
	return a.n
}

func (a *attemptTally) reading() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	kinds := make([]int, 0, len(a.statuses))
	for s := range a.statuses {
		kinds = append(kinds, s)
	}
	sort.Slice(kinds, func(i, j int) bool { return a.statuses[kinds[i]] > a.statuses[kinds[j]] })
	var says []string
	for _, s := range kinds {
		says = append(says, fmt.Sprintf("%d×%d", a.statuses[s], s))
	}
	shapes := make([]string, 0, len(a.why))
	for w := range a.why {
		shapes = append(shapes, w)
	}
	sort.Slice(shapes, func(i, j int) bool { return a.why[shapes[i]] > a.why[shapes[j]] })
	var why []string
	for i, w := range shapes {
		if i >= 5 {
			break
		}
		why = append(why, fmt.Sprintf("%d×%s", a.why[w], w))
	}
	tags := make([]string, 0, len(a.reasons))
	for r := range a.reasons {
		tags = append(tags, r)
	}
	sort.Slice(tags, func(i, j int) bool { return a.reasons[tags[i]] > a.reasons[tags[j]] })
	var named []string
	for _, r := range tags {
		named = append(named, fmt.Sprintf("%d×%s", a.reasons[r], r))
	}
	return fmt.Sprintf("%d attempts, %d of them retries, %d on a kept connection, %d never arrived; "+
		"statuses %v; what the service called them: %v; what the failures were: %v; "+
		"whole attempt %s; tunnel %s; first byte %s",
		a.n, a.retries, a.reused, a.failed, says, named, why,
		spread(a.total), spread(a.connect), spread(a.first))
}

// asksTally is the refusals Google judged, by kind. It is the tally from the
// session tests under a name of its own so both can live in one package.
type asksTally = asks

// envInt is a number the environment named, or the fallback.
func envInt(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// envOr is a string the environment named, or the fallback.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// portsAsOpened asks the service what its ports actually came out as. It is a
// request of its own rather than a client method because it is a question the
// program does not ask anywhere yet, and the answer is the point: a port opened
// without the first hop its profile names carries nothing on a list that can
// only be reached through one.
func portsAsOpened(ctx context.Context) (all, withChain, withSolver int, err error) {
	base := strings.TrimRight(os.Getenv("BLANKTRAIL_URL"), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/ports", nil)
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("X-API-Key", os.Getenv("BLANKTRAIL_API_KEY"))
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Ports []struct {
			ChainProxy   string `json:"chain_proxy"`
			ChainGateway string `json:"chain_gateway"`
			JSSolver     bool   `json:"js_solver"`
		} `json:"ports"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return 0, 0, 0, err
	}
	for _, one := range doc.Ports {
		all++
		if one.ChainProxy != "" || one.ChainGateway != "" {
			withChain++
		}
		if one.JSSolver {
			withSolver++
		}
	}
	return all, withChain, withSolver, nil
}

// whyFailed names the shape of a failure without repeating the address it
// failed on: a request through a proxy quotes both ends of the connection, and
// one of them is a secret.
func whyFailed(err error) string {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "forcibly closed"), strings.Contains(text, "connection reset"):
		return "the connection was closed with nothing on it"
	case strings.Contains(text, "context deadline exceeded"), strings.Contains(text, "timeout"):
		return "nothing came back before the deadline"
	case strings.Contains(text, "eof"):
		return "the connection ended"
	case strings.Contains(text, "refused"):
		return "the connection was refused"
	case strings.Contains(text, "tls"), strings.Contains(text, "handshake"):
		return "the handshake did not finish"
	default:
		// The words of an unknown failure, with anything that looks like an
		// address taken out.
		return hostless(text)
	}
}

// hostless takes anything shaped like an address out of a line.
var addressLike = regexp.MustCompile(`[0-9a-z.\-]+:[0-9]+`)

func hostless(s string) string {
	return addressLike.ReplaceAllString(s, "«address»")
}

// gatewaysNamed is the gateways a run goes out through: every one the service
// holds, or the ones named, in the order given. A name the service does not
// hold stops the run, since a run on fewer gateways than it says measures
// something else.
func gatewaysNamed(ctx context.Context, t *testing.T, client *blanktrail.Client, named string) []string {
	t.Helper()
	list, err := client.Gateways(ctx)
	if err != nil {
		fatalf(t, "asking the service for its gateways: %v", err)
	}
	if !list.Available {
		t.Skipf("the service cannot raise a tunnel through a gateway: %s", list.Reason)
	}
	held := map[string]bool{}
	var all []string
	for _, g := range list.Gateways {
		held[g.Name] = true
		all = append(all, g.Name)
	}
	if named == "all" {
		return all
	}
	var out []string
	for _, name := range strings.Split(named, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !held[name] {
			fatalf(t, "the service holds no gateway named %q", name)
		}
		out = append(out, name)
	}
	return out
}

// exitTally is what each gateway carried: the requests that went out through
// it, how they came back, and how long they took. It is what says how many
// threads one exit bears before Google turns on it — an exit that is being
// turned on answers more and more of its requests slowly, with a challenge
// solved, and then not at all.
type exitTally struct {
	mu sync.Mutex
	// through is the gateway each port went out through at its last lease.
	through map[int]string
	by      map[string]*exitCounts
}

type exitCounts struct {
	quick, solved, other, failed int
	took                         []time.Duration
}

// waitedAnswer is where an answer stops being a plain one: a search that met no
// challenge comes back in a few seconds, and one that met a challenge waited
// for it to be solved, which takes tens.
const waitedAnswer = 12 * time.Second

func (e *exitTally) leased(l blanktrail.LeaseTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.through == nil {
		e.through = map[int]string{}
	}
	e.through[l.Port] = l.Gateway
}

func (e *exitTally) note(tr blanktrail.RequestTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	gw := e.through[tr.Port]
	if gw == "" {
		return
	}
	if e.by == nil {
		e.by = map[string]*exitCounts{}
	}
	c := e.by[gw]
	if c == nil {
		c = &exitCounts{}
		e.by[gw] = c
	}
	switch {
	case tr.Err != nil:
		c.failed++
	case tr.Status == http.StatusOK && tr.Total < waitedAnswer:
		c.quick++
	case tr.Status == http.StatusOK:
		c.solved++
	default:
		c.other++
	}
	c.took = append(c.took, tr.Total)
}

// reading is one line per gateway, the busiest first.
func (e *exitTally) reading() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(e.by))
	for name := range e.by {
		names = append(names, name)
	}
	total := func(c *exitCounts) int { return c.quick + c.solved + c.other + c.failed }
	sort.Slice(names, func(i, j int) bool { return total(e.by[names[i]]) > total(e.by[names[j]]) })
	var out []string
	for _, name := range names {
		c := e.by[name]
		out = append(out, fmt.Sprintf("%-40s %4d requests: %4d answered quickly, %3d after a wait (a challenge), "+
			"%3d other statuses, %3d never arrived %s", name, total(c), c.quick, c.solved, c.other, c.failed, spread(c.took)))
	}
	return out
}

// paceAddresses is the list the run goes out through: the one the program
// reads, or a file named by GSERP_PACE_LIST_FILE — a list put together for one
// measurement, such as the sticky exits of a residential provider, one line per
// exit. Every address in such a file is kept out of the log whole and in its
// parts, because there the login and the password are the account.
func paceAddresses(ctx context.Context, t *testing.T, listURL string) []blanktrail.Upstream {
	t.Helper()
	path := envOr("GSERP_PACE_LIST_FILE", "")
	if path == "" {
		return addressList(ctx, t, listURL)
	}
	ups, bad, err := blanktrail.Source{Kind: "file", Location: path, DefaultScheme: "socks5"}.Load(ctx)
	for _, u := range ups {
		keepOut(u.URL(), u.User, u.Pass, u.Host)
	}
	if err != nil {
		fatalf(t, "reading the list file: %v", err)
	}
	logf(t, "MEASUREMENT list: %d addresses from a file, %d lines unusable", len(ups), len(bad))
	return ups
}

// solverCounts is what the service's challenge solver has done since it
// started: every attempt, the ones solved, by the kind of challenge, and why
// the rest failed.
type solverCounts struct {
	Attempts int `json:"total_attempts"`
	Solved   int `json:"total_solved"`
	Families []struct {
		Family   string `json:"family"`
		Attempts int    `json:"attempts"`
		Solved   int    `json:"solved"`
	} `json:"families"`
	FailReasons []struct {
		Reason string `json:"reason"`
		Count  int    `json:"count"`
	} `json:"fail_reasons"`
}

// solverStats reads the solver's counts, the way portsAsOpened reads the ports.
func solverStats(ctx context.Context) (solverCounts, error) {
	base := strings.TrimRight(os.Getenv("BLANKTRAIL_URL"), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/solver/stats", nil)
	if err != nil {
		return solverCounts{}, err
	}
	req.Header.Set("X-API-Key", os.Getenv("BLANKTRAIL_API_KEY"))
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return solverCounts{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out solverCounts
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return solverCounts{}, err
	}
	return out, nil
}

// since is what the solver did between two readings. The service is shared, so
// on a machine where something else is running too this is an upper bound.
func (c solverCounts) since(before solverCounts) string {
	families := map[string][2]int{}
	for _, f := range before.Families {
		families[f.Family] = [2]int{-f.Attempts, -f.Solved}
	}
	for _, f := range c.Families {
		was := families[f.Family]
		families[f.Family] = [2]int{was[0] + f.Attempts, was[1] + f.Solved}
	}
	var kinds []string
	for name, n := range families {
		if n[0] != 0 {
			kinds = append(kinds, fmt.Sprintf("%s %d of %d", name, n[1], n[0]))
		}
	}
	sort.Strings(kinds)
	reasons := map[string]int{}
	for _, r := range before.FailReasons {
		reasons[r.Reason] -= r.Count
	}
	for _, r := range c.FailReasons {
		reasons[r.Reason] += r.Count
	}
	var why []string
	for r, n := range reasons {
		if n != 0 {
			why = append(why, fmt.Sprintf("%d×%s", n, r))
		}
	}
	sort.Strings(why)
	return fmt.Sprintf("%d challenges attempted, %d solved; by kind %v; failures %v",
		c.Attempts-before.Attempts, c.Solved-before.Solved, kinds, why)
}

// phrasesOf reads the phrases of a job from the database the running program
// keeps, without touching it: read-only, and only the text, which is all a
// measurement of speed needs of them.
func phrasesOf(t *testing.T, job, most int) []google.Query {
	t.Helper()
	path := envOr("GSERP_DB", "")
	if path == "" {
		t.Skip("GSERP_DB is not set, so there is no job to take phrases from")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(8000)")
	if err != nil {
		t.Fatalf("opening the job's database: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT text FROM queries WHERE job_id = ? ORDER BY ordinal LIMIT ?`, job, most)
	if err != nil {
		t.Fatalf("reading the job's phrases: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []google.Query
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatalf("reading a phrase: %v", err)
		}
		out = append(out, google.Query{Text: text})
	}
	if len(out) == 0 {
		t.Skipf("job %d has no phrases", job)
	}
	return out
}

// line is the census on one line, for a log that prints one a minute: every
// place that has anybody in it, with how many and how long the longest of them
// has been there.
func (c Census) line() string {
	if len(c.Standing) == 0 {
		return "nothing standing anywhere yet"
	}
	var says []string
	for _, s := range c.Standing {
		if s.Threads == 0 {
			continue
		}
		says = append(says, fmt.Sprintf("%s %d (longest %v, %.0f%% of the time)",
			string(s.Doing), s.Threads, s.Longest.Round(time.Second), 100*c.Share(s.Doing)))
	}
	if len(says) == 0 {
		return "no thread is standing anywhere"
	}
	return strings.Join(says, "; ")
}
