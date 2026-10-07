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
	v, charsmap, rows, median, err := semantic.ReadSource(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f, err := os.Create(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	h := sha256.New()
	if err := semantic.Write(io.MultiWriter(f, h), v, charsmap, rows, median); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st, err := os.Stat(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s: %d bytes, %d pieces, sha256 %s\n", os.Args[2], st.Size(), len(v.Pieces), hex.EncodeToString(h.Sum(nil)))
}
