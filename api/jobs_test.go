// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/store"
	"github.com/blanktrail/google-serp-parser/web"
)

// The queue the browser puts jobs through has to fit the one this interface
// puts jobs through, or the two are two queues wearing one name.
var (
	_ Supervisor = (*web.Supervisor)(nil)
	_ Supervisor = (*queueStub)(nil)
)

// formFixture is the query list the form's own test is written against, kept
// character for character. The same list sent as JSON has to come out as the
// same two queries: a list that means one thing typed into the box and another
// posted by a program is a list nobody can check.
const formFixture = "  \n# note\niphone 13\n\n golang \n"

// queueStub stands in for the queue. It writes a job down the way the real one
// does, so what the handler reads back afterwards is a job that exists, and it
// hands back whichever refusal a test has put in front of it.
//
// Nothing in it is guarded, because a test drives it from the one goroutine.
type queueStub struct {
	st *store.Store

	// What was asked of it.
	specs   []store.JobSpec
	lists   [][]string
	stopped []int64
	resumed []int64

	// What it refuses with, and where it says the jobs are.
	enqueueErr error
	stopErr    error
	resumeErr  error
	running    int64
	waiting    []int64
	// idle is a queue with nothing to run on: it takes jobs and starts none.
	idle bool
}

func (q *queueStub) CanRun() bool { return !q.idle }

func (q *queueStub) Enqueue(spec store.JobSpec, queries []string) (int64, error) {
	q.specs = append(q.specs, spec)
	q.lists = append(q.lists, queries)
	if q.enqueueErr != nil {
		return 0, q.enqueueErr
	}
	id, err := q.st.CreateJob(context.Background(), spec, queries)
	if err != nil {
		return 0, err
	}
	q.waiting = append(q.waiting, id)
	return id, nil
}

func (q *queueStub) Stop(jobID int64) error {
	q.stopped = append(q.stopped, jobID)
	return q.stopErr
}

func (q *queueStub) Resume(jobID int64) error {
	q.resumed = append(q.resumed, jobID)
	return q.resumeErr
}

func (q *queueStub) Running() (int64, bool) { return q.running, q.running != 0 }

func (q *queueStub) Queued() []int64 { return q.waiting }

// jobServer builds a server with a queue behind it and a key that works.
func jobServer(t *testing.T) (*Server, *store.Store, *queueStub, string) {
	t.Helper()
	st := testStore(t)
	q := &queueStub{st: st}
	s, err := New(Config{Store: st, Logger: quiet(), Supervisor: q})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, st, q, issue(t, st, "nightly")
}

// call drives the server's own routes, so a handler nobody registered fails
// here rather than passing on a direct call.
func call(t *testing.T, s *Server, secret, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var send io.Reader
	if body != "" {
		send = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, send)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// jobBody is a job as the wire carries it. The names are written out here so
// that renaming a field in the answer fails a test rather than somebody's
// program.
type jobBody struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Pages    int    `json:"pages"`
	Country  string `json:"country"`
	Language string `json:"language"`
	Total    int    `json:"total"`
	Done     int    `json:"done"`
	Pending  int    `json:"pending"`
	Finished bool   `json:"finished"`
	Running  bool   `json:"running"`
	Queued   bool   `json:"queued"`
}

type createdBody struct {
	Job      jobBody        `json:"job"`
	Estimate map[string]any `json:"estimate"`
}

