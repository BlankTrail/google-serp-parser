// SPDX-License-Identifier: MIT

package main

import (
	"path/filepath"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/semantic"
)

func TestServeOptions_KeepTheModelBesideTheHistory(t *testing.T) {
	dir := t.TempDir()
	o := serveOptions{DB: filepath.Join(dir, "gserp.db")}
	if got, want := o.modelPath(), filepath.Join(dir, semantic.FileName); got != want {
		t.Errorf("modelPath = %q, want %q", got, want)
	}
}
