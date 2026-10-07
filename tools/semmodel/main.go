// SPDX-License-Identifier: MIT

// Command semmodel converts a model2vec snapshot into the one file the program
// downloads: go run ./tools/semmodel <snapshot dir> <out file>. It prints the
// file's size and SHA-256, which go into internal/semantic/release.go.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/blanktrail/google-serp-parser/internal/semantic"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: semmodel <snapshot dir> <out file>")
		os.Exit(2)
	}
	r, err := convert(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s: %d bytes, %d pieces read back of the snapshot's %d, sha256 %s\n", os.Args[2], r.size, r.pieces, r.snapshot, r.sum)
}

// converted is what convert reports of the file it wrote.
type converted struct {
	size             int64
	pieces, snapshot int
	sum              string
}

// convert writes the model of the snapshot in src to out. It writes to a
// temporary file beside out and renames it only once the whole file is written
// and has been read back by the same Load the program uses: a conversion that
// fails part way, or writes a file the program would refuse, leaves no file at
// out and leaves a previous one there untouched, rather than a half-written
// model whose hash would then be copied into release.go and published.
func convert(src, out string) (r converted, err error) {
	tmp := out + ".part"
	pieces, err := writeModel(src, tmp)
	if err != nil {
		return r, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	// The snapshot's float32 table, four times the size of the file, is out of
	// scope by now, so the read-back does not hold both in memory at once.
	//
	// The count printed is the one read back, beside the snapshot's own, for
	// the person converting to compare: Write and Load are tested to agree, so
	// a check of one against the other here could never fire in a test.
	m, err := semantic.Load(tmp)
	if err != nil {
		return r, fmt.Errorf("the written model does not read back: %w", err)
	}
	r.pieces, r.snapshot = m.Pieces(), pieces
	f, err := os.Open(tmp)
	if err != nil {
		return r, err
	}
	h := sha256.New()
	r.size, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		return r, err
	}
	r.sum = hex.EncodeToString(h.Sum(nil))
	return r, os.Rename(tmp, out)
}

// writeModel writes the model of the snapshot in src to tmp and reports how
// many pieces it has. A failure removes what was written.
func writeModel(src, tmp string) (pieces int, err error) {
	v, charsmap, rows, median, err := semantic.ReadSource(src)
	if err != nil {
		return 0, err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	err = semantic.Write(f, v, charsmap, rows, median)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return len(v.Pieces), nil
}
