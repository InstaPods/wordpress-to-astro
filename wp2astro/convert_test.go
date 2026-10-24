package wp2astro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// The WordPress to Astro tests run against a fixture recorded from the public
// REST API of wordpress.org/news (testdata/wp-rest, see meta.json), with its
// hosts rewritten to wp.example. Nothing here touches the network: the test
// transport sends every request, whatever its host, to a local server that
// answers from the fixture files.

// wpFixture serves the recorded site.
type wpFixture struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	requests []string // "host path?query" of every request
	images   []string // "host path" of every image request
	// collections overrides a fixture file, e.g. for a generated site.
	collections map[string][]map[string]any
	index       map[string]any
	types       map[string]any
	comments    int
	// plain serves the API only at ?rest_route= and advertises nothing,
	// the way a site with plain permalinks and a stripped <head> looks.
	plain bool
	// restOff answers every API request with 404, like a security plugin.
	restOff bool
	// homeHTML is what the site's front page returns.
	homeHTML string
	// rootSite serves a second, empty WordPress at the domain root
	// (/wp-json/), the way wordpress.org does under its /news/ blog.
	rootSite bool
	// noAdvert drops the Link header (a stripped <head> is up to homeHTML).
	noAdvert bool
	// subdirOff answers 404 for the /news/ install's API.
	subdirOff bool
	// throttle429 answers that many /news/ API requests with HTTP 429.
	throttle429 int
	// cdn429 answers every Jetpack CDN (i0.wp.com) image with HTTP 429.
	cdn429 bool
}

