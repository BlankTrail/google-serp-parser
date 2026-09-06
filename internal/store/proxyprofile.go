// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoProfile is returned when a profile asked for by id is not there.
var ErrNoProfile = errors.New("store: no such proxy profile")

// ErrProfileName is returned when a profile is saved with no name, or with a
// name another profile already carries.
//
// The two are one error because they are one thing to the reader: the name is
// how a profile is picked out of a list, and a blank one and a repeated one are
// both a name that picks out nothing.
var ErrProfileName = errors.New("store: a proxy profile needs a name of its own")

// ErrLastProfile is returned when deleting would leave none.
//
// Something has to be default — it is what the identities kept warm are raised
// on and what a job that named no profile runs through — so the last one stays.
// Emptying it is done by editing it, which says what happens; deleting it would
// leave the program with no answer and nothing said.
var ErrLastProfile = errors.New("store: this is the last proxy profile")

// Profile is one named set of exits: where the addresses come from and how they
// are used.
//
// Everything here is about the exits and nothing else. How this machine reaches
// BlankTrail — the control address and the key — is the same whichever profile
// runs, and stays in the settings file.
type Profile struct {
	ID   int64
	Name string

	// Kind is where the addresses come from: "file", "url", "gateways", or
	// empty for none at all. Location is the path or the address, and is
	// meaningless under "gateways", where the list lives in BlankTrail.
	Kind     string
	Location string
	// Refresh is how often a list at a URL is read again. Nought reads it once.
	Refresh time.Duration
	// Ban is how long an address that failed is left out of the rotation.
	Ban time.Duration
	// ThreadsPerUpstream is how many identities may work through one exit at a
	// time. Nought is one.
	ThreadsPerUpstream int
	// RenewEvery is how often a port is opened again to change the identity it
	// wears. Nought never does.
	RenewEvery time.Duration
	// Protocol is how the ports themselves are reached: "socks5" or "http".
	// Empty is socks5.
	Protocol string
	// Gateways are the stored configurations this profile egresses through, by
	// name, when Kind is "gateways".
	Gateways []string
	// Default marks the one profile the warm identities are raised on, that the
	// API's own search goes through, and that a job naming none runs on.
	Default bool
}

// profileColumns is the read half of every query below, written once so a
// column added to the table cannot be added to one query and forgotten in
// another.
const profileColumns = `id, name, kind, location, refresh_ms, ban_ms,
	threads_per_upstream, renew_ms, protocol, gateways, is_default`

// scanProfile reads one row in the order profileColumns names.
func scanProfile(row interface{ Scan(...any) error }) (Profile, error) {
	var p Profile
	var refreshMS, banMS, renewMS int64
	var gateways string
	if err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.Location, &refreshMS, &banMS,
		&p.ThreadsPerUpstream, &renewMS, &p.Protocol, &gateways, &p.Default); err != nil {
		return Profile{}, err
	}
	p.Refresh = time.Duration(refreshMS) * time.Millisecond
	p.Ban = time.Duration(banMS) * time.Millisecond
	p.RenewEvery = time.Duration(renewMS) * time.Millisecond
	p.Gateways = gatewaysOf(gateways)
	return p, nil
}

// gatewaysOf reads the names back, one to a line, dropping the blank lines a
// text box leaves behind.
func gatewaysOf(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// gatewayLines is the other direction, and it trims for the same reason: a name
// stored with a space on the end matches no configuration and looks identical
// to one that does.
func gatewayLines(names []string) string {
	var kept []string
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			kept = append(kept, name)
		}
	}
	return strings.Join(kept, "\n")
}

