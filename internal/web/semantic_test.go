// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/semantic"
	"github.com/blanktrail/google-serp-parser/internal/settings"
)

// tinyModelBytes is the smallest model Write will make, as a file's contents.
func tinyModelBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "semantic", "testdata", "charsmap.bin"))
	if err != nil {
		t.Fatal(err)
	}
	v := semantic.Vocab{
		Pieces: []string{"[PAD]", "[UNK]", "▁", "▁кофе"},
		Scores: []float32{0, 0, -5, -2},
		Unk:    1,
	}
	var buf bytes.Buffer
	if err := semantic.Write(&buf, v, raw, [][]float32{{0, 0}, {0, 0}, {0.1, 0.1}, {1, 0}}, 4); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

// serverWithModelPath is a server that keeps settings and a model at a path in
// a directory of its own, with nothing there yet.
func serverWithModelPath(t *testing.T) (*Server, string) {
	t.Helper()
	model := filepath.Join(t.TempDir(), semantic.FileName)
	s, err := New(Config{
		Store: testStore(t), Logger: quiet(),
		SettingsPath: settingsFile(t, settings.Settings{ControlURL: "http://127.0.0.1:1", APIKey: "k"}),
		ModelPath:    model,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The tiny model stands in for the real one, so it is the size that counts.
	s.modelSize = int64(len(tinyModelBytes(t)))
	return s, model
}

func TestSettings_OffersTheModelOnlyWhereThereIsNone(t *testing.T) {
	s, model := serverWithModelPath(t)
	page := get(t, s, settingsAt).Body.String()
	if !strings.Contains(page, `action="`+semanticAt+`"`) {
		t.Error("the page does not offer the download of a model that is not there")
	}
	if strings.Contains(page, LangEN.T("settings.semantic.ready")) {
		t.Error("the page says the model is ready when there is none")
	}
	if strings.Contains(page, "data-refresh") {
		t.Error("the page redraws itself with no download running")
	}

	if err := os.WriteFile(model, tinyModelBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	page = get(t, s, settingsAt).Body.String()
	if !strings.Contains(page, LangEN.T("settings.semantic.ready")) {
		t.Error("the page does not say the model is ready")
	}
	// Ready, with the version: the file's name carries it.
	if !strings.Contains(page, "<code>"+semantic.FileName+"</code>") {
		t.Errorf("the ready line does not name the model's version %s", semantic.FileName)
	}
	if strings.Contains(page, `action="`+semanticAt+`"`) {
		t.Error("the page still offers to download a model that is there")
	}
}

func TestSettings_FollowsADownloadInProgress(t *testing.T) {
	s, _ := serverWithModelPath(t)
	s.fetching.mu.Lock()
	s.fetching.running, s.fetching.done, s.fetching.total = true, 50, 100
	s.fetching.mu.Unlock()
	page := get(t, s, settingsAt).Body.String()
	if !strings.Contains(page, "50%") {
		t.Error("the page does not show how far the download is")
	}
	if !strings.Contains(page, "data-refresh") {
		t.Error("the page does not redraw itself while the download runs")
	}
	if strings.Contains(page, `action="`+semanticAt+`"`) {
		t.Error("the page offers a second download while one runs")
	}
}

func TestSettings_UnitOfTheModelSizeComesFromTheCatalogue(t *testing.T) {
	s, _ := serverWithModelPath(t)
	if page := get(t, s, settingsAt).Body.String(); !strings.Contains(page, "MB)") {
		t.Error("the English page does not give the size in MB")
	}
	req := httptest.NewRequest(http.MethodGet, settingsAt, nil)
	req.AddCookie(&http.Cookie{Name: langCookie, Value: string(LangRU)})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "МБ)") {
		t.Error("the Russian page does not give the size in МБ")
	}
}

func TestSettings_HasNoModelBlockWithoutAModelPath(t *testing.T) {
	s, _ := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1", APIKey: "k"})
	if page := get(t, s, settingsAt).Body.String(); strings.Contains(page, semanticAt) {
		t.Error("a server that keeps no model offers to download one")
	}
	rec := postForm(t, s, semanticAt, url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST %s = %d, want 404", semanticAt, rec.Code)
	}
}

// waitFetched waits for the download to be over, however it ended.
func waitFetched(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.fetching.mu.Lock()
		running := s.fetching.running
		s.fetching.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the download did not finish")
}

func TestFetchModel_DownloadsKeepsAndReadsTheModel(t *testing.T) {
	body := tinyModelBytes(t)
	sum := sha256.Sum256(body)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	s, model := serverWithModelPath(t)
	s.modelURL, s.modelSum = srv.URL, hex.EncodeToString(sum[:])

	rec := postForm(t, s, semanticAt, url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != settingsAt {
		t.Fatalf("POST = %d to %q, want 303 to %s", rec.Code, rec.Header().Get("Location"), settingsAt)
	}
	waitFetched(t, s)
	if got, _ := os.ReadFile(model); !bytes.Equal(got, body) {
		t.Fatal("the model was not put in place")
	}
	if m, err := s.semantic.Get(); err != nil || m == nil {
		t.Errorf("the holder cannot read what was downloaded: %v", err)
	}
	if page := get(t, s, settingsAt).Body.String(); !strings.Contains(page, LangEN.T("settings.semantic.ready")) {
		t.Error("the page does not say the model is ready after the download")
	}

	// A press on a page that was left open does not fetch a model that is there.
	before := hits.Load()
	postForm(t, s, semanticAt, url.Values{})
	waitFetched(t, s)
	if hits.Load() != before {
		t.Error("a second press downloaded the model again")
	}
}

func TestFetchModel_APressWhileOneRunsStartsNoSecond(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	s, _ := serverWithModelPath(t)
	s.modelURL, s.modelSum = srv.URL, strings.Repeat("0", 64)
	s.fetching.mu.Lock()
	s.fetching.running = true
	s.fetching.mu.Unlock()

	postForm(t, s, semanticAt, url.Values{})
	time.Sleep(200 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("a press while a download ran made %d more requests", n)
	}
}

func TestFetchModel_ShowsHowFarTheRealDownloadHasGot(t *testing.T) {
	body := tinyModelBytes(t)
	sum := sha256.Sum256(body)
	half, release := len(body)/2, make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body[:half])
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write(body[half:])
	}))
	defer srv.Close()
	defer close(release)
	s, _ := serverWithModelPath(t)
	s.modelURL, s.modelSum = srv.URL, hex.EncodeToString(sum[:])

	postForm(t, s, semanticAt, url.Values{})
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.fetching.mu.Lock()
		done, total := s.fetching.done, s.fetching.total
		s.fetching.mu.Unlock()
		if done == int64(half) {
			// The total is what the server said, not the release's size: a real
			// model and a tiny one would otherwise show the same percentage.
			if total != int64(len(body)) {
				t.Errorf("total = %d, want the %d the server announced", total, len(body))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress never reached %d (at %d)", half, done)
		}
		time.Sleep(5 * time.Millisecond)
	}
	page := get(t, s, settingsAt).Body.String()
	if !strings.Contains(page, "data-refresh") || !strings.Contains(page, "%") {
		t.Error("the page does not follow a download that is really running")
	}
}