func (f *wpFixture) requested(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func loadFixtureJSON(t *testing.T, name string, out any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "wp-rest", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
}

// newRecordedFixture loads the wordpress.org/news recording.
func newRecordedFixture(t *testing.T) *wpFixture {
	f := &wpFixture{t: t, collections: map[string][]map[string]any{}}
	for _, name := range []string{"posts", "pages", "categories", "tags", "media"} {
		var items []map[string]any
		loadFixtureJSON(t, name+".json", &items)
		f.collections[name] = items
	}
	loadFixtureJSON(t, "index.json", &f.index)
	loadFixtureJSON(t, "types.json", &f.types)
	var meta struct {
		Comments int `json:"comments_total"`
	}
	loadFixtureJSON(t, "meta.json", &meta)
	f.comments = meta.Comments
	// The hand-added "product" type (see meta.json) has two public items.
	f.collections["product"] = []map[string]any{{"id": 1}, {"id": 2}}
	f.homeHTML = `<!doctype html><html><head><link rel="https://api.w.org/" href="http://wp.example/news/wp-json/" />
<link rel="stylesheet" href="http://wp.example/news/wp-content/themes/news/style.css"></head><body>News</body></html>`
	return f
}

func (f *wpFixture) start() *Converter {
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(f.srv.Close)
	target, _ := url.Parse(f.srv.URL)
	return &Converter{
		Client: &http.Client{Transport: &fixtureTransport{target: target}},
		// The limits the hosted converter runs under, which the budget tests
		// rely on, with a short 429 backoff.
		Limits: Limits{
			DefaultPosts: 500, MaxPosts: 1000, MaxPages: 200, MaxTerms: 1000,
			JSONBytes: 24 << 20, ImageBytes: 40 << 20, ImageFileBytes: 8 << 20,
			Timeout: 70 * time.Second, MaxRequests: 300,
			MaxRetries: 2, RetryBackoff: time.Millisecond, MaxRetryWait: 3 * time.Second,
		},
		PreviewLimits: Limits{
			JSONBytes: 4 << 20, Timeout: 20 * time.Second, MaxRequests: 25,
			MaxRetries: 2, RetryBackoff: time.Millisecond, MaxRetryWait: 3 * time.Second,
		},
	}
}

// fixtureTransport sends every request to the fixture server and keeps the
// original host in the Host header, so the importer sees the URLs it asked for.
type fixtureTransport struct{ target *url.URL }

func (tr *fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = tr.target.Scheme
	out.URL.Host = tr.target.Host
	out.Host = req.URL.Host
	resp, err := http.DefaultTransport.RoundTrip(out)
	if resp != nil {
		resp.Request = req
	}
	return resp, err
}

var fixtureImageExt = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

func (f *wpFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Host+" "+r.URL.RequestURI())
	f.mu.Unlock()

	if ct, ok := fixtureImageExt[strings.ToLower(filepath.Ext(r.URL.Path))]; ok {
		if f.cdn429 && r.Host == "i0.wp.com" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		f.mu.Lock()
		f.images = append(f.images, r.Host+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", ct)
		fmt.Fprintf(w, "IMG:%s%s", r.Host, r.URL.Path)
		return
	}

	if f.rootSite && (strings.HasPrefix(r.URL.Path, "/wp-json") || (r.URL.Path == "/" && r.URL.Query().Has("rest_route"))) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		route := strings.TrimPrefix(r.URL.Path, "/wp-json")
		if q := r.URL.Query().Get("rest_route"); q != "" {
			route = q
		}
		if route == "/" || route == "" {
			fmt.Fprint(w, `{"name":"Root Site","description":"","url":"http://wp.example","home":"http://wp.example","namespaces":["wp/v2"]}`)
			return
		}
		w.Header().Set("X-WP-Total", "0")
		w.Header().Set("X-WP-TotalPages", "0")
		fmt.Fprint(w, `[]`)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/news/wp-json") || (strings.HasPrefix(r.URL.Path, "/news") && r.URL.Query().Has("rest_route")) {
		if f.subdirOff {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":"rest_no_route"}`)
			return
		}
		f.mu.Lock()
		throttle := f.throttle429 > 0
		if throttle {
			f.throttle429--
		}
		f.mu.Unlock()
		if throttle {
			// What wordpress.org's nginx sends: HTML, no Retry-After.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "<html><body><h1>429 Too Many Requests</h1></body></html>")
			return
		}
	}

	route := ""
	switch {
	case r.URL.Query().Has("rest_route"):
		route = r.URL.Query().Get("rest_route")
	case strings.Contains(r.URL.Path, "/wp-json"):
		if f.plain {
			http.NotFound(w, r)
			return
		}
		route = r.URL.Path[strings.Index(r.URL.Path, "/wp-json")+len("/wp-json"):]
		if route == "" {
			route = "/"
		}
	}
	if route == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if !f.plain && !f.restOff && !f.noAdvert {
			w.Header().Add("Link", `<http://wp.example/news/wp-json/>; rel="https://api.w.org/"`)
		}
		fmt.Fprint(w, f.homeHTML)
		return
	}
	if f.restOff {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"rest_no_route","message":"No route was found matching the URL and request method."}`)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	switch route {
	case "/":
		json.NewEncoder(w).Encode(f.index)
		return
	case "/wp/v2/types":
		json.NewEncoder(w).Encode(f.types)
		return
	case "/wp/v2/comments":
		w.Header().Set("X-WP-Total", strconv.Itoa(f.comments))
		w.Header().Set("X-WP-TotalPages", strconv.Itoa(f.comments))
		fmt.Fprint(w, `[{"id":1}]`)
		return
	}
	name := strings.TrimPrefix(route, "/wp/v2/")
	items, ok := f.collections[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"rest_no_route"}`)
		return
	}
	q := r.URL.Query()
	if inc := q.Get("include"); inc != "" {
		want := map[string]bool{}
		for _, id := range strings.Split(inc, ",") {
			want[id] = true
		}
		var kept []map[string]any
		for _, it := range items {
			if want[fmt.Sprint(it["id"])] {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	if perPage <= 0 {
		perPage = 10
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	totalPages := (len(items) + perPage - 1) / perPage
	if page > 1 && page > totalPages {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":"rest_post_invalid_page_number"}`)
		return
	}
	lo, hi := (page-1)*perPage, page*perPage
	if hi > len(items) {
		hi = len(items)
	}
	w.Header().Set("X-WP-Total", strconv.Itoa(len(items)))
	w.Header().Set("X-WP-TotalPages", strconv.Itoa(totalPages))
	if lo >= len(items) {
		fmt.Fprint(w, `[]`)
		return
	}
	json.NewEncoder(w).Encode(items[lo:hi])
}

func artifactFile(t *testing.T, a *Project, p string) string {
	t.Helper()
	var have []string
	for _, f := range a.Files {
		if f.Path == p {
			return f.Content
		}
		have = append(have, f.Path)
	}
	t.Fatalf("no file %q in project; have %v", p, have)
	return ""
}

func mentions(items []SummaryItem, needle string) bool {
	needle = strings.ToLower(needle)
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Label+" "+it.Detail), needle) {
			return true
		}
	}
	return false
}

