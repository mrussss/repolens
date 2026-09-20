package indexing

import "testing"

func TestResolveCommitFromLsRemote(t *testing.T) {
	const commit = "0123456789012345678901234567890123456789"
	const tagObject = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	output := "" +
		commit + "\trefs/heads/main\n" +
		tagObject + "\trefs/tags/v2.2.0\n" +
		commit + "\trefs/tags/v2.2.0^{}\n"

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "branch", ref: "main", want: commit},
		{name: "lightweight tag", ref: "v2.2.0", want: commit},
		{name: "annotated tag peeled", ref: "refs/tags/v2.2.0", want: commit},
		{name: "full sha", ref: commit, want: commit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveCommitFromLsRemote(output, tt.ref); got != tt.want {
				t.Fatalf("resolveCommitFromLsRemote(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestResolveCommitFromLsRemoteUnknownRef(t *testing.T) {
	if got := resolveCommitFromLsRemote("0123456789012345678901234567890123456789\trefs/heads/main\n", "missing"); got != "" {
		t.Fatalf("unknown ref resolved to %q", got)
	}
}