func TestFetchModel_ReadsTheNewFileAfterABrokenOneWasRemembered(t *testing.T) {
	body := tinyModelBytes(t)
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	s, model := serverWithModelPath(t)
	s.modelURL, s.modelSum = srv.URL, hex.EncodeToString(sum[:])
	if err := os.WriteFile(model, []byte("not a model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.semantic.Get(); err == nil {
		t.Fatal("a broken file was read")
	}
	if err := os.Remove(model); err != nil {
		t.Fatal(err)
	}

	postForm(t, s, semanticAt, url.Values{})
	waitFetched(t, s)
	if m, err := s.semantic.Get(); err != nil || m == nil {
		t.Errorf("the holder still remembers the broken file: %v", err)
	}
}

func TestFetchModel_AFailureIsToldAndLeavesNoModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("something else"))
	}))
	defer srv.Close()
	s, model := serverWithModelPath(t)
	s.modelURL, s.modelSum = srv.URL, strings.Repeat("0", 64)

	postForm(t, s, semanticAt, url.Values{})
	waitFetched(t, s)
	if _, err := os.Stat(model); !os.IsNotExist(err) {
		t.Error("a download that did not hash right left a model")
	}
	page := get(t, s, settingsAt).Body.String()
	if !strings.Contains(page, LangEN.T("settings.semantic.failed")) {
		t.Error("the page does not say the download failed")
	}
	if !strings.Contains(page, `action="`+semanticAt+`"`) {
		t.Error("the page does not offer to try again")
	}

	// The next try starts clean: a sentence about the last failure does not stay
	// on a page that is showing a download which is going well.
	body := tinyModelBytes(t)
	sum := sha256.Sum256(body)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer good.Close()
	s.modelURL, s.modelSum = good.URL, hex.EncodeToString(sum[:])
	postForm(t, s, semanticAt, url.Values{})
	waitFetched(t, s)
	if page := get(t, s, settingsAt).Body.String(); strings.Contains(page, LangEN.T("settings.semantic.failed")) {
		t.Error("the sentence about the earlier failure outlived a download that worked")
	}
}