func TestWordPressAstroConvertsRecordedSite(t *testing.T) {
	fx := newRecordedFixture(t)
	w := fx.start()
	a, err := w.Convert(context.Background(), "http://wp.example/news/", Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	r := a.Report
	if r == nil || a.Host != "wp.example" || a.SourceURL != "http://wp.example/news/" {
		t.Fatalf("project = %+v", a)
	}
	if r.Found.Posts != 4 || r.Found.Pages != 2 || r.Converted.Posts != 4 || r.Converted.Pages != 2 {
		t.Errorf("counts: found %+v converted %+v", r.Found, r.Converted)
	}
	if r.Found.Comments != 234 {
		t.Errorf("comments = %d, want 234 from the recording", r.Found.Comments)
	}
	if !contains(r.CustomTypes, "Products (2)") {
		t.Errorf("custom types = %v, want the fixture's Products", r.CustomTypes)
	}
	if r.Site.Name != "WordPress News" || r.Site.Home != "http://wp.example/news" || !r.Site.PrettyPermalinks {
		t.Errorf("site = %+v", r.Site)
	}

	// Posts keep their exact WordPress path.
	post := artifactFile(t, a, "src/content/posts/critical-php-security-release.md")
	for _, want := range []string{
		`title: "Critical PHP Security Release"`,
		`permalink: "/news/2026/10/critical-php-security-release/"`,
		`categories: ["Security"]`,
		`slug: "critical-php-security-release"`,
		`wordpressId: 21835`,
		"\n> Where immediate remediation is not feasible", // blockquote
		"\n- [Get a faster, more secure website: update PHP today](https://wordpress.org/support/update-php/)",
	} {
		if !strings.Contains(post, want) {
			t.Errorf("converted post is missing %q\n%s", want, post)
		}
	}
	if r.Permalinks.Kept != 6 || r.Permalinks.Redirected != 0 {
		t.Errorf("permalinks = %+v, want all 6 kept", r.Permalinks)
	}

	// Jetpack CDN images: fetched from the CDN's resized copy, saved at the
	// original path, and the post points at the local copy.
	beta := artifactFile(t, a, "src/content/posts/wordpress-7-1-beta-4.md")
	if strings.Contains(beta, "i0.wp.com/wp.example") {
		t.Errorf("a same-site image still points at the Jetpack CDN:\n%s", beta)
	}
	sawPhoton := false
	for _, img := range fx.images {
		if strings.HasPrefix(img, "i0.wp.com /wp.example/news/files/") {
			sawPhoton = true
		}
	}
	if !sawPhoton {
		t.Errorf("expected an image download from the CDN's resized copy, got %v", fx.images)
	}
	if !strings.Contains(beta, `featuredImage: "/news/files/2026/07/wordpress-7-1-beta4.png"`) {
		t.Errorf("featured image not localised:\n%s", beta[:min(len(beta), 600)])
	}
	if got := artifactFile(t, a, "public/news/files/2026/07/wordpress-7-1-beta4.png"); !strings.HasPrefix(got, "IMG:") {
		t.Errorf("featured image bytes = %q", got)
	}
	if r.Converted.ImagesDownloaded == 0 || r.Converted.ImagesRemote != 0 {
		t.Errorf("images: %+v", r.Converted)
	}
	// Tables don't convert cleanly: kept as HTML, not dropped.
	if !strings.Contains(beta, "<table") {
		t.Error("the release table was dropped")
	}
	if r.Converted.RawHTMLBlocks == 0 {
		t.Error("expected raw HTML blocks to be counted")
	}
	// Code blocks become fenced Markdown.
	pkgs := artifactFile(t, a, "src/content/posts/wp-packages.md")
	if !strings.Contains(pkgs, "\n```") {
		t.Errorf("no fenced code block in the post with <pre>:\n%s", pkgs)
	}

	// Archives at their WordPress paths, redirects for the moved feed.
	var terms map[string][]wpTermOut
	if err := json.Unmarshal([]byte(artifactFile(t, a, "src/data/terms.json")), &terms); err != nil {
		t.Fatal(err)
	}
	if len(terms["categories"]) == 0 || !strings.HasPrefix(terms["categories"][0].Permalink, "/news/category/") {
		t.Errorf("category archives = %+v", terms["categories"])
	}
	var redirects map[string]string
	if err := json.Unmarshal([]byte(artifactFile(t, a, "redirects.json")), &redirects); err != nil {
		t.Fatal(err)
	}
	if redirects["/news/feed/"] != "/rss.xml" || redirects["/feed/"] != "/rss.xml" {
		t.Errorf("feed redirects = %v", redirects)
	}
	if !strings.Contains(artifactFile(t, a, "astro.config.mjs"), `site: "http://wp.example/news"`) {
		t.Error("astro.config.mjs does not point site at the WordPress home")
	}
	if !strings.Contains(artifactFile(t, a, "package.json"), `"astro": "^5.`) {
		t.Error("package.json does not pin Astro 5")
	}

	// Every path is safe to write under an output directory.
	for _, f := range a.Files {
		if !SafeRelativePath(f.Path) {
			t.Errorf("unsafe output path %q", f.Path)
		}
	}

	// In house style: no em dashes, never "container". And the default
	// project names no host: it is a plain Astro project.
	sum := DefaultSummary(a.Report)
	readme := artifactFile(t, a, "README.md")
	copyTexts := []string{readme, artifactFile(t, a, "MIGRATION-NOTES.md")}
	for _, it := range append(sum.Transfers, sum.DoesNotTransfer...) {
		copyTexts = append(copyTexts, it.Label, it.Detail)
	}
	copyTexts = append(copyTexts, sum.Notes...)
	for _, s := range copyTexts {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(s, DefaultGenerator, "")), "instapods") {
			t.Errorf("the default output must not name a host: %q", s)
		}
	}
	if !strings.Contains(readme, DefaultGenerator) || !strings.Contains(readme, "**With Node**") {
		t.Errorf("README lacks the default credit or deploy section:\n%s", readme)
	}
	for _, s := range copyTexts {
		if strings.Contains(s, "—") || strings.Contains(strings.ToLower(s), "container") {
			t.Errorf("copy breaks the style rule (em dash or 'container'): %q", s)
		}
	}
	if !mentions(sum.DoesNotTransfer, "comments (234") {
		t.Errorf("the comment count should be stated in 'does not transfer': %+v", sum.DoesNotTransfer)
	}
}

