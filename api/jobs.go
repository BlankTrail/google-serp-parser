// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
	"github.com/blanktrail/google-serp-parser/web"
)

// Supervisor is the queue a job is set running through.
//
// It is named by what it does rather than by its type so that a test can hold a
// job half run without opening anything, which is the only way to ask what a
// stop of the wrong job answers. The queue the browser uses satisfies it, and
// that is the point: a job set up by a program and a job set up by hand wait in
// one line. Two queues would each run a job at a time and the machine would run
// two, which is neither what was measured nor what was asked for.
type Supervisor interface {
	// Enqueue writes a job down and puts it at the back of the queue.
	Enqueue(spec store.JobSpec, queries []string) (int64, error)
	// Resume puts a job that was left part way back in the queue.
	Resume(jobID int64) error
	// Stop ends the job that is running.
	Stop(jobID int64) error
	// Running is the job in flight and whether there is one.
	Running() (int64, bool)
	// Queued is the jobs waiting their turn.
	Queued() []int64
}

// startingPorts and startingThreads are what a job costs by when the request
// names neither.
//
// They are the numbers the form starts from and the numbers the command starts
// from, so the same job quoted through this interface and quoted in the browser
// comes to the same figure. A caller who runs a pool of another size says so and
// is answered for the pool they have.
const (
	startingPorts   = 6
	startingThreads = 2
)

// noteMark begins a line that is a note rather than a query. It is the same mark
// the form and the command pass over, because the list somebody pastes here is
// the list they keep in a file.
const noteMark = "#"

// createJSON is a job as a program asks for one. The fields are the form's
// fields under the names a program would guess.
type createJSON struct {
	Name     string   `json:"name"`
	Queries  []string `json:"queries"`
	Pages    int      `json:"pages"`
	Country  string   `json:"country"`
	Language string   `json:"language"`
	Spec     string   `json:"spec"`
	// Ports and Threads shape the estimate and nothing else. They describe the
	// pool the caller runs, which this interface does not decide for them.
	Ports   int `json:"ports"`
	Threads int `json:"threads"`
}

