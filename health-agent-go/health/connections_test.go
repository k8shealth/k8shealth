package health

import "testing"

func TestNormalizeLocalPort(t *testing.T) {
	cases := map[int]int{
		0:     0,
		80:    80,
		8080:  8080,
		9999:  9999,
		29999: 29999,
		30000: 0,
		32768: 0,
		60999: 0,
		65535: 0,
	}
	for in, want := range cases {
		if got := normalizeLocalPort(in); got != want {
			t.Errorf("normalizeLocalPort(%d) = %d, want %d", in, got, want)
		}
	}
}

// Many outbound connections to the same dependency (different ephemeral local ports) must map to
// one key, otherwise the 24 h union grows without bound.
func TestOutboundConnectionsCollapseToOneKey(t *testing.T) {
	seen := map[connKey]struct{}{}
	for p := 32768; p < 32768+5000; p++ {
		seen[connKey{remote: "10.0.0.5:3306", localPort: normalizeLocalPort(p)}] = struct{}{}
	}
	if len(seen) != 1 {
		t.Fatalf("expected 1 key for the same remote, got %d", len(seen))
	}
}

// Inbound connections keep their (low) server port so the manager can still classify them.
func TestInboundServerPortKept(t *testing.T) {
	if got := normalizeLocalPort(8082); got != 8082 {
		t.Fatalf("server port must be kept, got %d", got)
	}
}