func TestWordPressAstroPreviewIsSmallAndDownloadsNothing(t *testing.T) {
	fx := newRecordedFixture(t)
	w := fx.start()
	r, err := w.Preview(context.Background(), "wp.example/news")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if r.Found.Posts != 4 || r.Found.Pages != 2 || r.Found.Categories != 4 || r.Found.Tags != 5 {
		t.Errorf("found = %+v", r.Found)
	}
	if r.Sample == nil || r.Sample.Title != "Critical PHP Security Release" || !strings.Contains(r.Sample.Markdown, "> Where immediate") {
		t.Fatalf("sample = %+v", r.Sample)
	}
	if !strings.Contains(r.Sample.Frontmatter, `categories: ["Security"]`) {
		t.Errorf("sample frontmatter = %s", r.Sample.Frontmatter)
	}
	if len(fx.images) != 0 {
		t.Errorf("the preview downloaded images: %v", fx.images)
	}
	// Discovery 2, site facts 2 + up to 8 one-item type counts, sample 2,
	// term counts 2, sample terms and media 3.
	if len(fx.requests) > 21 {
		t.Errorf("the preview made %d requests; it should stay small: %v", len(fx.requests), fx.requests)
	}
	for _, req := range fx.requests {
		if strings.Contains(req, "per_page=50") || strings.Contains(req, "per_page=100") {
			t.Errorf("the preview paged a full collection: %s", req)
		}
	}
	// Jetpack's internal types hold nothing public; probing them only adds
	// to the burst that gets us throttled.
	for _, base := range []string{"/wp/v2/feedback", "/wp/v2/jetpack-forms", "/wp/v2/activity-log-events", "/wp/v2/jp_pay_order", "/wp/v2/jetpack-search-overlay"} {
		if fx.requested(base + "?") {
			t.Errorf("the preview probed the plugin-internal type %s", base)
		}
	}
	if !fx.requested("/wp/v2/podcast?") {
		t.Error("a real custom type (podcast) must still be counted")
	}
}

// A site with plain permalinks and nothing advertised in its <head>: found by
// probing ?rest_route=, every post moves to a clean path and the old ?p= URL
// gets a 301. Also exercises pagination and the post cap.
func TestWordPressAstroPlainPermalinksRedirectAndPaginate(t *testing.T) {
	fx := &wpFixture{t: t, plain: true, collections: map[string][]map[string]any{}}
	fx.index = map[string]any{"name": "Plain &amp; Simple", "home": "http://wp.example", "url": "http://wp.example", "namespaces": []string{"wp/v2"}}
	fx.types = map[string]any{"post": map[string]any{"slug": "post", "name": "Posts"}}
	fx.homeHTML = `<html><head><link rel="stylesheet" href="/wp-content/themes/x/style.css"></head><body>Hi</body></html>`
	var posts []map[string]any
	for i := 60; i >= 1; i-- {
		content := fmt.Sprintf("<p>Post %d body.</p>", i)
		if i == 60 {
			content = `<p>See <a href="http://wp.example/?p=59">the previous post</a> and <a href="http://wp.example/?page_id=900">about</a>.</p>`
		}
		posts = append(posts, map[string]any{
			"id": i, "date": fmt.Sprintf("2024-01-%02dT10:00:00", i%28+1), "date_gmt": fmt.Sprintf("2024-01-%02dT10:00:00", i%28+1),
			"slug": fmt.Sprintf("post-number-%d", i), "type": "post", "link": fmt.Sprintf("http://wp.example/?p=%d", i),
			"title":   map[string]any{"rendered": fmt.Sprintf("Post &#8220;%d&#8221;", i)},
			"content": map[string]any{"rendered": content, "protected": false},
			"excerpt": map[string]any{"rendered": "<p>Short.</p>", "protected": false},
		})
	}
	fx.collections["posts"] = posts
	fx.collections["pages"] = []map[string]any{{
		"id": 900, "date_gmt": "2023-05-01T00:00:00", "slug": "about", "type": "page", "link": "http://wp.example/?page_id=900",
		"title": map[string]any{"rendered": "About"}, "content": map[string]any{"rendered": "<p>About us.</p>"}, "excerpt": map[string]any{"rendered": ""},
	}}
	w := fx.start()

	a, err := w.Convert(context.Background(), "http://wp.example/", Options{MaxPosts: 55})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	r := a.Report
	if r.Site.PrettyPermalinks || !strings.Contains(r.Site.RESTAPI, "rest_route=") {
		t.Errorf("site = %+v, want the ?rest_route= API", r.Site)
	}
	if r.Site.Name != "Plain & Simple" {
		t.Errorf("name = %q", r.Site.Name)
	}
	if r.Found.Posts != 60 || r.Converted.Posts != 55 || !r.Truncated {
		t.Errorf("found %d converted %d truncated %v; want 60/55/true", r.Found.Posts, r.Converted.Posts, r.Truncated)
	}
	pagesRequested := 0
	for _, req := range fx.requests {
		if strings.Contains(req, "rest_route=%2Fwp%2Fv2%2Fposts") && strings.Contains(req, "page=") {
			pagesRequested++
		}
	}
	if pagesRequested != 2 {
		t.Errorf("post pages requested = %d, want 2 (50 + 5)", pagesRequested)
	}

	var redirects map[string]string
	json.Unmarshal([]byte(artifactFile(t, a, "redirects.json")), &redirects)
	if redirects["/?p=60"] != "/post-number-60/" || redirects["/?page_id=900"] != "/about/" {
		t.Errorf("redirects = %v", redirects)
	}
	if redirects["/?feed=rss2"] != "/rss.xml" {
		t.Errorf("the plain feed URL should redirect: %v", redirects)
	}
	if !strings.Contains(artifactFile(t, a, "public/_redirects"), "/ p=60 /post-number-60/ 301") {
		t.Errorf("_redirects lacks the query redirect:\n%s", artifactFile(t, a, "public/_redirects"))
	}
	top := artifactFile(t, a, "src/content/posts/post-number-60.md")
	if !strings.Contains(top, "[the previous post](/post-number-59/)") || !strings.Contains(top, "[about](/about/)") {
		t.Errorf("internal ?p= links were not rewritten to the new paths:\n%s", top)
	}
	if !strings.Contains(top, `title: "Post “60”"`) {
		t.Errorf("title entities not decoded:\n%s", top)
	}
	if r.Permalinks.Redirected != 56 {
		t.Errorf("redirected = %d, want 56 (55 posts + 1 page)", r.Permalinks.Redirected)
	}
}

