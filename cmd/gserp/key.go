// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/store"
)

// keyOptions is everything a key command was asked to do.
type keyOptions struct {
	DB   string
	Name string
	ID   int64
}

// keyFlags declares the flags one key command takes. It is separate from the
// parsing so the help text can be checked against the flags themselves rather
// than against a list somebody has to remember to keep up.
func keyFlags(action string, opts *keyOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("key "+action, flag.ContinueOnError)
	fs.StringVar(&opts.DB, "db", "gserp.db", "database the keys are kept in")
	switch action {
	case "new":
		fs.StringVar(&opts.Name, "name", "", "name to file the key under")
	case "revoke":
		fs.Int64Var(&opts.ID, "id", 0, "key to stop, by the id gserp key list prints")
	}
	return fs
}

// keyCommand issues, lists and stops the keys that let a program reach this
// one.
//
// Everything it says goes to the writer it was handed, and it logs nothing. The
// secret it prints is the single thing this program puts on screen on purpose,
// and a line written through a logger goes on to wherever that output is kept.
func keyCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("key takes one of: new, list, revoke")
	}
	action := args[0]
	switch action {
	case "new", "list", "revoke":
	default:
		return fmt.Errorf("unknown key command %q, want one of: new, list, revoke", action)
	}

	var opts keyOptions
	fs := keyFlags(action, &opts)
	fs.SetOutput(out)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	opts.Name = strings.TrimSpace(opts.Name)

	// Refused before anything is opened: a command that cannot go through must
	// not leave a database behind that nobody asked for.
	switch {
	case action == "new" && opts.Name == "":
		return errors.New("key new needs -name: a listing has nothing else to tell one key from another by")
	case action == "revoke" && opts.ID < 1:
		return errors.New("key revoke needs -id, as gserp key list prints it")
	}

	st, err := store.Open(opts.DB)
	if err != nil {
		return opts.scrubbed(err)
	}
	defer func() { _ = st.Close() }()

	switch action {
	case "new":
		return opts.issue(ctx, out, st)
	case "list":
		return opts.list(ctx, out, st)
	default:
		return opts.revoke(ctx, out, st)
	}
}

// issue makes a key and prints it, once.
//
// The warning shares the line with the secret. What follows a key on screen is
// scrolled past, and somebody who did not save it finds out at their first
// request, when the only way out is another key.
func (o keyOptions) issue(ctx context.Context, out io.Writer, st *store.Store) error {
	secret, key, err := st.CreateKey(ctx, o.Name)
	if err != nil {
		return o.scrubbed(err)
	}
	_, _ = fmt.Fprintf(out, "key %d %q — shown once and cannot be read back afterwards, save it now: %s\n",
		key.ID, key.Name, secret)
	return nil
}

// list prints what exists.
//
// The prefix is all of a key that goes out: it is enough to recognise one by
// and not enough to use. The hash the database holds is left out for the same
// reason it is a hash.
func (o keyOptions) list(ctx context.Context, out io.Writer, st *store.Store) error {
	keys, err := st.Keys(ctx)
	if err != nil {
		return o.scrubbed(err)
	}
	if len(keys) == 0 {
		_, _ = fmt.Fprintln(out, "no keys — gserp key new -name <name> issues one")
		return nil
	}
	_, _ = fmt.Fprintf(out, "%-5s %-20s %-10s %-22s %-22s %s\n",
		"ID", "NAME", "PREFIX", "CREATED", "LAST USED", "STATE")
	for _, k := range keys {
		_, _ = fmt.Fprintf(out, "%-5d %-20s %-10s %-22s %-22s %s\n",
			k.ID, k.Name, k.Prefix, k.CreatedAt.Format(time.RFC3339), lastUse(k), keyState(k))
	}
	return nil
}

// revoke stops one key.
//
// A refusal from the history is passed on with its chain, because it names
// nothing about this machine and because whoever is revoking after a leak reads
// this line and stops looking.
func (o keyOptions) revoke(ctx context.Context, out io.Writer, st *store.Store) error {
	err := st.RevokeKey(ctx, o.ID)
	if errors.Is(err, store.ErrBadKey) {
		return err
	}
	if err != nil {
		return o.scrubbed(err)
	}
	_, _ = fmt.Fprintf(out, "key %d is stopped and will not be accepted again\n", o.ID)
	return nil
}

// lastUse says when a key was last accepted. A key that has never been used
// says so, rather than carrying a date from the zero of the calendar.
func lastUse(k store.APIKey) string {
	if k.LastUsedAt.IsZero() {
		return "never"
	}
	return k.LastUsedAt.Format(time.RFC3339)
}

// keyState says whether a key still works. Revoked keys stay in the listing,
// so the listing has to say which ones they are.
func keyState(k store.APIKey) string {
	if k.Revoked {
		return "revoked"
	}
	return "active"
}

// scrubbed rewrites an error so it can be printed.
//
// The chain goes with the path. Nothing above this command tells these errors
// apart, and a wrapped error would carry the original text — the very thing
// being taken out — along inside it.
func (o keyOptions) scrubbed(err error) error {
	return errors.New(scrubDB(err.Error(), o.DB))
}
