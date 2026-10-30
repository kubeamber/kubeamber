package risk

import (
	"sort"
	"strings"
)

// imageFactors scores the images that a new or changed pod spec adds: from a
// repository outside the trusted ones, or not pinned by digest. Images the
// old spec already ran score nothing, so unrelated updates aren't penalised.
// Both factors are off unless the policy sets spec.images.
func imageFactors(a *Assessment, c Change, opts Options) {
	if opts.TrustedImage == nil && !opts.RequireDigest {
		return
	}
	if c.Operation != "CREATE" && c.Operation != "UPDATE" {
		return
	}
	before := map[string]bool{}
	for _, img := range images(podSpec(c.OldObject)) {
		before[img] = true
	}
	var untrusted, unpinned []string
	for _, img := range images(podSpec(c.Object)) {
		if before[img] {
			continue
		}
		if opts.TrustedImage != nil && !opts.TrustedImage(imageRepository(img)) {
			untrusted = append(untrusted, img)
		}
		// A digest is the only part of a reference that contains '@'.
		if opts.RequireDigest && !strings.Contains(img, "@") {
			unpinned = append(unpinned, img)
		}
	}
	if len(untrusted) > 0 {
		a.add(30, "runs images from outside the trusted repositories (%s)", strings.Join(untrusted, ", "))
	}
	if len(unpinned) > 0 {
		a.add(10, "runs images not pinned by digest (%s)", strings.Join(unpinned, ", "))
	}
}

// images lists the distinct images of a pod spec's containers.
func images(ps map[string]any) []string {
	var out []string
	for _, ctr := range containers(ps) {
		if img, _ := ctr["image"].(string); img != "" {
			out = append(out, img)
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

// imageRepository returns the repository of an image reference, without tag
// or digest and with Docker Hub's defaults made explicit, so policies can
// match it: "nginx:1.27" is docker.io/library/nginx, "bitnami/redis" is
// docker.io/bitnami/redis, "ghcr.io/org/app@sha256:..." is ghcr.io/org/app.
func imageRepository(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	host, path, ok := strings.Cut(name, "/")
	switch {
	case !ok:
		return "docker.io/library/" + name
	case host == "docker.io" || host == "index.docker.io":
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}
		return "docker.io/" + path
	case strings.ContainsAny(host, ".:") || host == "localhost":
		return name
	}
	return "docker.io/" + name
}
