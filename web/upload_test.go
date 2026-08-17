// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/store"
)

// linesOverABatch is a fixture larger than one batch of the plan behind it.
//
// It is that size deliberately. A batch boundary is where an order is lost, a
// count goes wrong and a commit is forgotten, and a fixture that never reaches
// one is a fixture on which every one of those breakages is harmless.
const linesOverABatch = 2500

// testServerHolding is a server that can start a job and will never get through
// one: the engine waits before every query, so a list that has just been
// uploaded is still there to be read exactly as it was written.
func testServerHolding(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	v := newSupervisor(st, &heldEngine{hold: make(chan struct{})})
	t.Cleanup(func() { _ = v.Close() })

	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// uploadBody builds the form exactly as the page builds it: the boxes first and
// the file last, because that order is what lets the server know the name and
// the depth before the first line arrives.
func uploadBody(t *testing.T, boxes map[string]string, list string) (string, *bytes.Buffer) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, box := range []string{"name", "kind", "target", "from", "pages", "country", "language", "spec", "unique", "threads", "ports", "tries", "queries"} {
		value, filled := boxes[box]
		if !filled {
			continue
		}
		if err := form.WriteField(box, value); err != nil {
			t.Fatalf("writing the %s box: %v", box, err)
		}
	}
	part, err := form.CreateFormFile(listField, "queries.txt")
	if err != nil {
		t.Fatalf("opening the file part: %v", err)
	}
	if _, err := io.WriteString(part, list); err != nil {
		t.Fatalf("writing the list: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("closing the form: %v", err)
	}
	return form.FormDataContentType(), &body
}

// postUpload sends a whole file the way a browser does.
func postUpload(t *testing.T, s *Server, boxes map[string]string, list string) *httptest.ResponseRecorder {
	t.Helper()
	kind, body := uploadBody(t, boxes, list)
	req := httptest.NewRequest(http.MethodPost, uploadAt, body)
	req.Header.Set("Content-Type", kind)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// stoppedShort is a body that stops the way a browser that went away stops: it
// hands over what had arrived and then refuses to say any more.
func stoppedShort(head string) io.Reader {
	return io.MultiReader(strings.NewReader(head), refusing{})
}

type refusing struct{}

func (refusing) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// numbered is a list of n lines, each naming its own place, so a shuffle or a
// dropped line is visible rather than merely a wrong total.
func numbered(n int) string {
	var list strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&list, "q%05d\n", i)
	}
	return list.String()
}

