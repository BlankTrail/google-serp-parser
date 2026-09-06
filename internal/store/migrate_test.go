// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// windBackToVersionFour turns a database this build has just made into the one
// the build before the third kind of job would have left behind: no target
// column, and a kind column that takes two words rather than three.
//
// It rebuilds the table rather than dropping the column, because the narrow
// CHECK is the half of version four that matters here. A wind-back that left the
// wider one behind would hand the upgrade a table it did not have to widen, and
// the test resting on it would pass with the widening taken out.
//
// It winds back the way the step goes forward, and on one connection with the
// foreign keys held off, for the same reason: dropping a table other tables
// point at takes their rows with it, and a wind-back that quietly emptied the
// database would leave every test after it proving that nothing survives
// nothing.
func windBackToVersionFour(t *testing.T, s *Store) {
	t.Helper()
	windBackToVersionSix(t, s)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("taking a connection to wind back on: %v", err)
	}
	defer func() { _ = conn.Close() }()

	for _, stmt := range []string{
		`PRAGMA foreign_keys = OFF`,
		`CREATE TABLE jobs_v4 (
		    id          INTEGER PRIMARY KEY,
		    name        TEXT    NOT NULL,
		    created_at  TEXT    NOT NULL,
		    finished_at TEXT,
		    pages       INTEGER NOT NULL,
		    spec_name   TEXT    NOT NULL DEFAULT '',
		    country     TEXT    NOT NULL DEFAULT '',
		    language    TEXT    NOT NULL DEFAULT '',
		    kind        TEXT    NOT NULL DEFAULT 'search' CHECK (kind IN ('search', 'index')),
		    unique_by   TEXT    NOT NULL DEFAULT '' CHECK (unique_by IN ('', 'url', 'host')),
		    plan_ready  INTEGER NOT NULL DEFAULT 0,
		    dropped     INTEGER NOT NULL DEFAULT 0,
		    ports       INTEGER NOT NULL DEFAULT 0 CHECK (ports   >= 0),
		    threads     INTEGER NOT NULL DEFAULT 0 CHECK (threads >= 0)
		)`,
		`INSERT INTO jobs_v4(id, name, created_at, finished_at, pages, spec_name, country,
		                     language, kind, unique_by, plan_ready, dropped, ports, threads)
		      SELECT id, name, created_at, finished_at, pages, spec_name, country,
		             language, kind, unique_by, plan_ready, dropped, ports, threads
		        FROM jobs`,
		`DROP TABLE jobs`,
		`ALTER TABLE jobs_v4 RENAME TO jobs`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("winding back with %q: %v", stmt, err)
		}
	}
}

// windBackToVersionThree turns a database this build has just made into the one
// the build before the pool columns would have left behind.
// windBackToVersionThree takes a database all the way back to three. It is an
// entry point, so it starts by undoing the steps above it; the part that undoes
// the fourth step alone is separate, because a caller that has already come
// down through it must not do it twice.
func windBackToVersionThree(t *testing.T, s *Store) {
	t.Helper()
	windBackToVersionSix(t, s)
	windBackToVersionThreeOnly(t, s)
}

