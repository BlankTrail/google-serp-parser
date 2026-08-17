// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// windBackToVersionThree turns a database this build has just made into the one
// the build before the pool columns would have left behind.
func windBackToVersionThree(t *testing.T, s *Store) {
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
func windBackToVersionOne(t *testing.T, s *Store) {
	t.Helper()
	windBackToVersionThree(t, s)
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
	if err := again.Reshape(context.Background(), id, 5, 9); err != nil {
		t.Errorf("a job that came through the upgrade cannot be given a pool: %v", err)
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