type listBody struct {
	Jobs []jobBody `json:"jobs"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("the answer is not the JSON it claims to be: %v (body %q)", err, rec.Body.String())
	}
}

// refusalOf reads the sentence a refusal carries, insisting it is the one shape
// this interface refuses in.
func refusalOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	decode(t, rec, &body)
	msg, named := body["error"].(string)
	if !named {
		t.Fatalf("the refusal carries no error field: %v", body)
	}
	return msg
}

// createBody is a creation request built from a query list.
func createBody(t *testing.T, name string, queries []string, pages int) string {
	t.Helper()
	list, err := json.Marshal(queries)
	if err != nil {
		t.Fatalf("marshalling the query list: %v", err)
	}
	return `{"name":` + strconv.Quote(name) +
		`,"queries":` + string(list) +
		`,"pages":` + strconv.Itoa(pages) + `}`
}

// jobIn writes a job straight into the history, for the jobs a test needs to
// exist without having been asked for through the interface.
func jobIn(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	id, err := st.CreateJob(context.Background(),
		store.JobSpec{Name: name, Pages: 1}, []string{"iphone 13"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return id
}

func TestCreatingAJob_PutsItThroughTheQueueTheBrowserUses(t *testing.T) {
	// Two ways in and one queue. A job written down beside the queue is a job
	// that sits in the history and never runs, and nothing about the answer to
	// the request that made it would say so.
	s, _, q, secret := jobServer(t)

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs",
		createBody(t, "nightly", []string{"iphone 13", "golang"}, 2))

	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a job gave %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var got createdBody
	decode(t, rec, &got)
	if got.Job.ID == 0 {
		t.Error("the answer carries no job id, so nothing can be asked about it afterwards")
	}
	if len(q.specs) != 1 {
		t.Fatalf("the queue was handed %d jobs, want the one that was asked for", len(q.specs))
	}
	if q.specs[0].Name != "nightly" || q.specs[0].Pages != 2 {
		t.Errorf("the queue was handed %+v, want the name and depth that were asked for", q.specs[0])
	}
	if !got.Job.Queued {
		t.Error("the answer says the job is not waiting its turn, though it was just queued")
	}
}

func TestCreatingAJob_DropsBlankLinesAndNotesTheWayTheFormDoes(t *testing.T) {
	// The same fixture the form's test uses. Two paths reading one list by two
	// rules go wrong quietly: the job runs, it is short, and nobody looks until
	// a customer counts the rows.
	s, st, q, secret := jobServer(t)

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs",
		createBody(t, "nightly", strings.Split(formFixture, "\n"), 1))

	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a job gave %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if len(q.lists) != 1 {
		t.Fatalf("the queue was handed %d lists, want one", len(q.lists))
	}
	want := []string{"iphone 13", "golang"}
	got := q.lists[0]
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("the queue was handed %q, want %q", got, want)
	}

	var body createdBody
	decode(t, rec, &body)
	sum, err := st.Progress(context.Background(), body.Job.ID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Total != 2 {
		t.Errorf("the history holds %d queries for the job, want the two real ones", sum.Total)
	}
}

func TestCreatingAJob_AnswersWithWhatTheJobWillCost(t *testing.T) {
	// A program sizing a run needs the estimate as much as a person does, and it
	// needs it before the run rather than an hour into it.
	s, _, _, secret := jobServer(t)
	// More queries than there are ports, so the pacing floor is a length rather
	// than nothing: a fixture that costs zero cannot tell a floor that was worked
	// out from one that was never filled in.
	list := make([]string, 13)
	for i := range list {
		list[i] = "query " + strconv.Itoa(i)
	}

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs", createBody(t, "nightly", list, 2))

	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a job gave %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var got createdBody
	decode(t, rec, &got)
	if got.Estimate == nil {
		t.Fatal("the answer carries no estimate")
	}
	// Read off the wire rather than through a Go type, because the names are the
	// part somebody else's program depends on.
	for _, want := range []struct {
		field string
		value float64
	}{
		{"queries", 13},
		{"pages", 2},
		{"searches", 26},
		// The six ports the form and the command both start from. The threads
		// passed where the ports go quotes a job nobody asked for.
		{"ports", 6},
		// Thirteen queries over six ports puts three on the busiest one, which
		// waits out two gaps of the two seconds the pool keeps between requests.
		{"floor_seconds", 4},
	} {
		if got.Estimate[want.field] != want.value {
			t.Errorf("the estimate says %s=%v, want %v", want.field, got.Estimate[want.field], want.value)
		}
	}
	expected, ok := got.Estimate["expected_seconds"].(float64)
	if !ok || expected <= 0 {
		t.Errorf("the estimate expects %v seconds, want a length somebody can plan around",
			got.Estimate["expected_seconds"])
	}
	if floor := got.Estimate["floor_seconds"].(float64); floor > expected {
		t.Errorf("the estimate has a floor of %v seconds above an expectation of %v", floor, expected)
	}
}

func TestCreatingAJob_RefusesABodyThatIsNotJSONWithSomethingToActOn(t *testing.T) {
	// A program that sent the wrong thing has to learn what the right thing is
	// from the answer, because there is nobody at the other end to read a log.
	s, _, q, secret := jobServer(t)

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs", "name=nightly&queries=iphone")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body that is not JSON gave %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	msg := refusalOf(t, rec)
	for _, word := range []string{"JSON", "queries"} {
		if !strings.Contains(msg, word) {
			t.Errorf("the refusal %q does not say what to send instead: it never mentions %q", msg, word)
		}
	}
	if len(q.specs) != 0 {
		t.Error("a body nobody could read still put a job in the queue")
	}
}

func TestCreatingAJob_RefusesAListThatHoldsNoQueries(t *testing.T) {
	// A list of nothing but notes is a mistake, and a job of no queries is one
	// the history refuses anyway. Saying so here names the fault.
	s, _, q, secret := jobServer(t)

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs",
		createBody(t, "nightly", []string{"  ", "# a note", ""}, 1))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a list holding no queries gave %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if len(q.specs) != 0 {
		t.Error("a list holding no queries still put a job in the queue")
	}
}

func TestCreatingAJob_SaysSoWhenThereIsNoQueueBehindTheServer(t *testing.T) {
	// A history opened on another machine has no queue behind it. Answering that
	// with a fault would send whoever deployed it after a broken database.
	st := testStore(t)
	s, err := New(Config{Store: st, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	secret := issue(t, st, "nightly")

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs",
		createBody(t, "nightly", []string{"iphone 13"}, 1))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a server with no queue gave %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestCreatingAJob_SaysTheQueueIsClosedRatherThanBroken(t *testing.T) {
	// A server winding down will take work again once it is back up. A caller
	// told the server is broken goes looking for a fault that is not there.
	s, _, q, secret := jobServer(t)
	q.enqueueErr = web.ErrClosed

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs",
		createBody(t, "nightly", []string{"iphone 13"}, 1))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("creating a job while the queue is closed gave %d, want 503 (body %q)",
			rec.Code, rec.Body.String())
	}
}

func TestJobAddresses_AreBehindTheKeyCheck(t *testing.T) {
	// Every one of them, and not merely the ones somebody remembered. A single
	// address left open is the whole check gone.
	s, st, q, _ := jobServer(t)
	id := jobIn(t, st, "nightly")
	at := strconv.FormatInt(id, 10)

	for _, req := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/jobs", createBody(t, "nightly", []string{"iphone 13"}, 1)},
		{http.MethodGet, "/api/v1/jobs", ""},
		{http.MethodGet, "/api/v1/jobs/" + at, ""},
		{http.MethodPost, "/api/v1/jobs/" + at + "/stop", ""},
		{http.MethodPost, "/api/v1/jobs/" + at + "/resume", ""},
	} {
		rec := call(t, s, "", req.method, req.target, req.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d without a key, want 401", req.method, req.target, rec.Code)
		}
	}
	if len(q.specs) != 0 || len(q.stopped) != 0 || len(q.resumed) != 0 {
		t.Error("a request with no key still reached the queue")
	}
}

func TestListingJobs_AnswersWithEveryJobTheHistoryHolds(t *testing.T) {
	// Two of them, because a listing of one cannot tell having listed the right
	// job from having listed everything.
	s, st, _, secret := jobServer(t)
	first := jobIn(t, st, "monday")
	second := jobIn(t, st, "tuesday")

	rec := call(t, s, secret, http.MethodGet, "/api/v1/jobs", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("listing gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got listBody
	decode(t, rec, &got)
	if len(got.Jobs) != 2 {
		t.Fatalf("the listing holds %d jobs, want the two in the history", len(got.Jobs))
	}
	// Newest first, which is what the history hands back and what a reader
	// paging through a night's runs expects.
	if got.Jobs[0].ID != second || got.Jobs[1].ID != first {
		t.Errorf("the listing runs %d then %d, want %d then %d",
			got.Jobs[0].ID, got.Jobs[1].ID, second, first)
	}
	if got.Jobs[0].Name != "tuesday" || got.Jobs[0].Total != 1 {
		t.Errorf("the newest job reads %+v, want the name and count the history holds", got.Jobs[0])
	}
}

func TestListingJobs_TakesTheNewestFewWhenANumberIsNamed(t *testing.T) {
	s, st, _, secret := jobServer(t)
	jobIn(t, st, "monday")
	second := jobIn(t, st, "tuesday")

	rec := call(t, s, secret, http.MethodGet, "/api/v1/jobs?limit=1", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("listing gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got listBody
	decode(t, rec, &got)
	if len(got.Jobs) != 1 || got.Jobs[0].ID != second {
		t.Errorf("a listing limited to one holds %+v, want only the newest job", got.Jobs)
	}
}

func TestListingJobs_RefusesANumberItCannotRead(t *testing.T) {
	// Quietly handing back fifty jobs to somebody who asked for a number this
	// program could not read is an answer they will believe.
	s, _, _, secret := jobServer(t)

	rec := call(t, s, secret, http.MethodGet, "/api/v1/jobs?limit=lots", "")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("a limit that is not a number gave %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestOneJob_AnswersWithThatJobAndNoOther(t *testing.T) {
	// Answering the whole listing to somebody who named one job hands them a
	// shape their program will not read, and the job they asked about is in
	// there somewhere.
	s, st, q, secret := jobServer(t)
	wanted := jobIn(t, st, "monday")
	jobIn(t, st, "tuesday")
	q.running = wanted

	rec := call(t, s, secret, http.MethodGet, "/api/v1/jobs/"+strconv.FormatInt(wanted, 10), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("asking after one job gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got jobBody
	decode(t, rec, &got)
	if got.ID != wanted || got.Name != "monday" {
		t.Errorf("the answer is job %d named %q, want %d named monday", got.ID, got.Name, wanted)
	}
	if got.Total != 1 || got.Pending != 1 {
		t.Errorf("the answer counts %d queries and %d pending, want one of each", got.Total, got.Pending)
	}
	if !got.Running {
		t.Error("the answer does not say the job is running, though the queue has it in flight")
	}
}

func TestOneJob_SaysThereIsNoSuchJobWhileOthersExist(t *testing.T) {
	// On a history that holds jobs, so that a handler answering 404 because it
	// could read nothing at all is not mistaken for one that looked.
	s, st, _, secret := jobServer(t)
	jobIn(t, st, "monday")
	jobIn(t, st, "tuesday")

	rec := call(t, s, secret, http.MethodGet, "/api/v1/jobs/9999", "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("a job nobody stored gave %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	refusalOf(t, rec)
}

func TestStoppingAJob_RefusesWhenAnotherJobIsTheOneRunning(t *testing.T) {
	// The history holds a job that is running, so the refusal is about which job
	// was named and not about there being nothing to stop at all.
	s, st, q, secret := jobServer(t)
	inFlight := jobIn(t, st, "monday")
	idle := jobIn(t, st, "tuesday")
	q.running = inFlight
	q.stopErr = web.ErrNotRunning

	rec := call(t, s, secret, http.MethodPost,
		"/api/v1/jobs/"+strconv.FormatInt(idle, 10)+"/stop", "")

	if rec.Code != http.StatusConflict {
		t.Fatalf("stopping a job that is not running gave %d, want 409 (body %q)",
			rec.Code, rec.Body.String())
	}
	refusalOf(t, rec)
}

func TestStoppingAJob_SaysThereIsNoSuchJobWhileOthersExist(t *testing.T) {
	s, st, q, secret := jobServer(t)
	jobIn(t, st, "monday")
	jobIn(t, st, "tuesday")

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs/9999/stop", "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("stopping a job nobody stored gave %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if len(q.stopped) != 0 {
		t.Error("the queue was told to stop a job that does not exist")
	}
}

func TestStoppingAJob_TakesTheJobFromTheAddressAndNotFromTheBody(t *testing.T) {
	// The address names the job. A body that names another one is either a
	// mistake or somebody reaching for a job that is not theirs, and either way
	// the address is what was asked for.
	s, st, q, secret := jobServer(t)
	named := jobIn(t, st, "monday")
	other := jobIn(t, st, "tuesday")
	q.running = named

	rec := call(t, s, secret, http.MethodPost,
		"/api/v1/jobs/"+strconv.FormatInt(named, 10)+"/stop",
		`{"id":`+strconv.FormatInt(other, 10)+`}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("stopping the job in flight gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(q.stopped) != 1 || q.stopped[0] != named {
		t.Errorf("the queue was told to stop %v, want only job %d", q.stopped, named)
	}
}