func windBackToVersionThreeOnly(t *testing.T, s *Store) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE jobs DROP COLUMN ports`,
		`ALTER TABLE jobs DROP COLUMN threads`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("winding back with %q: %v", stmt, err)
		}
	}
}

// windBackToVersionOne turns a database this build has just made into the
// database the first build would have left behind: every column and table the
// steps after it brought is gone, and the stamp says 1.
//
// It goes through the wind-back for each version rather than listing every
// column itself, so that a step added without a wind-back for it is a test that
// fails rather than a step the upgrade path is never asked to run.
// windBackToVersionSix undoes the seventh step, so a database can be taken back
// past the choice of what a result keeps.
//
// Every step needs one of these, and a step added without it silently stops
// being covered by the tests that walk the whole path — which is why the wind
// backs are chained rather than written out at each caller.
func windBackToVersionSix(t *testing.T, s *Store) {
	t.Helper()
	for _, stmt := range []string{
		// The profiles and the column naming one, which the thirteenth step
		// brought. The column has to go too: SQLite has no "add column if it is
		// not there", so a second walk of the path over a database that kept it
		// stops on a duplicate name.
		`DROP INDEX IF EXISTS proxy_profiles_one_default`,
		`DROP INDEX IF EXISTS proxy_profiles_name`,
		`DROP TABLE IF EXISTS proxy_profiles`,
		`ALTER TABLE jobs DROP COLUMN profile_id`,
		`DROP TABLE IF EXISTS rested_upstreams`,
		// The index goes first: SQLite will not drop a column an index is built
		// on, and the message it gives says nothing about the index.
		`ALTER TABLE jobs DROP COLUMN cooldown_ms`,
		`ALTER TABLE jobs DROP COLUMN device`,
		`DROP INDEX IF EXISTS queries_settled_at`,
		`ALTER TABLE queries DROP COLUMN settled_at`,
		`ALTER TABLE jobs DROP COLUMN fields`,
		`ALTER TABLE results DROP COLUMN display_path`,
		`PRAGMA user_version = 6`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("winding back with %q: %v", stmt, err)
		}
	}
}

func windBackToVersionOne(t *testing.T, s *Store) {
	t.Helper()
	windBackToVersionFour(t, s)
	windBackToVersionThreeOnly(t, s)
	for _, stmt := range []string{
		`ALTER TABLE jobs DROP COLUMN dropped`,
		`DROP TABLE seen`,
		`DROP TABLE api_keys`,
		`ALTER TABLE jobs DROP COLUMN kind`,
		`ALTER TABLE jobs DROP COLUMN unique_by`,
		`ALTER TABLE jobs DROP COLUMN plan_ready`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("winding back with %q: %v", stmt, err)
		}
	}
}

// jobWithAnUnfinishedPlan writes the wreckage of a list that was still being
// read when the upload broke off: a job, some of its queries, and no word that
// the plan is whole.
func jobWithAnUnfinishedPlan(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	res, err := s.db.Exec(
		`INSERT INTO jobs(name, created_at, pages, plan_ready)
		 VALUES(?, '2026-08-16T10:00:00Z', 1, 0)`, name)
	if err != nil {
		t.Fatalf("writing a half-loaded job: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("reading the job id: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO queries(job_id, ordinal, text) VALUES(?, 0, 'a')`, id); err != nil {
		t.Fatalf("writing a query of a half-loaded job: %v", err)
	}
	return id
}

func TestOpen_CarriesAnOlderDatabaseAndEverythingInItToThisVersion(t *testing.T) {
	// This is the first schema change the program has ever made, and until this
	// test the upgrade path had never run. An upgrade that loses a night's work
	// is worse than no upgrade at all.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := jobWith(t, s, "a", "b")
	mustRecord(t, s, id, 0, "one.test", "two.test")
	windBackToVersionOne(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("opening a version-1 database: %v", err)
	}
	defer func() { _ = again.Close() }()

	jobs, err := again.Jobs(context.Background(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs survived the upgrade, want 1", len(jobs))
	}
	if jobs[0].Total != 2 || jobs[0].Done != 1 || jobs[0].Pending != 1 {
		t.Errorf("the upgraded job reads as %d queries, %d done, %d pending — want 2, 1, 1",
			jobs[0].Total, jobs[0].Done, jobs[0].Pending)
	}
	var results int
	if err := again.db.QueryRow(`SELECT count(*) FROM results`).Scan(&results); err != nil {
		t.Fatalf("counting results: %v", err)
	}
	if results != 2 {
		t.Errorf("%d results survived the upgrade, want 2", results)
	}

	var version int
	if err := again.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("the upgraded database says version %d, want %d", version, schemaVersion)
	}

	var kind, uniqueBy string
	var dropped, ports, threads int
	if err := again.db.QueryRow(`SELECT kind, unique_by, dropped, ports, threads FROM jobs WHERE id = ?`, id).
		Scan(&kind, &uniqueBy, &dropped, &ports, &threads); err != nil {
		t.Fatalf("reading the new columns: %v", err)
	}
	if kind != "search" || uniqueBy != "" || dropped != 0 {
		t.Errorf("a job from before the columns existed reads as kind %q, unique_by %q, %d dropped — want a plain search that filtered nothing", kind, uniqueBy, dropped)
	}
	if ports != 0 || threads != 0 {
		t.Errorf("a job from before the pool columns existed reads as %d ports and %d threads — want the zero that says it named none", ports, threads)
	}

	if _, err := again.db.Exec(`INSERT INTO seen(job_id, key) VALUES(?, 'https://one.test/1')`, id); err != nil {
		t.Errorf("the upgraded database cannot record what a job has seen: %v", err)
	}
	if _, err := again.db.Exec(
		`INSERT INTO api_keys(name, prefix, hash, created_at)
		 VALUES('after', 'abcd', 'a-hash', '2026-08-16T10:00:00Z')`); err != nil {
		t.Errorf("the upgraded database cannot hold a key: %v", err)
	}
}

