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
)

// ErrChecksum is a download that does not hash to what the release says.
var ErrChecksum = errors.New("semantic: the downloaded model does not hash to what it should")

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
// progress, when not nil, is told how many bytes have arrived and how many are
// expected (negative when the server did not say).
func Fetch(ctx context.Context, client *http.Client, url, sum, path string, progress func(done, total int64)) (err error) {
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
	if _, err = io.Copy(io.MultiWriter(f, h, counted), resp.Body); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return ErrChecksum
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(part, path)
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
