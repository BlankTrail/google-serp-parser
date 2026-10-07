// SPDX-License-Identifier: MIT

package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ErrChecksum is a download that does not hash to what the release says.
var ErrChecksum = errors.New("semantic: the downloaded model does not hash to what it should")

// ErrTooLarge is a download that goes on past the size the release gives.
var ErrTooLarge = errors.New("semantic: the download is larger than the model")

// rename puts the finished download in place, and renameTries and renamePause
// are how often and how far apart it is tried. On Windows a rename over a file
// fails while anything holds that file open without sharing its deletion, and
// for a moment something often does: an antivirus scanning the old model, an
// indexer, a backup. Those let go within a second, so the rename is tried a few
// times before the download is given up for it. A test replaces rename to fail
// on cue, since a lock like that cannot be made to happen on demand.
var (
	rename      = os.Rename
	renameTries = 5
	renamePause = 200 * time.Millisecond
)

// Fetch downloads the model to path. It is written beside it first and put in
// place only once it hashes right, so a download that breaks off, or a server
// that sends something else, leaves whatever was there before untouched.
//
// The hash is checked here and not left to Load: Load only knows whether the
// bytes are shaped like a model, and a file that is shaped like one but is not
// the one the release published is a file nobody has measured the parity of.
// A body cut short needs no check of its own: net/http reports one that ends
// before its Content-Length as an error out of the copy, and one with no
// Content-Length that is cut off hashes wrong.
//
// size is how many bytes the model is. The body is read to one byte past it and
// no further: a server that keeps sending would otherwise fill the disk before
// the hash had its say, which happens only at the end. One byte more than size
// is a download that is not the model, and is refused as soon as it arrives.
//
// progress, when not nil, is told how many bytes have arrived and how many are
// expected (negative when the server did not say).
func Fetch(ctx context.Context, client *http.Client, url, sum string, size int64, path string, progress func(done, total int64)) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("semantic: downloading the model: %s", resp.Status)
	}
	part := path + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(part)
		}
	}()
	h := sha256.New()
	counted := &counter{total: resp.ContentLength, progress: progress}
	n, err := io.Copy(io.MultiWriter(f, h, counted), io.LimitReader(resp.Body, size+1))
	if err != nil {
		return err
	}
	if n > size {
		return ErrTooLarge
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return ErrChecksum
	}
	if err = f.Close(); err != nil {
		return err
	}
	for try := 1; ; try++ {
		if err = rename(part, path); err == nil || try >= renameTries {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(renamePause):
		}
	}
}

// counter tells how much has arrived.
type counter struct {
	done, total int64
	progress    func(done, total int64)
}

func (c *counter) Write(p []byte) (int, error) {
	c.done += int64(len(p))
	if c.progress != nil {
		c.progress(c.done, c.total)
	}
	return len(p), nil
}