func TestOpen_LeavesAJobFromBeforeThePlanFlagResumable(t *testing.T) {
	// Jobs written by an earlier build had their whole plan committed in one
	// transaction, so every one of them is whole. Coming out of the upgrade
	// without the flag would make the history unresumable in a single step.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	jobWith(t, s, "a")
	windBackToVersionOne(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("opening a version-1 database: %v", err)
	}
	defer func() { _ = again.Close() }()

	if _, err := again.LastUnfinished(context.Background(), "j"); err != nil {
		t.Errorf("a job that came through the upgrade cannot be taken up: %v", err)
	}
}

func TestOpen_CarriesAJobWrittenBeforeThePoolColumnsAndLeavesItRunnable(t *testing.T) {
	// The one upgrade this version makes, run against a database with a night's
	// work in it rather than an empty one — an empty database would take any step
	// that compiles.
	//
	// A job recorded before a job could carry a pool has to go on being work. It
	// comes back saying it named no size, which is what the raising of a pool is
	// prepared for, rather than a size of nothing, which is a job that would
	// never run again.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := jobWith(t, s, "a", "b")
	mustRecord(t, s, id, 0, "one.test", "two.test")
	windBackToVersionThree(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("opening a version-3 database: %v", err)
	}
	defer func() { _ = again.Close() }()

	var version int
	if err := again.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("the upgraded database says version %d, want %d", version, schemaVersion)
	}

	sum, err := again.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Total != 2 || sum.Done != 1 {
		t.Errorf("the upgraded job reads as %d queries, %d done — want 2 and 1", sum.Total, sum.Done)
	}
	if sum.Ports != 0 || sum.Threads != 0 {
		t.Errorf("a job from before the columns reads as %d ports and %d threads, want neither named",
			sum.Ports, sum.Threads)
	}

	taken, err := again.LastUnfinished(context.Background(), "j")
	if err != nil {
		t.Fatalf("a job that came through the upgrade cannot be taken up: %v", err)
	}
	if taken.Spec.Ports != 0 || taken.Spec.Threads != 0 {
		t.Errorf("the resumed job asks for %d ports and %d threads, want neither named",
			taken.Spec.Ports, taken.Spec.Threads)
	}
	pending, err := again.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("%d queries left to run after the upgrade, want the one that was not done", len(pending))
	}
	if err := again.Reshape(context.Background(), id, 5, 9, 5, 0); err != nil {
		t.Errorf("a job that came through the upgrade cannot be given a pool: %v", err)
	}
}

