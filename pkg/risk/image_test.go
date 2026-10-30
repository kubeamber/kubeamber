package risk

import (
	"strings"
	"testing"
)

func TestImageRepository(t *testing.T) {
	cases := map[string]string{
		"nginx":                                          "docker.io/library/nginx",
		"nginx:1.27":                                     "docker.io/library/nginx",
		"bitnami/redis:7":                                "docker.io/bitnami/redis",
		"docker.io/nginx":                                "docker.io/library/nginx",
		"index.docker.io/library/nginx:latest":           "docker.io/library/nginx",
		"cgr.dev/chainguard/nginx:latest":                "cgr.dev/chainguard/nginx",
		"cgr.dev/chainguard/static@sha256:fc42af":        "cgr.dev/chainguard/static",
		"cgr.dev/chainguard/static:latest@sha256:fc42af": "cgr.dev/chainguard/static",
		"ghcr.io/kubeamber/kubeamber:v0.2.0":             "ghcr.io/kubeamber/kubeamber",
		"localhost:5000/app:dev":                         "localhost:5000/app",
		"localhost/app":                                  "localhost/app",
		"registry.example.com:443/team/app":              "registry.example.com:443/team/app",
	}
	for ref, want := range cases {
		if got := imageRepository(ref); got != want {
			t.Errorf("imageRepository(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestImageFactors(t *testing.T) {
	chainguard := func(repo string) bool { return strings.HasPrefix(repo, "cgr.dev/chainguard/") }
	const pinned = "cgr.dev/chainguard/nginx:latest@sha256:fc42af33"
	cases := []struct {
		name       string
		change     Change
		opts       Options
		want       int
		wantReason string
	}{
		{
			name:   "off without spec.images",
			change: Change{Operation: "CREATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", Object: deployment("shop", 1, "nginx:latest")},
			want:   10,
		},
		{
			name:       "untrusted and unpinned image",
			change:     Change{Operation: "CREATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", Object: deployment("shop", 1, "nginx:latest")},
			opts:       Options{TrustedImage: chainguard, RequireDigest: true},
			want:       50,
			wantReason: "outside the trusted repositories (nginx:latest)",
		},
		{
			name:   "trusted, pinned image",
			change: Change{Operation: "CREATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", Object: deployment("shop", 1, pinned)},
			opts:   Options{TrustedImage: chainguard, RequireDigest: true},
			want:   10,
		},
		{
			name:       "trusted image on a tag only",
			change:     Change{Operation: "CREATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", Object: deployment("shop", 1, "cgr.dev/chainguard/nginx:latest")},
			opts:       Options{TrustedImage: chainguard, RequireDigest: true},
			want:       20,
			wantReason: "not pinned by digest (cgr.dev/chainguard/nginx:latest)",
		},
		{
			name:       "an agent swaps in an untrusted image",
			change:     Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", OldObject: deployment("shop", 3, pinned), Object: deployment("shop", 3, "evil.example.com/nginx:latest")},
			opts:       Options{TrustedImage: chainguard},
			want:       55,
			wantReason: "evil.example.com/nginx:latest",
		},
		{
			name:   "images the old spec already ran score nothing",
			change: Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", OldObject: deployment("shop", 3, "nginx:latest"), Object: deployment("shop", 4, "nginx:latest")},
			opts:   Options{TrustedImage: chainguard, RequireDigest: true},
			want:   15,
		},
		{
			name: "init and ephemeral containers count",
			change: Change{Operation: "CREATE", Kind: "Pod", Namespace: "shop", Name: "p",
				Object: map[string]any{"kind": "Pod", "spec": map[string]any{
					"initContainers":      []any{map[string]any{"name": "init", "image": "busybox"}},
					"containers":          []any{map[string]any{"name": "app", "image": pinned}},
					"ephemeralContainers": []any{map[string]any{"name": "debug", "image": "alpine"}},
				}}},
			opts:       Options{TrustedImage: chainguard},
			want:       35,
			wantReason: "(alpine, busybox)",
		},
		{
			name:   "deletes are not scored for images",
			change: Change{Operation: "DELETE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", OldObject: deployment("shop", 1, "nginx")},
			opts:   Options{TrustedImage: chainguard, RequireDigest: true},
			want:   35,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Assess(c.change, c.opts)
			if a.Score != c.want {
				t.Errorf("score = %d, want %d; factors: %+v", a.Score, c.want, a.Factors)
			}
			if c.wantReason != "" && !hasReason(a, c.wantReason) {
				t.Errorf("no factor mentions %q: %+v", c.wantReason, a.Factors)
			}
		})
	}
}