// offersDownload says the page has the button for the model.
func offersDownload(page string) bool { return strings.Contains(page, `action="`+semanticAt+`"`) }

func TestSettings_ADamagedModelIsToldAndTheButtonIsOfferedAgain(t *testing.T) {
	good := tinyModelBytes(t)
	for name, content := range map[string][]byte{
		"not a model":        []byte("this is not a model at all"),
		"the wrong size":     append(append([]byte{}, good...), 0),
		"cut off, signature": good[:len(good)/2],
	} {
		s, model := serverWithModelPath(t)
		if err := os.WriteFile(model, content, 0o644); err != nil {
			t.Fatal(err)
		}
		page := get(t, s, settingsAt).Body.String()
		if !strings.Contains(page, LangEN.T("settings.semantic.damaged")) {
			t.Errorf("%s: the page does not say the file is damaged", name)
		}
		if !offersDownload(page) {
			t.Errorf("%s: the page does not offer the download again", name)
		}
		if strings.Contains(page, LangEN.T("settings.semantic.ready")) {
			t.Errorf("%s: the page calls a damaged file ready", name)
		}
	}
}

func TestSettings_AModelThatFailedToLoadIsDamaged(t *testing.T) {
	// The right size and the right signature, and still not a model: only a read
	// finds that out, and once a read has, the page says so.
	good := tinyModelBytes(t)
	broken := append([]byte{}, good...)
	for i := 16; i < len(broken); i++ {
		broken[i] = 0xff
	}
	s, model := serverWithModelPath(t)
	if err := os.WriteFile(model, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if page := get(t, s, settingsAt).Body.String(); !strings.Contains(page, LangEN.T("settings.semantic.ready")) {
		t.Fatal("a file that looks right and was not read is not called ready")
	}
	if _, err := s.semantic.Get(); err == nil {
		t.Fatal("a file of 0xff was read as a model")
	}
	page := get(t, s, settingsAt).Body.String()
	if !strings.Contains(page, LangEN.T("settings.semantic.damaged")) || !offersDownload(page) {
		t.Error("the page does not call a file that failed to load damaged")
	}
}

func TestFetchModel_APressReplacesADamagedFile(t *testing.T) {
	body := tinyModelBytes(t)
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	s, model := serverWithModelPath(t)
	s.modelURL, s.modelSum = srv.URL, hex.EncodeToString(sum[:])
	if err := os.WriteFile(model, []byte("not a model"), 0o644); err != nil {
		t.Fatal(err)
	}

	postForm(t, s, semanticAt, url.Values{})
	waitFetched(t, s)
	if got, _ := os.ReadFile(model); !bytes.Equal(got, body) {
		t.Fatal("the damaged file was not replaced")
	}
	page := get(t, s, settingsAt).Body.String()
	if !strings.Contains(page, LangEN.T("settings.semantic.ready")) || strings.Contains(page, LangEN.T("settings.semantic.damaged")) {
		t.Error("the page does not show the model ready after it replaced a damaged one")
	}
}

func TestSettings_ThePercentStaysBetweenNoughtAndAHundred(t *testing.T) {
	s, _ := serverWithModelPath(t)
	s.fetching.mu.Lock()
	s.fetching.running, s.fetching.done, s.fetching.total = true, 250, 100
	s.fetching.mu.Unlock()
	if got := s.modelReading().Percent; got != 100 {
		t.Errorf("Percent with more than the total = %d, want 100", got)
	}
}