func TestOpen_CarriesAFilledDatabaseThroughTheTableBeingRebuiltForTheThirdKind(t *testing.T) {
	// The one upgrade this version makes, and the only one so far that builds a
	// table again instead of adding to it. Everything captured hangs off the jobs
	// table by id and cascades when a job is deleted, so the step is one wrong
	// statement away from taking every query, page and result in the database
	// with it — and against an empty database that step would look perfect.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "nightly", Kind: KindIndex, Pages: 2, UniqueBy: UniqueHost,
			Country: "de", Language: "de", Ports: 5, Threads: 3},
		[]string{"a", "b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	// Two results of two hosts, under a filter that drops repeats, so the job
	// leaves a row in every table that hangs off it — including what it has seen,
	// which is the one whose rows nothing would ever miss.
	mustRecord(t, s, id, 0, "one.test", "two.test")
	windBackToVersionFour(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("opening a version-4 database: %v", err)
	}
	defer func() { _ = again.Close() }()

	var version int
	if err := again.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("the upgraded database says version %d, want %d", version, schemaVersion)
	}

	// What hangs off the job. Counted rather than read back through a summary,
	// because the failure being guarded against deletes rows rather than
	// changing them, and a summary of nothing reads as a job that captured
	// nothing.
	for _, c := range []struct {
		what string
		want int
	}{{"queries", 2}, {"pages", 1}, {"results", 2}, {"seen", 2}} {
		var got int
		if err := again.db.QueryRow(`SELECT count(*) FROM ` + c.what).Scan(&got); err != nil {
			t.Fatalf("counting %s: %v", c.what, err)
		}
		if got != c.want {
			t.Errorf("%d rows left in %s after the upgrade, want %d", got, c.what, c.want)
		}
	}

	sum, err := again.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	// Every column the old table carried, read back off the new one. A rebuild
	// that copied the columns in the wrong order compiles and runs, and files the
	// country under the language.
	switch {
	case sum.Name != "nightly":
		t.Errorf("the upgraded job is named %q, want nightly", sum.Name)
	case sum.Kind != KindIndex:
		t.Errorf("the upgraded job reads as kind %q, want %q", sum.Kind, KindIndex)
	case sum.UniqueBy != UniqueHost:
		t.Errorf("the upgraded job drops repeats by %q, want %q", sum.UniqueBy, UniqueHost)
	case sum.Pages != 2 || sum.Ports != 5 || sum.Threads != 3:
		t.Errorf("the upgraded job reads as %d pages, %d ports, %d threads — want 2, 5 and 3",
			sum.Pages, sum.Ports, sum.Threads)
	case sum.Country != "de" || sum.Language != "de":
		t.Errorf("the upgraded job reads as country %q, language %q — want de and de",
			sum.Country, sum.Language)
	// The kind of result page is empty, and has to be: this database was wound
	// back to a version that had no such column, so there was nothing for the
	// upgrade to carry. Empty is the desktop, which is what every job written
	// before there was a choice ran on.
	case sum.Device != "":
		t.Errorf("a job from before the column reads as asking for %q, want the desktop it ran on",
			sum.Device)
	case sum.Total != 2 || sum.Done != 1 || sum.Pending != 1:
		t.Errorf("the upgraded job reads as %d queries, %d done, %d pending — want 2, 1, 1",
			sum.Total, sum.Done, sum.Pending)
	}
	// A job written before there were three kinds is about no site, and that is
	// not the same as a site nobody could find.
	if sum.Target != "" {
		t.Errorf("a job from before the column reads as being about %q, want no site named", sum.Target)
	}

	if _, err := again.LastUnfinished(context.Background(), "nightly"); err != nil {
		t.Errorf("a job that came through the upgrade cannot be taken up: %v", err)
	}
	if _, err := again.CreateJob(context.Background(),
		JobSpec{Name: "after", Kind: KindPosition, Target: "example.com", Pages: 1},
		[]string{"a"}); err != nil {
		t.Errorf("the upgraded database will not take the kind the step was for: %v", err)
	}
}

func TestOpen_LeavesTheVersionAloneWhenAStepCannotFinish(t *testing.T) {
	// A step that runs half way and still records its version leaves a database
	// no version describes, and the next open would skip the rest of the step
	// for good. Here the upgrade meets a name it cannot create; the proof that
	// nothing was kept is that the same upgrade goes through once the obstacle
	// is out of the way, rather than tripping over its own first half.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := jobWith(t, s, "a")
	windBackToVersionOne(t, s)
	if _, err := s.db.Exec(`CREATE TABLE seen (mine TEXT)`); err != nil {
		t.Fatalf("standing in the way of the upgrade: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("Open reported success although the upgrade could not finish")
	}

	// Reading the version and clearing the obstacle both need a connection, and
	// Open refuses this database, so the rest of the test goes straight to the
	// driver.
	stuck, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopening the blocked database: %v", err)
	}
	var version int
	if err := stuck.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	if version != 1 {
		t.Errorf("a database whose upgrade failed says version %d, want 1", version)
	}
	if _, err := stuck.Exec(`DROP TABLE seen`); err != nil {
		t.Fatalf("clearing the obstacle: %v", err)
	}
	if err := stuck.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("the failed upgrade left something behind: %v", err)
	}
	defer func() { _ = again.Close() }()

	var ready int
	if err := again.db.QueryRow(`SELECT plan_ready FROM jobs WHERE id = ?`, id).Scan(&ready); err != nil {
		t.Fatalf("reading the job: %v", err)
	}
	if ready != 1 {
		t.Error("the retried upgrade did not finish")
	}
}

func TestCreateJob_MarksThePlanReadyBecauseItWroteTheWholeList(t *testing.T) {
	// A plan written in one transaction is whole the moment the job exists. The
	// flag is for the loader that reads a list too large for one transaction and
	// writes it in batches; it must not make ordinary jobs look half-loaded.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "j", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var ready int
	if err := s.db.QueryRow(`SELECT plan_ready FROM jobs WHERE id = ?`, id).Scan(&ready); err != nil {
		t.Fatalf("query: %v", err)
	}
	if ready != 1 {
		t.Error("a job whose whole list was written reads as half-loaded")
	}
}

