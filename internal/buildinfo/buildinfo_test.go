package buildinfo

import "testing"

func TestResolveCommit(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct{ name, injected, vcs, want string }{
		{"injected wins", full, "abcdef1", full},
		{"short sha", "abcdef1", "", "abcdef1"},
		{"default falls back to vcs", Unknown, "abcdef1234", "abcdef1234"},
		{"nothing available", Unknown, "", Unknown},
		{"empty injected", "", "", Unknown},
		{"uppercase refused", "ABCDEF1234", "", Unknown},
		{"too short refused", "abc12", "", Unknown},
		{"too long refused", full + full, "", Unknown},
		{"shell or log injection refused", "abcdef1; rm -rf /", "", Unknown},
		{"newline refused", "abcdef1\nINFO fake", "", Unknown},
		{"bad injected still uses valid vcs", "not a sha", "abcdef1234", "abcdef1234"},
		{"bad vcs ignored", Unknown, "zzzzzzz", Unknown},
	}
	for _, c := range cases {
		if got := resolveCommit(c.injected, c.vcs); got != c.want {
			t.Errorf("%s: resolveCommit(%q, %q) = %q, want %q", c.name, c.injected, c.vcs, got, c.want)
		}
	}
}

func TestCommitIDIsAlwaysValidOrUnknown(t *testing.T) {
	got := CommitID()
	if got != Unknown && !commitPattern.MatchString(got) {
		t.Fatalf("CommitID() = %q", got)
	}
}