// Review findings on PR #969: running out of JSON budget part-way through the
// posts must keep what was read (not fail the whole conversion), and a heavy
// homepage must not stop discovery.
func TestWordPressAstroBudgetKeepsWhatItRead(t *testing.T) {
	fx := &wpFixture{t: t, collections: map[string][]map[string]any{}}
	fx.index = map[string]any{"name": "Heavy", "home": "http://wp.example", "namespaces": []string{"wp/v2"}}
	fx.types = map[string]any{}
	// 3 MB homepage, past the probe's read limit; the Link header still works.
	fx.homeHTML = "<html><head></head><body>" + strings.Repeat("x", 3<<20) + "</body></html>"
	big := "<p>" + strings.Repeat("words ", 25000) + "</p>" // ~150 KB per post
	var posts []map[string]any
	for i := 200; i >= 1; i-- {
		posts = append(posts, map[string]any{
			"id": i, "date_gmt": "2024-02-01T10:00:00", "slug": fmt.Sprintf("p%d", i), "type": "post",
			"link": fmt.Sprintf("http://wp.example/p%d/", i), "title": map[string]any{"rendered": "P"},
			"content": map[string]any{"rendered": big}, "excerpt": map[string]any{"rendered": ""},
		})
	}
	fx.collections["posts"] = posts
	w := fx.start()
	a, err := w.Convert(context.Background(), "http://wp.example/", Options{MaxPosts: 200})
	if err != nil {
		t.Fatalf("Convert failed instead of keeping what it read: %v", err)
	}
	r := a.Report
	if r.Converted.Posts == 0 || r.Converted.Posts >= 200 || !r.Truncated {
		t.Errorf("converted %d, truncated %v; want a partial, flagged result", r.Converted.Posts, r.Truncated)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "time or size limit") {
		t.Errorf("notes don't say why it stopped: %v", r.Notes)
	}
}

func TestSanitizeOutputPathKeepsUTF8(t *testing.T) {
	long := strings.Repeat("я", 70) + ".md" // 143 bytes
	got := sanitizeOutputPath(long)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, ".md") || len(got) > 100 {
		t.Errorf("sanitizeOutputPath split a character or lost the extension: %q (%d bytes)", got, len(got))
	}
}

func TestWordPressAstroExplainsADisabledRESTAPI(t *testing.T) {
	fx := &wpFixture{t: t, restOff: true, homeHTML: `<html><head><script src="/wp-includes/js/jquery.js"></script></head></html>`}
	w := fx.start()
	_, err := w.Preview(context.Background(), "http://wp.example/")
	var restErr *RESTError
	if !errors.As(err, &restErr) || !restErr.IsWordPress {
		t.Fatalf("err = %v, want a RESTError for a WordPress site", err)
	}
	if !strings.Contains(err.Error(), "/wp-json/wp/v2/") {
		t.Errorf("the error must say what to unblock: %v", err)
	}

	fx2 := &wpFixture{t: t, restOff: true, homeHTML: `<html><body>Just a site</body></html>`}
	w2 := fx2.start()
	_, err = w2.Convert(context.Background(), "http://wp.example/", Options{})
	if !errors.As(err, &restErr) || restErr.IsWordPress {
		t.Fatalf("err = %v, want 'not WordPress'", err)
	}
}

