package cli

import (
	"strings"
	"testing"
)

func TestSplitTrim(t *testing.T) {
	got := splitTrim(" a, ,b ,, c ")
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("splitTrim broken: %q", got)
	}
	if len(splitTrim("")) != 0 || len(splitTrim(" , ")) != 0 {
		t.Fatal("empty input must yield nothing")
	}
}

func TestMaskAPIKey(t *testing.T) {
	masked := maskAPIKey("sk-abcdef1234567890")
	if strings.Contains(masked, "cdef1234") || !strings.Contains(masked, "sk-") {
		t.Fatalf("middle must be hidden: %q", masked)
	}
	if maskAPIKey("") == "" {
		t.Fatal("short input must be fully masked, never leaked")
	}
}