// Profiles is every proxy profile, the default one first and the rest by name,
// which is the order the screen that lists them wants.
func (s *Store) Profiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+profileColumns+` FROM proxy_profiles ORDER BY is_default DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("store: reading the proxy profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading a proxy profile: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading the proxy profiles: %w", err)
	}
	return out, nil
}

// Profile is the one with this id.
func (s *Store) Profile(ctx context.Context, id int64) (Profile, error) {
	p, err := scanProfile(s.db.QueryRowContext(ctx,
		`SELECT `+profileColumns+` FROM proxy_profiles WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, fmt.Errorf("%w: %d", ErrNoProfile, id)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("store: reading proxy profile %d: %w", id, err)
	}
	return p, nil
}

// DefaultProfile is the one marked default.
//
// It answers ErrNoProfile when there is none at all, which is a database that
// has never been carried over rather than a state anything should reach: the
// interface makes one at startup.
func (s *Store) DefaultProfile(ctx context.Context) (Profile, error) {
	p, err := scanProfile(s.db.QueryRowContext(ctx,
		`SELECT `+profileColumns+` FROM proxy_profiles WHERE is_default = 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNoProfile
	}
	if err != nil {
		return Profile{}, fmt.Errorf("store: reading the default proxy profile: %w", err)
	}
	return p, nil
}

// ProfileFor is the profile a job runs on: the one it named, or the default
// when it named none or named one that has since been deleted.
//
// Falling back rather than refusing is deliberate. A job whose profile is gone
// still has queries waiting, and a job that cannot start is worse than a job
// that starts on the exits everything else is using — which is also what it did
// before profiles existed.
func (s *Store) ProfileFor(ctx context.Context, id int64) (Profile, error) {
	if id != 0 {
		p, err := s.Profile(ctx, id)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrNoProfile) {
			return Profile{}, err
		}
	}
	return s.DefaultProfile(ctx)
}

// CreateProfile writes a new profile down and hands back its id.
//
// The first profile written is the default one whatever it says, because a
// database with profiles and no default is one where nothing knows what to warm
// or what a job naming none should run on.
func (s *Store) CreateProfile(ctx context.Context, p Profile) (int64, error) {
	if strings.TrimSpace(p.Name) == "" {
		return 0, ErrProfileName
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: writing a proxy profile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var have int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_profiles`).Scan(&have); err != nil {
		return 0, fmt.Errorf("store: counting the proxy profiles: %w", err)
	}
	if have == 0 {
		p.Default = true
	}
	if p.Default {
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_profiles SET is_default = 0`); err != nil {
			return 0, fmt.Errorf("store: standing the other profiles down: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO proxy_profiles(name, kind, location, refresh_ms, ban_ms,
			threads_per_upstream, renew_ms, protocol, gateways, is_default)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(p.Name), p.Kind, strings.TrimSpace(p.Location),
		p.Refresh.Milliseconds(), p.Ban.Milliseconds(), p.ThreadsPerUpstream,
		p.RenewEvery.Milliseconds(), p.Protocol, gatewayLines(p.Gateways), p.Default)
	if err != nil {
		return 0, nameOr(err, "store: writing a proxy profile")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: reading the new profile's id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: writing a proxy profile: %w", err)
	}
	return id, nil
}

// SaveProfile writes an edited profile back over itself.
func (s *Store) SaveProfile(ctx context.Context, p Profile) error {
	if strings.TrimSpace(p.Name) == "" {
		return ErrProfileName
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: saving proxy profile %d: %w", p.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if p.Default {
		// Stood down first and put up second, in one transaction: the index
		// allows one default and the two statements are never apart.
		if _, err := tx.ExecContext(ctx,
			`UPDATE proxy_profiles SET is_default = 0 WHERE id <> ?`, p.ID); err != nil {
			return fmt.Errorf("store: standing the other profiles down: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE proxy_profiles
		   SET name = ?, kind = ?, location = ?, refresh_ms = ?, ban_ms = ?,
		       threads_per_upstream = ?, renew_ms = ?, protocol = ?, gateways = ?,
		       is_default = ?
		 WHERE id = ?`,
		strings.TrimSpace(p.Name), p.Kind, strings.TrimSpace(p.Location),
		p.Refresh.Milliseconds(), p.Ban.Milliseconds(), p.ThreadsPerUpstream,
		p.RenewEvery.Milliseconds(), p.Protocol, gatewayLines(p.Gateways), p.Default, p.ID)
	if err != nil {
		return nameOr(err, fmt.Sprintf("store: saving proxy profile %d", p.ID))
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %d", ErrNoProfile, p.ID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: saving proxy profile %d: %w", p.ID, err)
	}
	return nil
}