// halfUploaded writes the job a browser that went away leaves behind: a list
// that stopped part way, with the batches that were committed still in it.
//
// The queries matter. A job of no queries at all is refused by everything for a
// second reason — there is nothing left to run — and a fixture like that would
// let a page offering to carry the job on look exactly like a page that refuses
// to, which is the one thing this is used to tell apart.
func halfUploaded(t *testing.T, s *Server, name string) *store.Plan {
	t.Helper()
	p, err := s.store.OpenPlan(t.Context(), store.JobSpec{Name: name, Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	for i := 0; i < linesOverABatch; i++ {
		if err := p.Add(t.Context(), fmt.Sprintf("q%05d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := p.Abandon(); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	sum, err := s.store.Progress(t.Context(), p.JobID())
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Pending == 0 {
		t.Fatal("the fixture left a job with nothing waiting, which every refusal would refuse anyway")
	}
	return p
}

// theOneJob is the single job in the history, and a failure when there is any
// other number of them.
func theOneJob(t *testing.T, s *Server) store.JobSummary {
	t.Helper()
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs in the history, want exactly one", len(jobs))
	}
	return jobs[0]
}

func TestUpload_WritesAListLargerThanOneBatchAndStartsIt(t *testing.T) {
	// The whole feature in one pass: a file goes in, a job comes out with every
	// line of it in the order it arrived, marked complete, and queued.
	s := testServerHolding(t)
	rec := postUpload(t, s, map[string]string{
		"name": "nightly", "pages": "2", "country": "de", "language": "de",
	}, numbered(linesOverABatch))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading gave %d, want a redirect:\n%s", rec.Code, rec.Body.String())
	}
	job := theOneJob(t, s)
	if !job.PlanReady {
		t.Error("the job was left saying its list never finished arriving")
	}
	if job.Name != "nightly" || job.Pages != 2 || job.Country != "de" || job.Language != "de" {
		t.Errorf("the job was filed as %+v, want the boxes that stood before the file", job)
	}
	if loc := rec.Header().Get("Location"); loc != jobPath(job.ID) {
		t.Errorf("Location=%q, want the job's own page", loc)
	}

	left, err := s.store.Pending(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != linesOverABatch {
		t.Fatalf("%d queries in the plan, want %d", len(left), linesOverABatch)
	}
	for i, q := range left {
		if q.Ordinal != i || q.Text != fmt.Sprintf("q%05d", i) {
			t.Fatalf("query %d is %+v — the order did not survive the batches", i, q)
		}
	}
	// The job was handed to the supervisor rather than merely written down. Both
	// answers are read together because a job leaves the queue by starting, and a
	// job that has just started is not a job nobody took.
	if running, queued := s.holds(job.ID); !running && !queued {
		t.Error("the uploaded job is neither running nor waiting its turn")
	}
}

func TestUpload_ReadsAFileByTheSameRuleTheBoxIsReadBy(t *testing.T) {
	// Two rules for reading a list are two answers to "how many queries have I
	// got", and nothing on any screen would say which of them a job ran. So the
	// file is read through the same rule the box is, and the proof is that the
	// same awkward text comes out of both the same way.
	const list = "  \n# a note\niphone 13\n\n golang generics \n#\r\n\r\n\tlast one\t\n"

	s := testServerHolding(t)
	if rec := postUpload(t, s, map[string]string{"name": "same", "pages": "1"}, list); rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading gave %d, want a redirect:\n%s", rec.Code, rec.Body.String())
	}
	byTheBox, complaints := jobForm{Name: "same", Pages: 1, Queries: list}.parse()
	if len(complaints) != 0 {
		t.Fatalf("the box refused the same text: %v", complaints)
	}

	job := theOneJob(t, s)
	left, err := s.store.Pending(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	byTheFile := make([]string, len(left))
	for i, q := range left {
		byTheFile[i] = q.Text
	}
	if len(byTheFile) != len(byTheBox) {
		t.Fatalf("the file gave %q and the box gave %q", byTheFile, byTheBox)
	}
	for i := range byTheBox {
		if byTheFile[i] != byTheBox[i] {
			t.Errorf("query %d is %q from the file and %q from the box", i, byTheFile[i], byTheBox[i])
		}
	}
}

func TestUpload_KeepsALineListedTwiceAsTwoPiecesOfWork(t *testing.T) {
	// A repeat is not a mistake to be tidied away. Somebody who listed a phrase
	// twice gets it searched for twice, exactly as the box does it, and a job
	// that silently ran fewer queries than the file holds would have nothing on
	// any screen saying so.
	s := testServerHolding(t)
	if rec := postUpload(t, s, map[string]string{"name": "twice", "pages": "1"},
		"iphone 13\ngolang generics\niphone 13\n"); rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading gave %d, want a redirect", rec.Code)
	}
	job := theOneJob(t, s)
	left, err := s.store.Pending(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != 3 {
		t.Fatalf("%d queries were kept, want the three lines that were sent", len(left))
	}
	if left[0].Text != left[2].Text {
		t.Errorf("the repeated line came back as %q and %q", left[0].Text, left[2].Text)
	}
}

func TestUpload_AListThatStoppedArrivingLeavesAJobNobodyCanRun(t *testing.T) {
	// The decision this pins. A browser that went away halfway through a file
	// leaves the job in the history, marked as a list that never finished
	// arriving: nothing will run it and nothing will carry it on, and it is there
	// to be seen, because twenty minutes of uploading that ends in an empty
	// history is twenty minutes with nothing to ask about.
	s := testServerHolding(t)
	kind, whole := uploadBody(t, map[string]string{"name": "cut off", "pages": "1"},
		numbered(3*linesOverABatch))
	// Cut in the middle of the file and past the first batch, so what is proved
	// is that the batches already committed stay and the one in hand does not.
	full := whole.String()
	stop := strings.Index(full, "q01500")
	if stop < 0 {
		t.Fatal("the fixture does not hold the line it is cut at")
	}
	req := httptest.NewRequest(http.MethodPost, uploadAt, stoppedShort(full[:stop]))
	req.Header.Set("Content-Type", kind)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusSeeOther {
		t.Fatal("a list that stopped arriving was accepted as a job")
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("form.upload.broke")) {
		t.Errorf("the reader was not told the file stopped arriving:\n%s", rec.Body.String())
	}

	job := theOneJob(t, s)
	if job.PlanReady {
		t.Error("a list that stopped arriving was marked complete")
	}
	// The batch that had been committed is there and the one in hand is not.
	if job.Total != 1000 {
		t.Errorf("the job holds %d queries, want the one batch that was committed", job.Total)
	}
	if _, err := s.store.Pending(t.Context(), job.ID); !errors.Is(err, store.ErrPlanUnfinished) {
		t.Errorf("Pending returned %v, want ErrPlanUnfinished", err)
	}
	if _, err := s.store.LastUnfinished(t.Context(), "cut off"); !errors.Is(err, store.ErrNoUnfinishedJob) {
		t.Errorf("the half-uploaded job was offered to be carried on: %v", err)
	}
	if running, queued := s.holds(job.ID); running || queued {
		t.Error("a job whose list never finished arriving was queued")
	}
}

func TestUpload_SaysSoWhenTheFormStoppedBeforeTheFileEvenBegan(t *testing.T) {
	// The other end of the same failure: the connection went away while the boxes
	// were still arriving, so there is no file part and no job. The reader is told
	// the same thing, because from where they stand it is the same thing.
	s := testServerHolding(t)
	kind, whole := uploadBody(t, map[string]string{"name": "cut off", "pages": "1"}, numbered(10))
	full := whole.String()
	stop := strings.Index(full, "pages")
	if stop < 0 {
		t.Fatal("the fixture does not hold the box it is cut at")
	}
	req := httptest.NewRequest(http.MethodPost, uploadAt, stoppedShort(full[:stop]))
	req.Header.Set("Content-Type", kind)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), LangEN.T("form.upload.broke")) {
		t.Errorf("a form that stopped before its file said nothing about it:\n%s", rec.Body.String())
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("a form that never reached its file left %d jobs behind", len(jobs))
	}
}

func TestUpload_TakesALineFarLongerThanTheDefaultAndRefusesOneLongerStill(t *testing.T) {
	// Lines over the reader's own default of 64 KB are not rare in somebody
	// else's list, so the reader is given room; a cap there still has to be, or a
	// file with no newline in it is a file held whole.
	s := testServerHolding(t)
	long := strings.Repeat("a", 100<<10)
	if rec := postUpload(t, s, map[string]string{"name": "long", "pages": "1"},
		long+"\niphone 13\n"); rec.Code != http.StatusSeeOther {
		t.Fatalf("a line of 100 KB was refused: %d\n%s", rec.Code, rec.Body.String())
	}
	job := theOneJob(t, s)
	left, err := s.store.Pending(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != 2 {
		t.Fatalf("%d queries came back, want the long line and the short one", len(left))
	}
	if left[0].Text != long {
		t.Errorf("the long line came back %d characters long, want %d", len(left[0].Text), len(long))
	}

	// And one over the cap is refused, saying so, leaving a job that will not run.
	other := testServerHolding(t)
	rec := postUpload(t, other, map[string]string{"name": "longer", "pages": "1"},
		strings.Repeat("b", lineCap+10)+"\n")
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a line longer than this program reads was accepted")
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("form.upload.longline")) {
		t.Errorf("the reader was not told which fault this was:\n%s", rec.Body.String())
	}
	if theOneJob(t, other).PlanReady {
		t.Error("a list this program could not read was marked complete")
	}
}

func TestUpload_SaysWhatIsWrongWithTheBoxesAndWritesNoJob(t *testing.T) {
	// The boxes stand before the file so this can happen before a million lines
	// are written into a job nobody asked for. A job written first and refused
	// afterwards is a job in the history for every mistyped form.
	s := testServerHolding(t)
	rec := postUpload(t, s, map[string]string{"name": "  ", "pages": "0"},
		numbered(linesOverABatch))

	if rec.Code == http.StatusSeeOther {
		t.Fatal("a job with no name was accepted")
	}
	body := rec.Body.String()
	for _, key := range []string{"form.name.required", "form.pages.positive"} {
		if !strings.Contains(body, LangEN.T(key)) {
			t.Errorf("the page does not report %s:\n%s", key, body)
		}
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("a refused upload left %d jobs in the history", len(jobs))
	}
}

func TestUpload_RefusesAFileWithNotOneQueryInIt(t *testing.T) {
	// A file of blanks and notes is the same mistake as an empty box, and it is
	// said the same way. Marked complete it would be picked up, found to have
	// nothing left, and stamped done.
	s := testServerHolding(t)
	rec := postUpload(t, s, map[string]string{"name": "empty", "pages": "1"},
		"  \n# a note\n\n#another\n")

	if rec.Code == http.StatusSeeOther {
		t.Fatal("a file holding no query was accepted")
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("form.queries.required")) {
		t.Errorf("the reader was not told the list was empty:\n%s", rec.Body.String())
	}
	if theOneJob(t, s).PlanReady {
		t.Error("a job with no queries was marked ready to run")
	}
}

func TestUpload_RefusesAFileOnAServerWithNothingToRunIt(t *testing.T) {
	// A server started to read a history has nothing to run a job on, and
	// writing the file down first would spend the whole upload to arrive at a
	// sentence that could have been said before the first line.
	s := testServer(t)
	rec := postUpload(t, s, map[string]string{"name": "nightly", "pages": "1"},
		numbered(linesOverABatch))

	if rec.Code == http.StatusSeeOther {
		t.Fatal("a job was accepted by a server with nothing to run it")
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("form.norunner")) {
		t.Errorf("the reader was not told why nothing happened:\n%s", rec.Body.String())
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs were written down by a server that cannot run them", len(jobs))
	}
}

func TestUpload_SaysSoWhenNoFileCameWithTheForm(t *testing.T) {
	s := testServerHolding(t)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("name", "nightly"); err != nil {
		t.Fatalf("writing the name: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("closing the form: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, uploadAt, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), LangEN.T("form.upload.none")) {
		t.Errorf("a form with no file said nothing about it:\n%s", rec.Body.String())
	}
}

func TestUpload_SaysSoWhenTheFormWasNotSentAsAFileAtAll(t *testing.T) {
	// A plain form posted at this address is not a file, and answering with the
	// server's own failure would tell the reader their machine is broken.
	s := testServerHolding(t)
	req := httptest.NewRequest(http.MethodPost, uploadAt, strings.NewReader("name=nightly&pages=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a form sent the wrong way gave %d, want the page back", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("form.upload.unreadable")) {
		t.Errorf("the reader was not told what was wrong:\n%s", rec.Body.String())
	}
}

func TestNewJob_TakesAListAsAFileWithTheFileStandingLast(t *testing.T) {
	// The file has to be the last part a browser sends, because the server reads
	// the parts in that order and has to know the name and the depth before a
	// million lines start arriving. The order is the markup's, so it is read out
	// of the markup.
	body := get(t, testServer(t), "/new").Body.String()

	_, opened, ok := strings.Cut(body, `action="`+uploadAt+`"`)
	if !ok {
		t.Fatalf("the page offers no form that takes a file:\n%s", body)
	}
	form, _, ok := strings.Cut(opened, "</form>")
	if !ok {
		t.Fatalf("the form that takes a file is never closed:\n%s", body)
	}
	if !strings.Contains(form, `enctype="multipart/form-data"`) {
		t.Error("the form that takes a file does not say it is sending one")
	}
	file := strings.Index(form, `type="file"`)
	if file < 0 {
		t.Errorf("the form has no box to attach a file to:\n%s", form)
	}
	if !strings.Contains(form, `name="`+listField+`"`) {
		t.Errorf("the file box is not the one the server reads:\n%s", form)
	}
	for _, box := range []string{`name="name"`, `name="pages"`} {
		at := strings.Index(form, box)
		if at < 0 {
			t.Errorf("the form that takes a file has no %s", box)
			continue
		}
		if at > file {
			t.Errorf("%s stands after the file, so the server reads it after a million lines", box)
		}
	}
	// The box beside it is not sent as a file. A form the server has to read
	// whole is the one thing a very large list must not be sent through.
	plain, _, _ := strings.Cut(body, `action="`+uploadAt+`"`)
	if strings.Contains(plain, "multipart/form-data") {
		t.Error("the form holding the box sends itself as a file")
	}
}

func TestJobs_TellsAHalfUploadedListApartFromAnUnfinishedRun(t *testing.T) {
	// The two look identical in the counts: queries waiting, no stamp. One is a
	// run to be carried on and the other is a job that will never run at all, so
	// the list says which is which. Both are on the page at once, because a
	// listing that called every job by one name would satisfy a test that looked
	// for either name alone.
	s := testServer(t)
	stopped, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "stopped", Pages: 1},
		[]string{"iphone 13"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	p := halfUploaded(t, s, "cut off")

	if stopped == p.JobID() {
		t.Fatal("the fixture wrote one job where it meant two")
	}
	// Each phrase is read out of the row of the job it belongs to. A listing that
	// called every job by one name would satisfy a search of the whole page for
	// either name on its own.
	var seen int
	for _, row := range strings.Split(get(t, s, jobsAt).Body.String(), "<tr>") {
		switch {
		case strings.Contains(row, ">cut off<"):
			seen++
			if !strings.Contains(row, LangEN.T("job.state.listunfinished")) {
				t.Errorf("the half-uploaded job is not said to be one:\n%s", row)
			}
		case strings.Contains(row, ">stopped<"):
			seen++
			if !strings.Contains(row, LangEN.T("job.state.unfinished")) {
				t.Errorf("the run that was stopped is not said to be unfinished:\n%s", row)
			}
			if strings.Contains(row, LangEN.T("job.state.listunfinished")) {
				t.Errorf("a run that was stopped is called a half-uploaded list:\n%s", row)
			}
		}
	}
	if seen != 2 {
		t.Errorf("%d of the two jobs were drawn", seen)
	}
}

func TestJob_DoesNotOfferToCarryOnAListThatNeverFinishedArriving(t *testing.T) {
	// Its queries look exactly like work waiting to be done, and they are a
	// fraction of a list nobody knows the length of. A button that took the job
	// up would run that fraction and stamp the job complete.
	s := testServerHolding(t)
	p := halfUploaded(t, s, "cut off")

	body := get(t, s, jobPath(p.JobID())).Body.String()
	if !strings.Contains(body, LangEN.T("job.state.listunfinished")) {
		t.Errorf("the job's own page does not say why nothing is happening:\n%s", body)
	}
	if strings.Contains(body, `action="/api/resume"`) {
		t.Errorf("the page offers to carry on a job that cannot be carried on:\n%s", body)
	}
	// And the queue refuses it too, because a page is not a lock: whoever presses
	// that button is whoever posts to the address behind it. This is asked of the
	// queue outright rather than read off the queue after a post, because a job
	// taken up here is one whose plan cannot be read, so it would be given up
	// again within the instant and a look afterwards would find nothing either
	// way.
	if err := s.sup.Resume(p.JobID()); !errors.Is(err, store.ErrPlanUnfinished) {
		t.Errorf("the queue took the job and said %v, want ErrPlanUnfinished", err)
	}
	// The reader who posted it anyway is sent back to the page rather than shown
	// the server's own failure.
	rec := postForm(t, s, "/api/resume", url.Values{"job": {fmt.Sprint(p.JobID())}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("resuming gave %d, want the reader sent back to the page", rec.Code)
	}
}

func TestUpload_RefusesAFileBeforeTheConnectionHasBeenSetUp(t *testing.T) {
	// A machine whose connection has not been set up has a queue and nothing to
	// run a job through, which is a different thing from having no queue at all,
	// and the difference is the whole of what this asks. Refusing on one and
	// accepting on the other would spend the length of a very large upload to
	// arrive at a job that then sits in the queue with nothing to say why.
	st := testStore(t)
	v := NewSupervisorWithoutAPool(st)
	t.Cleanup(func() { _ = v.Close() })
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if v.canRun() {
		t.Fatal("the fixture has something to run a job on, so it asks nothing")
	}

	rec := postUpload(t, s, map[string]string{"name": "nightly", "pages": "1"},
		numbered(linesOverABatch))
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a file was accepted by a machine with nothing to run it through")
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("form.notsetup")) {
		t.Errorf("the reader was not told the connection is not set up:\n%s", rec.Body.String())
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("a refused upload left %d jobs in the history", len(jobs))
	}
}

func TestUpload_ShowsNoBareKeyWhereAPhraseBelongs(t *testing.T) {
	// A key on the page is a phrase that was never looked up, and it survives
	// every test written about a particular phrase. Each refusal this handler can
	// give is drawn, because between them they are what puts its phrases on a
	// page.
	for _, l := range Languages() {
		s := testServerHolding(t)
		lang := "?lang=" + string(l)
		kind, body := uploadBody(t, map[string]string{"name": "", "pages": "0"}, "iphone 13\n")
		req := httptest.NewRequest(http.MethodPost, uploadAt+lang, body)
		req.Header.Set("Content-Type", kind)
		refused := httptest.NewRecorder()
		s.Handler().ServeHTTP(refused, req)

		empty := postUpload(t, s, map[string]string{"name": "n", "pages": "1"}, "# nothing\n")
		bodies := []string{
			refused.Body.String(),
			empty.Body.String(),
			get(t, s, "/new"+lang).Body.String(),
		}
		for _, page := range bodies {
			for key := range catalogue[l] {
				if strings.Contains(page, key) {
					t.Errorf("the %s page shows the key %q where its text belongs", l, key)
				}
			}
		}
	}
}

func TestUpload_FilesTheJobUnderTheKindTheFormChose(t *testing.T) {
	// A list of addresses too long to type arrives by this door. A kind dropped
	// here would search a million addresses as phrases, and the reader would
	// find out from the results.
	s := testServerHolding(t)
	rec := postUpload(t, s, map[string]string{
		"name": "addresses", "kind": store.KindIndex, "pages": "1",
	}, "example.com/a\nexample.com/b\n")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the upload gave %d, want a redirect:\n%s", rec.Code, rec.Body.String())
	}
	if job := theOneJob(t, s); job.Kind != store.KindIndex {
		t.Errorf("the uploaded job was filed as kind %q, want %q", job.Kind, store.KindIndex)
	}
}

func TestUpload_RefusesAKindNothingAnswersToBeforeReadingTheFile(t *testing.T) {
	// Every fault that can be known before the file is read is said before it.
	// Writing a million lines into a job that names a question nobody answers is
	// minutes spent to arrive at a sentence that could have been said first.
	s := testServerHolding(t)
	rec := postUpload(t, s, map[string]string{
		"name": "odd", "kind": "images", "pages": "1",
	}, "example.com/a\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("an unknown kind gave %d, want the form back", rec.Code)
	}
	if want := LangEN.T("form.kind.unknown"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the page does not say %q:\n%s", want, rec.Body.String())
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs written for a kind nothing answers to, want none", len(jobs))
	}
}

func TestUpload_CarriesTheFilterFromTheBoxThatStoodBeforeTheFile(t *testing.T) {
	// The list that runs to ten million results is the one that arrives as a
	// file, so this is the door the filter is actually asked for at. The boxes
	// are read as they go by, and a box this one did not know would be read and
	// thrown away without a word.
	s := testServerHolding(t)
	rec := postUpload(t, s, map[string]string{
		"name": "nightly", "pages": "1", "unique": string(store.UniqueURL),
	}, "a\nb\n")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading gave %d, want a redirect:\n%s", rec.Code, rec.Body.String())
	}
	if job := theOneJob(t, s); job.UniqueBy != store.UniqueURL {
		t.Errorf("the uploaded job was filed as filtered by %q, want %q", job.UniqueBy, store.UniqueURL)
	}
}

func TestUpload_FilesTheSiteAPositionCheckWasGivenAndRefusesOneWithout(t *testing.T) {
	// The door a list of any size arrives by has to refuse what the other door
	// refuses. A check whose site was dropped here would be written down with
	// minutes of file behind it and nothing to look for, and it would answer "not
	// found" about every line of it.
	s := testServerHolding(t)
	ok := postUpload(t, s, map[string]string{
		"name": "places", "kind": store.KindPosition,
		"target": "example.com", "pages": "1",
	}, "iphone 13\npixel 8\n")
	if ok.Code != http.StatusSeeOther {
		t.Fatalf("the upload gave %d, want a redirect:\n%s", ok.Code, ok.Body.String())
	}
	if job := theOneJob(t, s); job.Kind != store.KindPosition || job.Target != "example.com" {
		t.Errorf("the uploaded job was filed as kind %q about %q, want %q about example.com",
			job.Kind, job.Target, store.KindPosition)
	}

	missing := postUpload(t, s, map[string]string{
		"name": "places", "kind": store.KindPosition, "pages": "1",
	}, "iphone 13\n")
	if missing.Code != http.StatusOK {
		t.Fatalf("a check with no site gave %d, want the form back", missing.Code)
	}
	if want := LangEN.T("form.target.required"); !strings.Contains(missing.Body.String(), want) {
		t.Errorf("the page does not say %q:\n%s", want, missing.Body.String())
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("%d jobs were written down, want only the one that was taken", len(jobs))
	}
}

func TestNewJob_ReadsTheBoxWhenTheSwitchSaysTheBoxAndTheFileWhenItSaysTheFile(t *testing.T) {
	// Both are on the page at once, because a box that appeared and disappeared
	// would need a script and this page has none. So something has to say which
	// was meant, and this is that something: with both filled in and only the
	// switch different, the job is made of one or the other and never of both.
	for _, c := range []struct {
		from    string
		want    []string
		notWant string
	}{
		{from: fromBox, want: []string{"typed one", "typed two"}, notWant: "from the file"},
		{from: fromFile, want: []string{"from the file"}, notWant: "typed one"},
	} {
		t.Run(c.from, func(t *testing.T) {
			s := testServerHolding(t)
			rec := postUpload(t, s, map[string]string{
				"name": "both filled in", "kind": store.KindParse, "pages": "1",
				"from": c.from, "queries": "typed one\ntyped two",
			}, "from the file")
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("the upload came back %d, want a redirect: %s", rec.Code, rec.Body.String())
			}
			job := theOneJob(t, s)
			left, err := s.store.Pending(t.Context(), job.ID)
			if err != nil {
				t.Fatalf("Pending: %v", err)
			}
			got := make([]string, len(left))
			for i, q := range left {
				got[i] = q.Text
			}
			if len(got) != len(c.want) {
				t.Fatalf("the job holds %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("the job holds %v, want %v", got, c.want)
					break
				}
			}
			for _, q := range got {
				if strings.Contains(q, c.notWant) {
					t.Errorf("the job holds %q, which came from the source the switch did not name", q)
				}
			}
		})
	}
}