func TestPending_RefusesToHandOutWorkFromAPlanThatWasNeverFinished(t *testing.T) {
	// Running part of a list and stamping the job done is the exact loss the
	// one-transaction plan was there to prevent. Answering "nothing pending"
	// would read as a finished job, so this says why instead.
	s := testStore(t)
	id := jobWithAnUnfinishedPlan(t, s, "half")

	if _, err := s.Pending(context.Background(), id); !errors.Is(err, ErrPlanUnfinished) {
		t.Errorf("Pending returned %v, want ErrPlanUnfinished", err)
	}
}

func TestPending_StillAnswersForAJobWhosePlanIsWhole(t *testing.T) {
	s := testStore(t)
	id := jobWith(t, s, "a", "b")
	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("%d queries pending, want 2", len(pending))
	}
}

func TestLastUnfinished_PassesOverAJobWhosePlanWasNeverFinished(t *testing.T) {
	// A broken upload is not the run to resume. Choosing it would refuse the
	// name outright, when an older job of that name is still there to take up.
	//
	// The half-loaded job is stamped the later of the two, because a resume
	// takes the newest job of a name: left older, it would be passed over by the
	// ordering alone and the test would hold whether the filter was there or not.
	s := testStore(t)
	whole := jobWith(t, s, "a")
	if _, err := s.db.Exec(`UPDATE jobs SET name = 'nightly' WHERE id = ?`, whole); err != nil {
		t.Fatalf("naming the job: %v", err)
	}
	stampJob(t, s, whole, "2026-08-15T10:00:00Z")
	broken := jobWithAnUnfinishedPlan(t, s, "nightly")
	stampJob(t, s, broken, "2026-08-16T10:00:00Z")

	got, err := s.LastUnfinished(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("LastUnfinished: %v", err)
	}
	if got.ID != whole {
		t.Errorf("LastUnfinished chose job %d, want %d — a half-loaded plan is not work", got.ID, whole)
	}
}

func TestJobs_TakeOnlyTheKindsAndFiltersThatMeanSomething(t *testing.T) {
	// A job stored with a kind no engine answers to, or a filter no code reads,
	// would run as neither of the things it could have been and say nothing
	// about it. The database is the last place that can still refuse it.
	s := testStore(t)
	for _, c := range []struct {
		column, value string
		want          bool
	}{
		{"kind", "search", true},
		{"kind", "index", true},
		{"kind", "", false},
		{"kind", "images", false},
		{"kind", "parse", false},
		{"unique_by", "", true},
		{"unique_by", "url", true},
		{"unique_by", "host", true},
		{"unique_by", "domain", false},
	} {
		_, err := s.db.Exec(
			`INSERT INTO jobs(name, created_at, pages, `+c.column+`)
			 VALUES('j', '2026-08-16T10:00:00Z', 1, ?)`, c.value)
		if got := err == nil; got != c.want {
			t.Errorf("%s = %q was %s (%v), want it %s",
				c.column, c.value,
				map[bool]string{true: "taken", false: "refused"}[got], err,
				map[bool]string{true: "taken", false: "refused"}[c.want])
		}
	}
}

func TestJobs_RefuseAPositionCheckWithNothingToLookFor(t *testing.T) {
	// The store refuses this before the insert and the form refuses it before
	// that, so nothing in this program can get here. It is the guard against a
	// writer that went around both: a position check with no site would run, find
	// nothing to recognise, and answer "not found" for every phrase in the list —
	// a wrong answer with nothing left in the database to disbelieve it by.
	//
	// The other two kinds take the empty site, because for them there is nothing
	// to name: a parse is about every site the page carried, and an index check
	// is about the address on the line.
	s := testStore(t)
	for _, c := range []struct {
		kind, target string
		want         bool
	}{
		{"position", "example.com", true},
		{"position", "", false},
		{"search", "", true},
		{"index", "", true},
	} {
		_, err := s.db.Exec(
			`INSERT INTO jobs(name, created_at, pages, kind, target)
			 VALUES('j', '2026-08-17T10:00:00Z', 1, ?, ?)`, c.kind, c.target)
		if got := err == nil; got != c.want {
			t.Errorf("kind %q about %q was %s (%v), want it %s",
				c.kind, c.target,
				map[bool]string{true: "taken", false: "refused"}[got], err,
				map[bool]string{true: "taken", false: "refused"}[c.want])
		}
	}
}

