package ui

import "testing"

func TestFirstOSC8URL(t *testing.T) {
	u := "https://github.com/org/repo/tree/feature"
	line := "  " + OSC8(u, "feature") + "  meta"
	if got := FirstOSC8URL(line); got != u {
		t.Fatalf("FirstOSC8URL: got %q want %q", got, u)
	}
	if FirstOSC8URL("no link here") != "" {
		t.Fatal("expected empty")
	}
}
