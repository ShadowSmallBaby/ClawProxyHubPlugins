package main

import "testing"

func TestVersionFromUA(t *testing.T) {
	cases := []struct{ ua, want string }{
		{"WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1", "5.5.4"},
		{"WorkBuddy/6.0.0", "6.0.0"},
		{"MyClient/2.1.30 (Windows)", "2.1.30"},
		{"no-version-header", ""},
	}
	for _, c := range cases {
		if got := versionFromUA(c.ua); got != c.want {
			t.Fatalf("versionFromUA(%q) = %q, want %q", c.ua, got, c.want)
		}
	}
}