// jobJSON is one job and how far it has got.
type jobJSON struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Finished   bool       `json:"finished"`

	Pages    int    `json:"pages"`
	Country  string `json:"country"`
	Language string `json:"language"`
	Spec     string `json:"spec"`

	Total   int `json:"total"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Pending int `json:"pending"`

	Running bool `json:"running"`
	Queued  bool `json:"queued"`
}

// estimateJSON is what a job will cost, in the units a program works in.
//
// The two lengths are seconds rather than the rounded words the page shows: a
// caller deciding whether to wait or to poll has to compare them with something,
// and a number compares.
type estimateJSON struct {
	Queries     int     `json:"queries"`
	Pages       int     `json:"pages"`
	Searches    int     `json:"searches"`
	Requests    int     `json:"requests"`
	MaxRequests int     `json:"max_requests"`
	Ports       int     `json:"ports"`
	Expected    float64 `json:"expected_seconds"`
	Floor       float64 `json:"floor_seconds"`
}

// createdJSON is the answer to a creation: the job, and what it will cost.
type createdJSON struct {
	Job      jobJSON      `json:"job"`
	Estimate estimateJSON `json:"estimate"`
}

// listJSON is a listing. It is an object around the list rather than the bare
// list, so a later answer can carry something beside it without every reader
// having to be rewritten.
type listJSON struct {
	Jobs []jobJSON `json:"jobs"`
}

// jobRoutes registers what this interface serves about jobs. Every one of them
// is behind the key check; an address that is not is the check gone.
func (s *Server) jobRoutes() {
	s.mux.HandleFunc("POST /api/v1/jobs", s.authed(s.createJob))
	s.mux.HandleFunc("GET /api/v1/jobs", s.authed(s.listJobs))
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.authed(s.showJob))
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/stop", s.authed(s.stopJob))
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/resume", s.authed(s.resumeJob))
}

// createJob writes a job down, queues it, and says what it will cost.
//
// The queueing is the queue the browser uses and never a write of its own. A job
// written straight into the history is a job that will sit there unrun, and
// nothing about the answer to the request that made it would say so.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var asked createJSON
	if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
		writeError(w, http.StatusBadRequest,
			"the request body is not JSON: send an object with a name and a list of queries")
		return
	}
	queries := realQueries(asked.Queries)
	switch {
	case strings.TrimSpace(asked.Name) == "":
		writeError(w, http.StatusBadRequest, "the job needs a name to be found by afterwards")
		return
	case len(queries) == 0:
		writeError(w, http.StatusBadRequest,
			"the request names no queries: blank lines and lines beginning with a hash are notes")
		return
	case asked.Pages < 0:
		writeError(w, http.StatusBadRequest, "a job is taken to at least one page of results")
		return
	}
	if s.sup == nil {
		writeError(w, http.StatusServiceUnavailable,
			"this server was started without a queue and can set nothing running")
		return
	}

	id, err := s.sup.Enqueue(asked.spec(), queries)
	if err != nil {
		s.refuse(w, r, "a job could not be queued", err)
		return
	}
	sum, ok := s.jobRead(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, createdJSON{
		Job:      s.view(sum),
		Estimate: asked.estimate(queries),
	})
}

// listJobs answers with the history, newest first.
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if named := r.URL.Query().Get("limit"); named != "" {
		n, err := strconv.Atoi(named)
		if err != nil {
			// Handing back the usual fifty would be an answer the caller believes
			// and cannot tell from the number they meant to ask for.
			writeError(w, http.StatusBadRequest, "limit must be a number")
			return
		}
		limit = n
	}
	jobs, err := s.store.Jobs(r.Context(), limit)
	if err != nil {
		s.log.Error("the jobs could not be listed", "error", err)
		writeError(w, http.StatusInternalServerError, "the server could not read its own records")
		return
	}
	// An empty history is an empty list and never nothing at all: a caller
	// walking the answer should not have to tell the two apart.
	out := listJSON{Jobs: make([]jobJSON, 0, len(jobs))}
	for _, sum := range jobs {
		out.Jobs = append(out.Jobs, s.view(sum))
	}
	writeJSON(w, http.StatusOK, out)
}

// showJob answers with one job and how far it has got.
func (s *Server) showJob(w http.ResponseWriter, r *http.Request) {
	sum, ok := s.jobAsked(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.view(sum))
}

// stopJob ends the job that is running.
func (s *Server) stopJob(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, "a job could not be stopped", Supervisor.Stop)
}

// resumeJob takes up a job that was left part way.
func (s *Server) resumeJob(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, "a job could not be taken up again", Supervisor.Resume)
}

// act does one thing to the job the address names and answers with the job as
// it stands afterwards.
//
// The job is looked up before the queue is told anything, so an address naming a
// job nobody ever stored is answered with there being no such job rather than
// with whatever the queue makes of a number it has never seen.
func (s *Server) act(w http.ResponseWriter, r *http.Request, doing string, do func(Supervisor, int64) error) {
	sum, ok := s.jobAsked(w, r)
	if !ok {
		return
	}
	if s.sup == nil {
		writeError(w, http.StatusServiceUnavailable,
			"this server was started without a queue and can set nothing running")
		return
	}
	if err := do(s.sup, sum.ID); err != nil {
		s.refuse(w, r, doing, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(sum))
}

// refuse answers a refusal from the queue with the code it deserves.
//
// The code is what somebody else's program decides by. A job already running, a
// job with nothing left and a queue that has shut down are three different
// things to do next — wait, stop asking, come back later — and answering all
// three with a fault turns every one of them into a retry loop against a server
// that has nothing wrong with it.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, doing string, err error) {
	switch {
	case errors.Is(err, web.ErrBusy):
		writeError(w, http.StatusConflict, "this job is already running or waiting its turn")
	case errors.Is(err, web.ErrNotRunning):
		writeError(w, http.StatusConflict, "this job is not the one running")
	case errors.Is(err, web.ErrNothingLeft):
		writeError(w, http.StatusConflict, "this job has nothing left to do")
	case errors.Is(err, web.ErrClosed):
		writeError(w, http.StatusServiceUnavailable,
			"this server is shutting down and is taking no further work")
	case errors.Is(err, store.ErrNoJob):
		writeError(w, http.StatusNotFound, "no such job")
	default:
		s.log.Error(doing, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "the server could not carry that out")
	}
}

// jobAsked reads the job the address names.
//
// The number comes from the address and from nowhere else. A body that names
// another job is either a mistake or somebody reaching past the address they
// were given, and in both cases the address is what was asked for.
func (s *Server) jobAsked(w http.ResponseWriter, r *http.Request) (store.JobSummary, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such job")
		return store.JobSummary{}, false
	}
	return s.jobRead(w, r, id)
}

// jobRead reads one job, answering for it when it cannot.
func (s *Server) jobRead(w http.ResponseWriter, r *http.Request, id int64) (store.JobSummary, bool) {
	sum, err := s.store.Progress(r.Context(), id)
	if errors.Is(err, store.ErrNoJob) {
		writeError(w, http.StatusNotFound, "no such job")
		return store.JobSummary{}, false
	}
	if err != nil {
		s.log.Error("a job could not be read", "job", id, "error", err)
		writeError(w, http.StatusInternalServerError, "the server could not read its own records")
		return store.JobSummary{}, false
	}
	return sum, true
}

// view is one job as this interface hands it over.
func (s *Server) view(sum store.JobSummary) jobJSON {
	running, queued := s.holds(sum.ID)
	j := jobJSON{
		ID:        sum.ID,
		Name:      sum.Name,
		CreatedAt: sum.CreatedAt,
		Finished:  sum.Finished,
		Pages:     sum.Pages,
		Country:   sum.Country,
		Language:  sum.Language,
		Spec:      sum.SpecName,
		Total:     sum.Total,
		Done:      sum.Done,
		Failed:    sum.Failed,
		Pending:   sum.Pending,
		Running:   running,
		Queued:    queued,
	}
	if sum.Finished {
		at := sum.FinishedAt
		j.FinishedAt = &at
	}
	return j
}

// holds says where the queue has this job: in flight, or waiting its turn.
//
// The queue is read before the job in flight. A job leaves the queue by
// starting, so this order can at worst report a job that has just started as
// waiting, and a caller told to wait asks again. The other order reports a job
// that started between the two reads as neither, and a caller told that stops
// watching a job that has just begun.
func (s *Server) holds(jobID int64) (running, queued bool) {
	if s.sup == nil {
		return false, false
	}
	for _, waiting := range s.sup.Queued() {
		if waiting == jobID {
			queued = true
			break
		}
	}
	if now, ok := s.sup.Running(); ok && now == jobID {
		running = true
	}
	return running, queued
}

// realQueries is the list with the notes taken out.
//
// It is the rule the form applies to the box and the rule the command applies to
// a file: a line that is blank or begins with a hash is not a search. The same
// list has to mean the same thing sent all three ways, or a job asked for by
// program is quietly shorter than the one somebody checked in the browser.
func realQueries(lines []string) []string {
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, noteMark) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// spec is the job as the history will file it.
func (c createJSON) spec() store.JobSpec {
	return store.JobSpec{
		Name:     strings.TrimSpace(c.Name),
		Pages:    c.pages(),
		Country:  c.Country,
		Language: c.Language,
		SpecName: c.Spec,
	}
}

// pages is the depth, which is one when nobody named one. A body that leaves the
// depth out has asked for the shallowest job there is rather than for no job.
func (c createJSON) pages() int {
	if c.Pages < 1 {
		return 1
	}
	return c.Pages
}

// estimate works out what the job will cost, by the same arithmetic and against
// the same measured pace as the figure the form shows.
//
// The threads are named where the threads go. They are the lanes the work is
// shared between, and passing the ports in their place quotes a job nobody asked
// for.
func (c createJSON) estimate(queries []string) estimateJSON {
	ports, threads := c.Ports, c.Threads
	if ports < 1 {
		ports = startingPorts
	}
	if threads < 1 {
		threads = startingThreads
	}
	j := run.Job{Pages: c.pages(), SpecName: c.Spec}
	for _, text := range queries {
		j.Queries = append(j.Queries,
			google.Query{Text: text, Country: c.Country, Language: c.Language})
	}
	est := run.EstimateWith(j, ports, threads, blanktrail.DefaultCooldown, run.MeasuredPace)
	return estimateJSON{
		Queries:     est.Queries,
		Pages:       est.Pages,
		Searches:    est.Searches,
		Requests:    est.Requests,
		MaxRequests: est.MaxRequests,
		Ports:       est.Ports,
		Expected:    est.Expected.Seconds(),
		Floor:       est.Floor.Seconds(),
	}
}

// writeJSON answers with a value.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The header is already out, so a write that fails here leaves nothing to
	// tell the caller.
	_ = json.NewEncoder(w).Encode(v)
}
