/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package docs checks the documentation as a set of files that point at each other: every
// relative link in the Markdown and on the site names a file that exists, and every #anchor a
// heading or an id that exists there. Links into the public repository
// (github.com/aivandrago/subnet-operator/blob/main/...) are checked against this checkout, and
// links to the site (hypersurgery.dev/...) against site/. Nothing here goes to the network.
package docs

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

const root = "../.."

// The prefixes under which a link names a file of this repository or of site/.
var (
	repoPrefixes = []string{
		"https://github.com/aivandrago/subnet-operator/blob/main/",
		"https://github.com/aivandrago/subnet-operator/tree/main/",
	}
	sitePrefix = "https://hypersurgery.dev/"
)

// sources are the files whose links are checked: all Markdown and the site's pages.
func sources(t *testing.T) []string {
	t.Helper()
	var out []string
	skip := []string{".git", "bin", "vendor", "node_modules", "bundle", "catalog", "dist"}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// hack/api-docs holds a fragment of docs/reference/api.md, whose links are
			// relative to where it ends up; they are checked there.
			if slices.Contains(skip, d.Name()) || rel == "hack/api-docs" || strings.HasSuffix(rel, "/testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(rel, ".md"):
			out = append(out, rel)
		case strings.HasPrefix(rel, "site/") && strings.HasSuffix(rel, ".html"):
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var (
	fence       = regexp.MustCompile("(?ms)^\\s*(```|~~~).*?^\\s*(```|~~~)\\s*$")
	inlineCode  = regexp.MustCompile("`[^`\n]*`")
	mdLink      = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	mdRefDef    = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]:\s*(\S+)`)
	htmlRef     = regexp.MustCompile(`(?i)\b(?:href|src)\s*=\s*"([^"]*)"`)
	htmlID      = regexp.MustCompile(`(?i)\b(?:id|name)\s*=\s*"([^"]+)"`)
	mdHeading   = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*#*\s*$`)
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	scriptBlock = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
)

// links returns the link targets of one file, without the ones inside code.
func links(rel, text string) []string {
	var out []string
	if strings.HasSuffix(rel, ".md") {
		text = fence.ReplaceAllString(text, "")
		text = inlineCode.ReplaceAllString(text, "")
		text = htmlComment.ReplaceAllString(text, "")
		for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
			out = append(out, m[1])
		}
		for _, m := range mdRefDef.FindAllStringSubmatch(text, -1) {
			out = append(out, m[1])
		}
	} else {
		text = scriptBlock.ReplaceAllString(text, "")
		text = htmlComment.ReplaceAllString(text, "")
	}
	for _, m := range htmlRef.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// slug is the anchor GitHub gives a Markdown heading.
func slug(heading string) string {
	heading = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`).ReplaceAllString(heading, "$1")
	heading = strings.ReplaceAll(heading, "`", "")
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// anchors returns the anchors one file defines.
func anchors(rel string, cache map[string]map[string]bool) (map[string]bool, error) {
	if a, ok := cache[rel]; ok {
		return a, nil
	}
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return nil, err
	}
	text := string(b)
	out := map[string]bool{}
	if strings.HasSuffix(rel, ".md") {
		seen := map[string]int{}
		for _, m := range mdHeading.FindAllStringSubmatch(fence.ReplaceAllString(text, ""), -1) {
			s := slug(m[1])
			if n := seen[s]; n > 0 {
				out[fmt.Sprintf("%s-%d", s, n)] = true
			} else {
				out[s] = true
			}
			seen[s]++
		}
	}
	for _, m := range htmlID.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	cache[rel] = out
	return out, nil
}

// resolve maps a link target found in the file from to a file of the checkout and an anchor.
// ok is false for links this test does not check (other sites, mailto:, data:).
func resolve(from, target string) (file, anchor string, ok bool) {
	target = strings.TrimSpace(target)
	if target == "" || strings.HasPrefix(target, "data:") || strings.HasPrefix(target, "mailto:") ||
		strings.HasPrefix(target, "javascript:") || strings.Contains(target, "{{") || strings.Contains(target, "${") {
		return "", "", false
	}
	for _, p := range repoPrefixes {
		if rest, found := strings.CutPrefix(target, p); found {
			return split(rest)
		}
	}
	if rest, found := strings.CutPrefix(target, sitePrefix); found {
		f, a, _ := split(rest)
		return sitePage(path.Join("site", f)), a, true
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "", "", false
	}
	f, a, _ := split(target)
	switch {
	case strings.HasPrefix(target, "#"):
		return from, a, true
	case strings.HasPrefix(target, "/") && strings.HasPrefix(from, "site/"):
		return sitePage(path.Join("site", f)), a, true
	case strings.HasPrefix(target, "/"):
		return strings.TrimPrefix(path.Clean("/"+f), "/"), a, true
	}
	file = path.Join(path.Dir(from), f)
	if strings.HasPrefix(from, "site/") {
		file = sitePage(file)
	}
	return file, a, true
}

func split(target string) (file, anchor string, ok bool) {
	file, anchor, _ = strings.Cut(target, "#")
	file, _, _ = strings.Cut(file, "?")
	file, _ = url.PathUnescape(file)
	return strings.TrimSuffix(file, "/"), anchor, true
}

// sitePage maps a site path to the file a static server answers it with.
func sitePage(p string) string {
	p = path.Clean(p)
	if st, err := os.Stat(filepath.Join(root, p)); err == nil && st.IsDir() {
		return path.Join(p, "index.html")
	}
	return p
}

func TestLinksAndAnchorsResolve(t *testing.T) {
	cache := map[string]map[string]bool{}
	files := sources(t)
	if len(files) < 20 {
		t.Fatalf("found only %d documents; is root right?", len(files))
	}
	checked := 0
	for _, from := range files {
		b, err := os.ReadFile(filepath.Join(root, from))
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range links(from, string(b)) {
			file, anchor, ok := resolve(from, target)
			if !ok {
				continue
			}
			checked++
			if file == "" || strings.HasPrefix(file, "..") {
				t.Errorf("%s: link %q leaves the repository", from, target)
				continue
			}
			if _, err := os.Stat(filepath.Join(root, file)); err != nil {
				t.Errorf("%s: link %q: %s does not exist", from, target, file)
				continue
			}
			if anchor == "" || (!strings.HasSuffix(file, ".md") && !strings.HasSuffix(file, ".html")) {
				continue
			}
			a, err := anchors(file, cache)
			if err != nil {
				t.Errorf("%s: link %q: %v", from, target, err)
				continue
			}
			if !a[anchor] {
				t.Errorf("%s: link %q: %s has no anchor #%s", from, target, file, anchor)
			}
		}
	}
	if checked < 200 {
		t.Errorf("checked only %d links; is the link pattern right?", checked)
	}
	t.Logf("checked %d links in %d files", checked, len(files))
}

func TestSlugMatchesGitHub(t *testing.T) {
	for heading, want := range map[string]string{
		"Upgrading from 0.9 to 1.0":                 "upgrading-from-09-to-10",
		"`SubnetClaim` stuck in `Allocated`":        "subnetclaim-stuck-in-allocated",
		"Values and flags removed in 1.0":           "values-and-flags-removed-in-10",
		"What the operator does on its first start": "what-the-operator-does-on-its-first-start",
		"Publishing on OperatorHub.io":              "publishing-on-operatorhubio",
		"A — B, and [a link](x.md)":                 "a--b-and-a-link",
	} {
		if got := slug(heading); got != want {
			t.Errorf("slug(%q) = %q, want %q", heading, got, want)
		}
	}
}