func TestNewJob_ObeysTheSwitchWhateverOrderThePartsArriveIn(t *testing.T) {
	// The test above cannot see a handler that reads whichever list part it meets
	// first: the browser sends the typed box before the file, so a handler that
	// took the file regardless would never reach it. Here the file arrives first,
	// which is the arrangement where taking the wrong one is visible.
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for box, value := range map[string]string{
		"name": "file first", "kind": store.KindParse, "pages": "1", "from": fromBox,
	} {
		if err := form.WriteField(box, value); err != nil {
			t.Fatalf("writing the %s box: %v", box, err)
		}
	}
	part, err := form.CreateFormFile(listField, "queries.txt")
	if err != nil {
		t.Fatalf("opening the file part: %v", err)
	}
	if _, err := io.WriteString(part, "from the file"); err != nil {
		t.Fatalf("writing the list: %v", err)
	}
	if err := form.WriteField("queries", "typed one"); err != nil {
		t.Fatalf("writing the typed box: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("closing the form: %v", err)
	}

	s := testServerHolding(t)
	req := httptest.NewRequest(http.MethodPost, uploadAt, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the upload came back %d, want a redirect: %s", rec.Code, rec.Body.String())
	}

	job := theOneJob(t, s)
	left, err := s.store.Pending(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != 1 || left[0].Text != "typed one" {
		t.Errorf("the job holds %+v, want the typed phrase the switch named", left)
	}
}
