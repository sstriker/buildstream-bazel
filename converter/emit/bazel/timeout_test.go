package bazel

import (
	"testing"
	"time"
)

// TestFormatBazelTimeout pins the CTest TIMEOUT (seconds) -> Bazel test-rule
// `timeout` ENUM mapping (issue #314): Bazel rejects a duration string like
// "120s" on a test rule's timeout attribute, requiring short/moderate/long/
// eternal.
func TestFormatBazelTimeout(t *testing.T) {
	cases := []struct {
		secs int
		want string
	}{
		{0, "short"}, {1, "short"}, {60, "short"},
		{61, "moderate"}, {120, "moderate"}, {300, "moderate"},
		{301, "long"}, {900, "long"},
		{901, "eternal"}, {3600, "eternal"}, {100000, "eternal"},
	}
	for _, c := range cases {
		if got := formatBazelTimeout(time.Duration(c.secs) * time.Second); got != c.want {
			t.Errorf("formatBazelTimeout(%ds) = %q; want %q", c.secs, got, c.want)
		}
	}
}