func TestSafeRelativePath(t *testing.T) {
	for p, want := range map[string]bool{
		"src/pages/[...path].astro": true,
		"a/b.txt":                   true,
		"../etc/passwd":             false,
		"a/../../b":                 false,
		"/abs":                      false,
		"a\\..\\b":                  false,
		"":                          false,
	} {
		if got := SafeRelativePath(p); got != want {
			t.Errorf("SafeRelativePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestHTMLToMarkdown(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
		not      []string
		raw      int
	}{
		{
			name: "inline",
			in:   `<p>Hello <strong>bold</strong>, <em>it</em>, <code>x()</code> and <a href="https://a.example/p" title="T">a link</a>.</p>`,
			want: []string{"Hello **bold**, *it*, `x()` and [a link](https://a.example/p \"T\")."},
		},
		{
			name: "escaping",
			in:   `<p>1. not a list, *stars*, [brackets] &amp;copy; &lt;tag&gt;</p>`,
			want: []string{`1\. not a list, \*stars\*, \[brackets\] &amp;copy; &lt;tag>`},
		},
		{
			name: "headings and anchors",
			in:   `<h2>Plain</h2><h3 id="anchor">Kept</h3>`,
			want: []string{"## Plain", `<h3 id="anchor">Kept</h3>`},
			raw:  1,
		},
		{
			name: "nested list",
			in:   `<ul><li>One<ul><li>Inner</li></ul></li><li>Two</li></ul><ol start="3"><li>Three</li></ol>`,
			want: []string{"- One\n  - Inner\n- Two", "3. Three"},
		},
		{
			name: "code",
			in:   "<pre class=\"wp-block-code\"><code class=\"language-go\">fmt.Println(1)\n\nx := `a`</code></pre>",
			want: []string{"```go\nfmt.Println(1)\n\nx := `a`\n```"},
		},
		{
			name: "figure",
			in:   `<figure class="wp-block-image"><a href="/full.jpg"><img src="/a.jpg" alt="Alt [x]"></a><figcaption>Cap <em>tion</em></figcaption></figure>`,
			want: []string{`[![Alt \[x\]](/a.jpg)](/full.jpg)`, "*Cap *tion**"},
		},
		{
			name: "table kept, blank lines removed",
			in:   "<figure class=\"wp-block-table\"><table>\n\n<tr><td>a</td></tr>\n\n    <tr><td>b</td></tr></table></figure>",
			want: []string{"<figure class=\"wp-block-table\"><table>\n<tbody><tr><td>a</td></tr>\n    <tr><td>b</td></tr></tbody></table></figure>"},
			not:  []string{"\n\n<tr>"},
			raw:  1,
		},
		{
			name: "lazy image and layout wrapper",
			in:   `<div class="wp-block-group" style="padding:1em"><p>Inside</p><img src="data:image/gif;base64,R0" data-lazy-src="/real.png" alt=""></div>`,
			want: []string{"Inside", "![](/real.png)"},
			not:  []string{"<div", "data:image"},
		},
		{
			name: "styled span kept",
			in:   `<p>A <span style="color:red">red</span> word</p>`,
			want: []string{`A <span style="color:red">red</span> word`},
		},
		{
			name: "form and script kept",
			in:   `<form action="/wp-admin/admin-post.php"><input name="q"></form><script>var a = 1;</script>`,
			want: []string{"<form action=\"/wp-admin/admin-post.php\">", "<script>var a = 1;</script>"},
			raw:  2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := htmlToMarkdown(tc.in, nil)
			for _, w := range tc.want {
				if !strings.Contains(got.Markdown, w) {
					t.Errorf("missing %q in:\n%s", w, got.Markdown)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got.Markdown, n) {
					t.Errorf("unexpected %q in:\n%s", n, got.Markdown)
				}
			}
			if got.RawBlocks != tc.raw {
				t.Errorf("raw blocks = %d, want %d\n%s", got.RawBlocks, tc.raw, got.Markdown)
			}
		})
	}

	for in, want := range map[string]string{
		`<p>Wow!<a href="https://x.example">link</a></p>`: `Wow\![link](https://x.example)`,
		`<p>takes ~5 to ~10</p>`:                          `takes \~5 to \~10`,
		`<p>~~~</p>`:                                      `\~\~\~`,
	} {
		if got := htmlToMarkdown(in, nil).Markdown; !strings.Contains(got, want) {
			t.Errorf("htmlToMarkdown(%s) = %q, want it to contain %q", in, got, want)
		}
	}
	iframe := htmlToMarkdown(`<iframe src="https://wp.example/embed"></iframe>`, func(kind, raw string) string { return kind + ":" + raw })
	if !strings.Contains(iframe.Markdown, `src="link:https://wp.example/embed"`) {
		t.Errorf("an iframe src must be treated as a link, not an image to download: %s", iframe.Markdown)
	}

	res := htmlToMarkdown(`<p>[gallery ids="1,2"] [contact-form-7 id="5"] see [1] and [Note]</p><form></form>`, nil)
	if strings.Join(res.Shortcodes, ",") != "contact-form-7,gallery" {
		t.Errorf("shortcodes = %v; footnotes and bracketed words are not shortcodes", res.Shortcodes)
	}
	if res.Forms != 1 {
		t.Errorf("forms = %d", res.Forms)
	}
}

// wordpress.org/news: a WordPress install in a sub-directory, with a second,
// empty WordPress at the domain root. These pin the 2026-10-24 prod bug: the
// host throttled the /news/ API, discovery fell through to the root API, and
// the preview reported a clean empty site.

func shortThrottleBackoff(t *testing.T) {} // the fixture converter already uses a 1ms backoff

func TestWordPressAstroSubdirectoryInstallReadsItsOwnAPI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noAdvert bool
	}{{"advertised", false}, {"not advertised", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRecordedFixture(t)
			fx.rootSite = true
			if tc.noAdvert {
				fx.noAdvert = true
				fx.homeHTML = `<html><head><link rel="stylesheet" href="/news/wp-content/themes/news/style.css"></head></html>`
			}
			r, err := fx.start().Preview(context.Background(), "http://wp.example/news/")
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			if r.Site.RESTAPI != "http://wp.example/news/wp-json/" || r.Site.Name != "WordPress News" || r.Found.Posts != 4 {
				t.Errorf("read the wrong API: site %+v found %+v", r.Site, r.Found)
			}
			if fx.requested("wp.example /wp-json") {
				t.Error("the domain-root API was read although the sub-directory one answered")
			}
		})
	}
}

