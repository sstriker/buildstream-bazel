package lower

import "testing"

// TestCmakeSoname pins the soname derivation against cmake's actual rules
// (verified empirically: SOVERSION wins over VERSION; VERSION-only uses the
// full version; neither => no explicit soname). base is taken up to the first
// ".so" so a fully-versioned NameOnDisk (libbrotlidec.so.1.2.0) still yields
// the SOVERSION-form soname (libbrotlidec.so.1).
func TestCmakeSoname(t *testing.T) {
	cases := []struct {
		name          string
		sharedLibName string
		soversion     string
		version       string
		want          string
	}{
		{"soversion wins over version", "libgreet.so.1", "1", "1.2.3", "libgreet.so.1"},
		{"version only uses full version", "libgreet.so.1.2.3", "", "1.2.3", "libgreet.so.1.2.3"},
		{"soversion only", "libgreet.so.2", "2", "", "libgreet.so.2"},
		{"neither => no soname", "libgreet.so", "", "", ""},
		{"fully-versioned nameOnDisk uses soversion", "libbrotlidec.so.1.2.0", "1", "1.2.0", "libbrotlidec.so.1"},
		{"whitespace trimmed", "libz.so.1", "  1  ", "", "libz.so.1"},
		{"no .so in name => empty", "libgreet", "1", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cmakeSoname(tc.sharedLibName, tc.soversion, tc.version); got != tc.want {
				t.Errorf("cmakeSoname(%q, %q, %q) = %q, want %q",
					tc.sharedLibName, tc.soversion, tc.version, got, tc.want)
			}
		})
	}
}
