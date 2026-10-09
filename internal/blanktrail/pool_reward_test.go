// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// A quarantine takes weight from the port's channel, and a request the channel
// carries has to give it back: Reward was there and nothing called it, so a pool
// of one channel — the whole proxy list — was demoted for good after its fourth
// quarantine and never opened another port. The ports the hidden addresses are
// read through grow only by opening, so a position check of 10 000 titles stood
// with every thread waiting for a lookup port that could no longer be opened
// (jobs 22 and 9 of 2026-10-09, 7 810 and 713 titles in).
func TestPool_AChannelEarnsBackWhatQuarantinesTookByCarrying(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ch := cfg.Channels[0]

	for i := 0; i < initialWeight; i++ {
		p.mixer.Penalise(ch)
	}
	if err := p.Grow(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "no usable egress channel") {
		t.Fatalf("Grow with the only channel demoted to nothing = %v, want it refused", err)
	}

	// The port it still has answers.
	p.attemptSucceeded(p.ports[0].num)

	if w := p.mixer.Weight(ch); w < 1 {
		t.Fatalf("weight after the channel carried a request = %d, want it back above nought", w)
	}
	if err := p.Grow(context.Background(), 1); err != nil {
		t.Errorf("Grow after the channel carried a request: %v", err)
	}
}