// DeleteProfile removes one.
//
// The jobs that named it keep the number they were written with and fall back
// to the default when they run — see ProfileFor. Rewriting them to point at
// another profile would be this program deciding which exits somebody's job
// should use, which is the decision the profile was for.
func (s *Store) DeleteProfile(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: deleting proxy profile %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	var have int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_profiles`).Scan(&have); err != nil {
		return fmt.Errorf("store: counting the proxy profiles: %w", err)
	}
	if have <= 1 {
		return ErrLastProfile
	}
	var wasDefault bool
	if err := tx.QueryRowContext(ctx,
		`SELECT is_default FROM proxy_profiles WHERE id = ?`, id).Scan(&wasDefault); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrNoProfile, id)
	} else if err != nil {
		return fmt.Errorf("store: reading proxy profile %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_profiles WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: deleting proxy profile %d: %w", id, err)
	}
	if wasDefault {
		// The mark moves rather than vanishing. A database with profiles and no
		// default is one where nothing knows what to warm.
		if _, err := tx.ExecContext(ctx, `
			UPDATE proxy_profiles SET is_default = 1
			 WHERE id = (SELECT id FROM proxy_profiles ORDER BY name LIMIT 1)`); err != nil {
			return fmt.Errorf("store: passing the default on: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: deleting proxy profile %d: %w", id, err)
	}
	return nil
}

// SetDefaultProfile moves the mark to this profile.
func (s *Store) SetDefaultProfile(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: making proxy profile %d the default: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proxy_profiles WHERE id = ?`, id).Scan(&exists); err != nil {
		return fmt.Errorf("store: reading proxy profile %d: %w", id, err)
	}
	if exists == 0 {
		return fmt.Errorf("%w: %d", ErrNoProfile, id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proxy_profiles SET is_default = 0 WHERE id <> ?`, id); err != nil {
		return fmt.Errorf("store: standing the other profiles down: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proxy_profiles SET is_default = 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: making proxy profile %d the default: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: making proxy profile %d the default: %w", id, err)
	}
	return nil
}

// SetJobProfile points a job at a profile.
//
// It is its own call rather than another field of Reshape because it is a
// different question: Reshape changes how hard a job is run, this changes what
// it is run through. The caller decides whether a running job may be pointed
// somewhere else; the store writes what it is told, because a job that is
// stopped and repointed is exactly what this is for.
func (s *Store) SetJobProfile(ctx context.Context, jobID, profileID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET profile_id = ? WHERE id = ?`, profileID, jobID)
	if err != nil {
		return fmt.Errorf("store: pointing job %d at profile %d: %w", jobID, profileID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %d", ErrNoJob, jobID)
	}
	return nil
}

// nameOr turns the database's own word for a repeated name into this package's,
// and leaves everything else as it found it.
//
// The check is on the message because the driver reports a broken unique index
// as a string and there is nothing else in it to read: "UNIQUE constraint
// failed: proxy_profiles.name (2067)". It names the column rather than the
// index, which is why this looks for the column.
//
// It is narrow on purpose. This table has a second unique index — one default
// at a time — and that one is held by the code above rather than reached by a
// caller, so a message about it is a fault here and must not be dressed up as
// something the operator typed.
func nameOr(err error, doing string) error {
	if strings.Contains(err.Error(), "proxy_profiles.name") {
		return ErrProfileName
	}
	return fmt.Errorf("%s: %w", doing, err)
}