func TestJobs_RefuseAPoolSmallerThanNothing(t *testing.T) {
	// Nothing this package writes can get here — a size below nothing is read as
	// a size nobody named long before the insert — so this is the guard against a
	// writer that went around it. Zero has a meaning and has to be taken; minus
	// one has none under any reading, and a job carrying it would be raised
	// against a number no pool can be built from.
	s := testStore(t)
	for _, c := range []struct {
		column string
		value  int
		want   bool
	}{
		{"ports", 0, true},
		{"ports", 4, true},
		{"ports", -1, false},
		{"threads", 0, true},
		{"threads", 7, true},
		{"threads", -1, false},
	} {
		_, err := s.db.Exec(
			`INSERT INTO jobs(name, created_at, pages, `+c.column+`)
			 VALUES('j', '2026-08-16T10:00:00Z', 1, ?)`, c.value)
		if got := err == nil; got != c.want {
			t.Errorf("%s = %d was %s (%v), want it %s",
				c.column, c.value,
				map[bool]string{true: "taken", false: "refused"}[got], err,
				map[bool]string{true: "taken", false: "refused"}[c.want])
		}
	}
}

func TestSeen_TakesOneRowPerKeyAndSaysWhichInsertWasNew(t *testing.T) {
	// This is the whole of the uniqueness filter: the insert itself answers
	// whether a result is new, so nothing has to be counted or looked up twice.
	s := testStore(t)
	id := jobWith(t, s, "a")

	first, err := s.db.Exec(`INSERT OR IGNORE INTO seen(job_id, key) VALUES(?, 'example.test')`, id)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if n, err := first.RowsAffected(); err != nil || n != 1 {
		t.Errorf("the first sighting of a key wrote %d rows (err %v), want 1", n, err)
	}
	second, err := s.db.Exec(`INSERT OR IGNORE INTO seen(job_id, key) VALUES(?, 'example.test')`, id)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if n, err := second.RowsAffected(); err != nil || n != 0 {
		t.Errorf("the second sighting of a key wrote %d rows (err %v), want 0", n, err)
	}

	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM seen`).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d rows for one key, want 1", rows)
	}
}

func TestSeen_KeepsTheSameKeyApartForTwoJobs(t *testing.T) {
	// The filter works within a job. A host caught last month must be allowed
	// to turn up again in this month's run.
	s := testStore(t)
	one := jobWith(t, s, "a")
	two := jobWith(t, s, "b")
	for _, id := range []int64{one, two} {
		res, err := s.db.Exec(`INSERT OR IGNORE INTO seen(job_id, key) VALUES(?, 'example.test')`, id)
		if err != nil {
			t.Fatalf("insert for job %d: %v", id, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("job %d could not record a key another job had seen", id)
		}
	}
}

func TestSeen_CarriesNothingBesideTheKeyItIsSearchedBy(t *testing.T) {
	// Ten million keys are stored, and every one of them is looked up by the
	// pair it is keyed on. A row id would be a second tree holding the same
	// rows, paid for on every insert and read by nobody.
	s := testStore(t)
	rows, err := s.db.Query(`SELECT rowid FROM seen`)
	if err == nil {
		_ = rows.Close()
		t.Error("the seen table carries a row id, so it is stored twice over")
	}
}

func TestSeen_GoesWithTheJobItBelongsTo(t *testing.T) {
	s := testStore(t)
	id := jobWith(t, s, "a")
	if _, err := s.db.Exec(`INSERT INTO seen(job_id, key) VALUES(?, 'example.test')`, id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id); err != nil {
		t.Fatalf("deleting the job: %v", err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM seen`).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d rows outlived the job they belong to", rows)
	}
}

func TestAPIKeys_HoldOneRowPerHash(t *testing.T) {
	// Nothing in this build reads the table; it is here so that one version
	// number covers every change. The constraint the reader will lean on is
	// worth pinning now rather than discovering missing later.
	s := testStore(t)
	const insert = `INSERT INTO api_keys(name, prefix, hash, created_at)
	                VALUES(?, 'abcd', ?, '2026-08-16T10:00:00Z')`
	if _, err := s.db.Exec(insert, "one", "a-hash"); err != nil {
		t.Fatalf("first key: %v", err)
	}
	if _, err := s.db.Exec(insert, "two", "a-hash"); err == nil {
		t.Error("two keys were stored under one hash")
	}
	if _, err := s.db.Exec(insert, "three", "another-hash"); err != nil {
		t.Errorf("a second key of its own could not be stored: %v", err)
	}
}