func TestWordPressAstroThrottledSubdirectoryDoesNotFallBackToRoot(t *testing.T) {
	shortThrottleBackoff(t)
	fx := newRecordedFixture(t)
	fx.rootSite = true
	fx.throttle429 = 1000
	_, err := fx.start().Preview(context.Background(), "http://wp.example/news/")
	var thr *ThrottledError
	if !errors.As(err, &thr) {
		t.Fatalf("err = %v, want a ThrottledError", err)
	}
	if !strings.Contains(err.Error(), "rate-limiting") {
		t.Errorf("error = %q", err)
	}
	if fx.requested("wp.example /wp-json") || fx.requested("wp.example /?rest_route") {
		t.Errorf("a throttled API must not send us to the domain root: %v", fx.requests)
	}
}

func TestWordPressAstroRetriesAThrottledRequestOnce(t *testing.T) {
	shortThrottleBackoff(t)
	fx := newRecordedFixture(t)
	fx.rootSite = true
	fx.throttle429 = 1 // the index call is throttled once, then answers
	r, err := fx.start().Preview(context.Background(), "http://wp.example/news/")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if r.Site.Name != "WordPress News" || r.Found.Posts != 4 || r.Throttled {
		t.Errorf("site %+v found %+v throttled %v", r.Site, r.Found, r.Throttled)
	}
}

func TestWordPressAstroNotesAThrottledPartialPreview(t *testing.T) {
	shortThrottleBackoff(t)
	fx := newRecordedFixture(t)
	fx.rootSite = true
	w := fx.start()
	// Let discovery and the posts sample through, then throttle the rest.
	fx.mu.Lock()
	fx.throttle429 = 0
	fx.mu.Unlock()
	w.Client.Transport = &throttleAfter{next: w.Client.Transport, fx: fx, after: 3}
	r, err := w.Preview(context.Background(), "http://wp.example/news/")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if r.Found.Posts != 4 {
		t.Errorf("posts = %d, want 4", r.Found.Posts)
	}
	if !r.Throttled || !anyContains(r.Notes, "rate-limited") {
		t.Errorf("throttled = %v, notes = %v", r.Throttled, r.Notes)
	}
}

// throttleAfter turns on the fixture's 429s once `after` requests have gone by.
type throttleAfter struct {
	next  http.RoundTripper
	fx    *wpFixture
	after int
	n     int
}

func (t *throttleAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	t.n++
	if t.n == t.after+1 {
		t.fx.mu.Lock()
		t.fx.throttle429 = 1000
		t.fx.mu.Unlock()
	}
	return t.next.RoundTrip(req)
}

