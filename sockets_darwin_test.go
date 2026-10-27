package portkit

import (
	"slices"
	"testing"
)

func TestParseLsof(t *testing.T) {
	out := "p4242\nf20\ntIPv4\nn127.0.0.1:3000\nf21\ntIPv6\nn[::1]:5173\nf22\ntIPv6\nn*:8000\nf23\ntIPv4\nn*:9229\np4243\nf5\ntIPv4\nn10.0.0.2:4000\n"
	want := []Socket{{"127.0.0.1", 3000}, {"::1", 5173}, {"::", 8000}, {"0.0.0.0", 9229}, {"10.0.0.2", 4000}}
	if got := parseLsof([]byte(out)); !slices.Equal(got, want) {
		t.Errorf("parseLsof = %v, want %v", got, want)
	}
}
