// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"testing"
)

func TestGatewayChannel_HandsOutEveryGatewayInTurn(t *testing.T) {
	// The same rule the addresses follow, and for the same reason: a gateway
	// handed out twice while another has not been used at all is a set being
	// worked like a smaller one.
	names := []string{"WiseKeys.DE", "WiseKeys.EE", "WiseKeys.ES"}
	ch := NewGatewayListChannel("gateways", NewStaticRotor(GatewayUpstreams(names)))

	seen := map[string]int{}
	for range names {
		eg, ok := ch.Next()
		if !ok {
			t.Fatal("the channel ran out of gateways inside one turn of the list")
		}
		if eg.Gateway == "" {
			t.Fatalf("what came back is not a gateway: %+v", eg)
		}
		if eg.Upstream != "" {
			t.Errorf("a gateway came back carrying a proxy address as well: %+v", eg)
		}
		seen[eg.Gateway]++
	}
	for _, name := range names {
		if seen[name] != 1 {
			t.Errorf("%s was handed out %d times in one turn, want once", name, seen[name])
		}
	}
}

func TestGatewayChannel_PutsAFailingGatewayAsideAndSaysHowMany(t *testing.T) {
	// A gateway that does not carry anything is banned the way an address is,
	// and the proxy screen counts it the same way — it is the same question:
	// how much of what was chosen is still usable.
	names := []string{"one", "two", "three", "four"}
	r := NewStaticRotor(GatewayUpstreams(names))
	ch := NewGatewayListChannel("gateways", r)

	counted, ok := ch.(interface {
		Len() int
		Resting() int
		ReleaseAll() int
	})
	if !ok {
		t.Fatal("the proxy screen cannot count this channel")
	}
	if got := counted.Len(); got != len(names) {
		t.Errorf("the channel holds %d gateways, want %d", got, len(names))
	}

	ch.MarkDead(Egress{Gateway: "two"})
	if got := counted.Resting(); got != 1 {
		t.Errorf("%d gateways are resting after one was found dead, want one", got)
	}

	// And what is handed out afterwards is never the one put away.
	for range 6 {
		eg, ok := ch.Next()
		if !ok {
			t.Fatal("the channel has nothing to hand out")
		}
		if eg.Gateway == "two" {
			t.Error("a gateway that was put away was handed out again")
		}
	}

	if got := counted.ReleaseAll(); got != 1 {
		t.Errorf("%d gateways came back off the bench, want one", got)
	}
	if got := counted.Resting(); got != 0 {
		t.Errorf("%d gateways are still resting after the bench was let go", got)
	}
}

func TestGatewayChannel_RenewMovesToAnotherGateway(t *testing.T) {
	// Rotation for a gateway is the channel handing out the next one. What the
	// pool does with it — closing the port and opening it again, because a
	// gateway cannot be swapped on a live one — is the pool's business.
	ch := NewGatewayListChannel("gateways", NewStaticRotor(GatewayUpstreams([]string{"a", "b"})))
	first, err := ch.Renew(context.Background(), Egress{})
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	second, err := ch.Renew(context.Background(), first)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if first.Gateway == second.Gateway {
		t.Errorf("renewing handed back %q twice", first.Gateway)
	}
}

func TestGatewayUpstreams_KeepsTheNameAndDropsNothingElse(t *testing.T) {
	// The name is the whole of a gateway, and an empty one is not a gateway.
	ups := GatewayUpstreams([]string{"one", "", "two"})
	if len(ups) != 2 {
		t.Fatalf("%d entries were made from three names, one of them empty", len(ups))
	}
	for _, u := range ups {
		if u.Scheme != gatewayScheme {
			t.Errorf("an entry carries scheme %q, want %q", u.Scheme, gatewayScheme)
		}
		if u.Host == "" {
			t.Error("an entry carries no name")
		}
	}
	// Two gateways are two egresses, which is what the limit on threads per
	// egress counts.
	if a, b := (Egress{Gateway: "one"}).String(), (Egress{Gateway: "two"}).String(); a == b {
		t.Error("two gateways read as one egress")
	}
}