func TestWordPressAstroNotesAnEmptyAPIForASubPath(t *testing.T) {
	fx := newRecordedFixture(t)
	fx.rootSite = true
	fx.subdirOff = true
	fx.noAdvert = true
	fx.homeHTML = `<html><head><link rel="stylesheet" href="/news/wp-content/themes/news/style.css"></head></html>`
	r, err := fx.start().Preview(context.Background(), "http://wp.example/news/")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if r.Found.Posts != 0 || !anyContains(r.Notes, "lists no published posts or pages") || !anyContains(r.Notes, "http://wp.example/news/wp-json/") {
		t.Errorf("an empty API behind a sub-path URL must be explained: found %+v notes %v", r.Found, r.Notes)
	}

	// The same empty site asked for at its own root is just empty.
	r, err = fx.start().Preview(context.Background(), "http://wp.example/")
	if err != nil {
		t.Fatalf("Preview root: %v", err)
	}
	if anyContains(r.Notes, "lists no published posts") {
		t.Errorf("a root URL must not get the sub-directory note: %v", r.Notes)
	}
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// A stale siteurl can advertise an API on a dead domain. That must still fall
// through to the site's own /wp-json/, as before; only a failing API on the
// site's own host stops discovery.
func TestWordPressAstroAdvertisedAPIOnDeadHostFallsThrough(t *testing.T) {
	fx := newRecordedFixture(t)
	fx.noAdvert = true
	fx.homeHTML = `<html><head><link rel="https://api.w.org/" href="http://old.example/wp-json/" /></head></html>`
	w := fx.start()
	w.Client.Transport = &deadHost{next: w.Client.Transport, host: "old.example"}
	r, err := w.Preview(context.Background(), "http://wp.example/news/")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if r.Site.RESTAPI != "http://wp.example/news/wp-json/" || r.Found.Posts != 4 {
		t.Errorf("site %+v found %+v", r.Site, r.Found)
	}

	// The same failure on the site's own advertised API is transient.
	fx2 := newRecordedFixture(t)
	fx2.rootSite = true
	w2 := fx2.start()
	w2.Client.Transport = &deadHost{next: w2.Client.Transport, host: "wp.example", pathPrefix: "/news/wp-json"}
	_, err = w2.Preview(context.Background(), "http://wp.example/news/")
	var un *UnavailableError
	if !errors.As(err, &un) {
		t.Fatalf("err = %v, want UnavailableError", err)
	}
	if fx2.requested("wp.example /wp-json") {
		t.Error("a failing own-host API must not send us to the domain root")
	}
}

// deadHost fails every request to host (optionally only under pathPrefix)
// the way an unreachable server does.
type deadHost struct {
	next       http.RoundTripper
	host       string
	pathPrefix string
}

func (d *deadHost) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() == d.host && strings.HasPrefix(req.URL.Path, d.pathPrefix) {
		return nil, errors.New("connection refused")
	}
	return d.next.RoundTrip(req)
}

func TestConverterHooksReplaceREADMEAndSummary(t *testing.T) {
	fx := newRecordedFixture(t)
	w := fx.start()
	w.Generator = "the Example converter"
	w.DeploySection = "**On Example**: run `example deploy`.\n\n"
	w.Summary = func(r *Report) Summary {
		return Summary{Transfers: []SummaryItem{{Label: "Custom", Detail: "From the hook."}}}
	}
	a, err := w.Convert(context.Background(), "http://wp.example/news/", Options{})
	if err != nil {
		t.Fatal(err)
	}
	readme := artifactFile(t, a, "README.md")
	if !strings.Contains(readme, "by the Example converter.") || !strings.Contains(readme, "## Deploy\n\n**On Example**: run `example deploy`.\n\n**Your domain**") {
		t.Errorf("README hooks not applied:\n%s", readme)
	}
	if strings.Contains(readme, "**With Node**") {
		t.Error("the default deploy section should be replaced, not kept")
	}
	if notes := artifactFile(t, a, "MIGRATION-NOTES.md"); !strings.Contains(notes, "**Custom.** From the hook.") {
		t.Errorf("summary hook not applied:\n%s", notes)
	}
}

func TestZeroConverterUsesLocalDefaults(t *testing.T) {
	var w Converter
	l := w.limits()
	if l != DefaultLimits() {
		t.Errorf("zero Converter limits = %+v", l)
	}
	if p := w.previewLimits(); p.MaxRequests != 25 || p.JSONBytes != 4<<20 {
		t.Errorf("preview limits = %+v", p)
	}
	if (Limits{MaxRequests: -1}).withDefaults(DefaultPreviewLimits()).MaxRequests != 0 {
		t.Error("a negative MaxRequests must mean no cap")
	}
	if (Options{}).maxPosts(l) != l.DefaultPosts || (Options{MaxPosts: 1 << 30}).maxPosts(l) != l.MaxPosts {
		t.Error("maxPosts does not clamp to the limits")
	}
}

// Jetpack's image CDN rate-limits a burst (seen on wordpress.org/news): the
// original file on the site is downloaded instead, so the post still gets a
// local image.
func TestWordPressAstroFallsBackToTheOriginalWhenTheImageCDNFails(t *testing.T) {
	fx := newRecordedFixture(t)
	fx.cdn429 = true
	a, err := fx.start().Convert(context.Background(), "http://wp.example/news/", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Report.Converted.ImagesRemote != 0 || a.Report.Converted.ImagesDownloaded == 0 {
		t.Errorf("images = %+v, want every same-site image downloaded", a.Report.Converted)
	}
	beta := artifactFile(t, a, "src/content/posts/wordpress-7-1-beta-4.md")
	if strings.Contains(beta, "i0.wp.com") {
		t.Errorf("a same-site image still points at the CDN:\n%s", beta)
	}
	if !fx.requested("wp.example /news/files/") {
		t.Errorf("the original was never requested: %v", fx.images)
	}
}