func TestStoppingAJob_LetsThroughAStopOfTheJobInFlight(t *testing.T) {
	s, st, q, secret := jobServer(t)
	id := jobIn(t, st, "monday")
	q.running = id

	rec := call(t, s, secret, http.MethodPost,
		"/api/v1/jobs/"+strconv.FormatInt(id, 10)+"/stop", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("stopping the job in flight gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got jobBody
	decode(t, rec, &got)
	if got.ID != id {
		t.Errorf("the answer is about job %d, want %d", got.ID, id)
	}
	if len(q.stopped) != 1 || q.stopped[0] != id {
		t.Errorf("the queue was told to stop %v, want job %d", q.stopped, id)
	}
}

func TestCarryingOnAJob_RefusesOneWithNothingLeft(t *testing.T) {
	// Nothing left to do is a fact about the job, not a fault in the server, and
	// a program told 500 will retry until somebody notices.
	s, st, q, secret := jobServer(t)
	jobIn(t, st, "monday")
	done := jobIn(t, st, "tuesday")
	q.resumeErr = web.ErrNothingLeft

	rec := call(t, s, secret, http.MethodPost,
		"/api/v1/jobs/"+strconv.FormatInt(done, 10)+"/resume", "")

	if rec.Code != http.StatusConflict {
		t.Fatalf("carrying on a job with nothing left gave %d, want 409 (body %q)",
			rec.Code, rec.Body.String())
	}
	refusalOf(t, rec)
}

func TestCarryingOnAJob_SaysTheQueueIsClosedRatherThanBroken(t *testing.T) {
	// A server on its way down will take work again when it comes back, which is
	// a different thing to tell a caller than that something went wrong.
	s, st, q, secret := jobServer(t)
	id := jobIn(t, st, "monday")
	q.resumeErr = web.ErrClosed

	rec := call(t, s, secret, http.MethodPost,
		"/api/v1/jobs/"+strconv.FormatInt(id, 10)+"/resume", "")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("carrying on a job while the queue is closed gave %d, want 503 (body %q)",
			rec.Code, rec.Body.String())
	}
}

func TestCarryingOnAJob_PutsItBackInTheQueue(t *testing.T) {
	s, st, q, secret := jobServer(t)
	id := jobIn(t, st, "monday")

	rec := call(t, s, secret, http.MethodPost,
		"/api/v1/jobs/"+strconv.FormatInt(id, 10)+"/resume", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("carrying on a job gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(q.resumed) != 1 || q.resumed[0] != id {
		t.Errorf("the queue was told to carry on %v, want job %d", q.resumed, id)
	}
}

func TestCreateJob_RefusesUntilThereIsSomethingToRunItOn(t *testing.T) {
	// The browser refuses a job on a machine whose connection is not set up and
	// says what to do about it. A program asking the same machine for the same
	// thing has to be told the same, or it is handed an id and left watching a
	// job that never moves.
	s, _, queue, secret := jobServer(t)
	queue.idle = true

	rec := call(t, s, secret, http.MethodPost, "/api/v1/jobs",
		createBody(t, "a job", []string{"one"}, 1))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a job on a server with nothing to run it came back %d, want 503: %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(refusalOf(t, rec), "nothing to run a job on") {
		t.Errorf("the refusal does not say what is missing: %q", refusalOf(t, rec))
	}
	if len(queue.specs) != 0 {
		t.Errorf("the job was written down anyway: %v", queue.specs)
	}
}
