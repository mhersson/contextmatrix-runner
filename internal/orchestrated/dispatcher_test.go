package orchestrated

import "testing"

func TestSlugFromRepoURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"https with .git", "https://github.com/acme/auth-svc.git", "auth-svc"},
		{"https without .git", "https://github.com/acme/auth-svc", "auth-svc"},
		{"ssh github", "git@github.com:acme/auth-svc.git", "auth-svc"},
		{"trailing slash", "https://gitea.example/owner/repo/", "repo"},
		{"deeper path with .git", "https://gitea.example/owner/group/repo.git", "repo"},
		{"single path segment", "https://example.com/repo", "repo"},
		{"empty", "", ""},
		{"slug with uppercase becomes lowercase", "https://example.com/Owner/Auth-SVC.git", "auth-svc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := slugFromRepoURL(tc.url)
			if got != tc.want {
				t.Fatalf("slugFromRepoURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
