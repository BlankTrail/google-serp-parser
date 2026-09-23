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
	ups := addressList(ctx, t, listURL)
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
	st, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		fatalf(t, "store.Open: %v", err)
	}
	defer st.Close()

	k := sessions.NewKeeper(st)
	want := sessions.Want{Device: blanktrail.DeviceDesktop,
		Pause: sessions.DefaultRest, UpTo: sessions.DefaultRestUpTo}
	spec := blanktrail.DefaultPortSpec()
	if envInt("GSERP_PACE_HOP", 1) != 0 {
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

	attempts := &attemptTally{}
	p, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: paceThreads, PortsPerThread: 1, Spec: spec, CA: pre.CA,
		Channels: []blanktrail.Channel{blanktrail.NewListChannel("list",
			blanktrail.NewStaticRotor(ups, blanktrail.WithRest(ban)))},
		Sessions: true, Choose: k.Choose, MaxPerUpstream: perUpstream,
		ReviveAfter: time.Minute, WaitForIdentity: true, NoKeepAlives: true,
		Trace: attempts.note,
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

	// The run is stopped by the clock rather than by the work: what is wanted is
	// a reading of a run at its steady speed, not a run to the end of a list.
	runCtx, stop := context.WithTimeout(ctx, paceFor)
	defer stop()

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
	logf(t, "MEASUREMENT checks met %d, requests between them %.1f (over %d counted)",
		rhythm.Met, rhythm.Between, rhythm.Asked)

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

func (a *attemptTally) note(tr blanktrail.RequestTrace) {
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
