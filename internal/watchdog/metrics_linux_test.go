//go:build linux

package watchdog

import (
	"math"
	"testing"
	"time"
)

func TestLinuxCollector(t *testing.T) {
	c := &Collector{}
	first := c.Collect([]string{"/", "/watchdog-path-that-does-not-exist"})
	if len(first.Errors) == 0 {
		t.Fatal("missing mount error not reported")
	}
	time.Sleep(30 * time.Millisecond)
	s := c.Collect([]string{"/"})
	if len(s.Errors) > 0 {
		t.Fatal(s.Errors)
	}
	seen := map[string]bool{}
	for _, m := range s.Metrics {
		seen[m.Kind] = true
		if math.IsNaN(m.Value) || m.Value < 0 || m.Value > 100 {
			t.Fatal(m)
		}
	}
	for _, kind := range []string{"cpu", "ram", "swap", "disk"} {
		if !seen[kind] {
			t.Fatal("missing metric", kind)
		}
	}
}
