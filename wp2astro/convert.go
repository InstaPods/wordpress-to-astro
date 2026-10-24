package wp2astro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ------------------------------------------------------------------ REST client

type wpREST struct {
	client    *http.Client
	userAgent string
	lim       Limits
	progress  func(stage string, done, total int)
	root      *url.URL // API root: .../wp-json/ or .../?rest_route=/
	deadline  time.Time
	budget    int64
	used      int64
	hosts     map[string]bool
	// maxRequests caps outbound requests for one run (0 = no cap), so one
	// anonymous preview stays a small, known cost.
	maxRequests int
	requests    int
	// retries counts 429 retries; throttled is set when a 429 stuck.
	retries   int
	throttled bool
	mu        sync.Mutex
}

func (c *wpREST) wasThrottled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.throttled
}

func (c *wpREST) addHost(h string) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return
	}
	c.hosts[h] = true
	if strings.HasPrefix(h, "www.") {
		c.hosts[strings.TrimPrefix(h, "www.")] = true
	} else {
		c.hosts["www."+h] = true
	}
}

func (c *wpREST) ownHost(u *url.URL) bool {
	return u != nil && c.hosts[strings.ToLower(u.Hostname())]
}

func (c *wpREST) queryMode() bool { return c.root.Query().Has("rest_route") }

// endpoint builds the URL for a REST route such as "/wp/v2/posts".
func (c *wpREST) endpoint(route string, q url.Values) string {
	u := *c.root
	params := url.Values{}
	for k, v := range q {
		params[k] = v
	}
	if c.queryMode() {
		for k, v := range c.root.Query() {
			if k != "rest_route" {
				params[k] = v
			}
		}
		params.Set("rest_route", route)
	} else {
		for k, v := range c.root.Query() {
			params[k] = v
		}
		u.Path = strings.TrimSuffix(c.root.Path, "/") + route
		u.RawPath = ""
	}
	u.RawQuery = params.Encode()
	return u.String()
}

type wpHTTPError struct {
	Status int
	Code   string
}

func (e *wpHTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("the site's REST API returned HTTP %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("the site's REST API returned HTTP %d", e.Status)
}

var errWPBudget = errors.New("conversion budget reached")

var errWPNotJSON = errors.New("did not return JSON")

// A 429 is retried once after a short wait, at most Limits.MaxRetries times
// a run. Some hosts (wordpress.org's nginx) throttle a burst of REST calls
// from one IP and recover within a second or two.

func isThrottled(err error) bool {
	var herr *wpHTTPError
	return errors.As(err, &herr) && herr.Status == http.StatusTooManyRequests
}

// fetch GETs rawURL and returns at most limit bytes of body. A 429 is
// retried once when the wait fits the deadline and the run's retry budget.
func (c *wpREST) fetch(ctx context.Context, rawURL, accept string, limit int64) ([]byte, http.Header, *url.URL, error) {
	body, hdr, final, err := c.fetchOnce(ctx, rawURL, accept, limit)
	if !isThrottled(err) {
		return body, hdr, final, err
	}
	if c.backoff(ctx, hdr) {
		body, hdr, final, err = c.fetchOnce(ctx, rawURL, accept, limit)
	}
	if isThrottled(err) {
		c.mu.Lock()
		c.throttled = true
		c.mu.Unlock()
	}
	return body, hdr, final, err
}

// backoff waits before retrying an HTTP 429, if the wait fits the deadline
// and the run's retry budget. It reports whether the caller should retry.
func (c *wpREST) backoff(ctx context.Context, hdr http.Header) bool {
	wait := c.lim.RetryBackoff
	if secs, perr := strconv.Atoi(strings.TrimSpace(hdr.Get("Retry-After"))); perr == nil && secs >= 0 {
		wait = time.Duration(secs) * time.Second
	}
	c.mu.Lock()
	retry := c.retries < c.lim.MaxRetries && wait <= c.lim.MaxRetryWait && time.Now().Add(wait).Before(c.deadline)
	if retry {
		c.retries++
	}
	c.mu.Unlock()
	if !retry {
		return false
	}
	if c.progress != nil {
		c.progress("throttled", int(wait/time.Millisecond), -1)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *wpREST) fetchOnce(ctx context.Context, rawURL, accept string, limit int64) ([]byte, http.Header, *url.URL, error) {
	if time.Now().After(c.deadline) {
		return nil, nil, nil, errWPBudget
	}
	c.mu.Lock()
	remaining := c.budget - c.used
	over := c.maxRequests > 0 && c.requests >= c.maxRequests
	c.requests++
	c.mu.Unlock()
	if remaining <= 0 || over {
		return nil, nil, nil, errWPBudget
	}
	if remaining < limit {
		limit = remaining
	}
	ctx, cancel := context.WithDeadline(ctx, c.deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", accept)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, nil, cleanFetchError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read response: %w", err)
	}
	c.mu.Lock()
	c.used += int64(len(body))
	c.mu.Unlock()
	if int64(len(body)) > limit {
		return nil, nil, nil, errWPBudget
	}
	final := resp.Request.URL
	if resp.StatusCode != http.StatusOK {
		herr := &wpHTTPError{Status: resp.StatusCode}
		var e struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &e) == nil {
			herr.Code = e.Code
		}
		return body, resp.Header, final, herr
	}
	return body, resp.Header, final, nil
}

// probe GETs the page the user gave us, to read the API link WordPress puts on
// every page. It reads at most wpHTMLProbeBytes and keeps what it got rather
// than failing (the link is in the headers and <head>), and it is not charged
// to the JSON budget. Any HTTP status is fine: a 404 page carries the header too.
func (c *wpREST) probe(ctx context.Context, rawURL string) ([]byte, http.Header, *url.URL, error) {
	c.mu.Lock()
	c.requests++
	c.mu.Unlock()
	ctx, cancel := context.WithDeadline(ctx, c.deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,*/*;q=0.8")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, nil, cleanFetchError(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, wpHTMLProbeBytes))
	return body, resp.Header, resp.Request.URL, nil
}

// getJSON calls a REST route and decodes the response into out.
func (c *wpREST) getJSON(ctx context.Context, route string, q url.Values, out any) (http.Header, error) {
	body, hdr, _, err := c.fetch(ctx, c.endpoint(route, q), "application/json", wpMaxResponseBytes)
	if err != nil {
		return hdr, err
	}
	body = bytes.TrimPrefix(bytes.TrimSpace(body), []byte("\xef\xbb\xbf"))
	if err := json.Unmarshal(body, out); err != nil {
		return hdr, fmt.Errorf("the REST API at %s %w", route, errWPNotJSON)
	}
	return hdr, nil
}

func headerInt(h http.Header, key string) int {
	if h == nil {
		return -1
	}
	v, err := strconv.Atoi(strings.TrimSpace(h.Get(key)))
	if err != nil {
		return -1
	}
	return v
}

// count asks for one item and reads the total from X-WP-Total.
func (c *wpREST) count(ctx context.Context, route string) int {
	var raw []json.RawMessage
	hdr, err := c.getJSON(ctx, route, url.Values{"per_page": {"1"}, "_fields": {"id"}}, &raw)
	if err != nil {
		return -1
	}
	return headerInt(hdr, "X-WP-Total")
}

// list pages through a collection until max items, the last page, or the
// budget. It returns the total the API reported.
func (c *wpREST) list(ctx context.Context, route string, q url.Values, perPage, max int, each func(json.RawMessage)) (int, error) {
	total := -1
	got := 0
	for page := 1; got < max; page++ {
		params := url.Values{}
		for k, v := range q {
			params[k] = v
		}
		n := perPage
		if max-got < n {
			n = max - got
		}
		params.Set("per_page", strconv.Itoa(n))
		params.Set("page", strconv.Itoa(page))
		var items []json.RawMessage
		hdr, err := c.getJSON(ctx, route, params, &items)
		if err != nil {
			var herr *wpHTTPError
			// Asking past the last page answers 400 rest_post_invalid_page_number.
			if page > 1 && errors.As(err, &herr) && herr.Status == http.StatusBadRequest {
				break
			}
			return total, err
		}
		if t := headerInt(hdr, "X-WP-Total"); t >= 0 {
			total = t
		}
		for _, it := range items {
			each(it)
			got++
		}
		pages := headerInt(hdr, "X-WP-TotalPages")
		if len(items) < n || (pages >= 0 && page >= pages) {
			break
		}
	}
	if total < 0 {
		total = got
	}
	return total, nil
}

// ------------------------------------------------------------------ discovery

var apiLinkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?https://api\.w\.org/"?`)

// discover finds the REST API root. WordPress advertises it on every page, in
// a Link header and a <link rel="https://api.w.org/"> tag, which also tells us
// whether the site uses ?rest_route= (plain permalinks). When neither is
// there we probe the usual places.
func (w *Converter) discover(ctx context.Context, start *url.URL, lim Limits) (*wpREST, *wpIndex, error) {
	c := &wpREST{
		client: w.client(), userAgent: w.userAgent(), lim: lim, progress: w.Progress,
		deadline: time.Now().Add(lim.Timeout), budget: lim.JSONBytes, maxRequests: lim.MaxRequests,
		hosts: map[string]bool{},
	}
	c.addHost(start.Hostname())

	var candidates []*url.URL
	isWP := false
	advertised := map[string]bool{}
	body, hdr, final, err := c.probe(ctx, start.String())
	if err != nil {
		// Unreachable (or refused by the caller's client): the REST API is on
		// the same host, so there is no point probing it.
		return nil, nil, err
	}
	if final != nil {
		c.addHost(final.Hostname())
	}
	base := start
	if final != nil {
		base = final
	}
	for _, v := range hdr.Values("Link") {
		if m := apiLinkRe.FindStringSubmatch(v); m != nil {
			if u, err := base.Parse(strings.TrimSpace(m[1])); err == nil {
				candidates = append(candidates, u)
			}
		}
	}
	if len(candidates) == 0 && len(body) > 0 {
		if href := findAPILinkTag(body); href != "" {
			if u, err := base.Parse(href); err == nil {
				candidates = append(candidates, u)
			}
		}
	}
	for _, u := range candidates {
		advertised[u.String()] = true
	}
	isWP = len(candidates) > 0 || bytes.Contains(body, []byte("/wp-content/")) || bytes.Contains(body, []byte("/wp-includes/"))

	// Fallbacks: the given path, then the site root, pretty then plain.
	dir := strings.TrimSuffix(base.Path, "/") + "/"
	for _, p := range []string{dir, "/"} {
		candidates = append(candidates,
			&url.URL{Scheme: base.Scheme, Host: base.Host, Path: p + "wp-json/"},
			&url.URL{Scheme: base.Scheme, Host: base.Host, Path: p, RawQuery: "rest_route=/"})
	}

	seen := map[string]bool{}
	var lastStatus int
	for _, cand := range candidates {
		if cand.Scheme != "http" && cand.Scheme != "https" {
			continue
		}
		if seen[cand.String()] {
			continue
		}
		seen[cand.String()] = true
		c.root = cand
		var idx wpIndex
		_, err := c.getJSON(ctx, "/", url.Values{"_fields": {"name,description,url,home,namespaces"}}, &idx)
		if err != nil {
			var herr *wpHTTPError
			if errors.As(err, &herr) {
				lastStatus = herr.Status
			}
			if errors.Is(err, errWPBudget) {
				return nil, nil, err
			}
			// A throttled or failing API is still the right API. Moving on
			// to the next candidate would read whatever WordPress sits at the
			// domain root, which for a sub-directory install is another site.
			if isThrottled(err) {
				return nil, nil, &ThrottledError{URL: start.String()}
			}
			// Only for an API on the site's own host: a stale siteurl can
			// advertise a dead domain, and then the fallbacks are right.
			if advertised[cand.String()] && c.ownHost(cand) && ((herr == nil && !errors.Is(err, errWPNotJSON)) || (herr != nil && herr.Status >= 500)) {
				return nil, nil, &UnavailableError{URL: start.String(), API: cand.String(), Err: err}
			}
			continue
		}
		if idx.Name == "" && len(idx.Namespaces) == 0 && idx.Home == "" {
			continue // JSON, but not a WordPress index
		}
		isWP = true
		if len(idx.Namespaces) > 0 && !contains(idx.Namespaces, "wp/v2") {
			return nil, nil, &RESTError{URL: start.String(), IsWordPress: true}
		}
		c.addHost(cand.Hostname())
		for _, h := range []string{idx.Home, idx.URL} {
			if u, err := url.Parse(h); err == nil {
				c.addHost(u.Hostname())
			}
		}
		return c, &idx, nil
	}
	return nil, nil, &RESTError{URL: start.String(), IsWordPress: isWP, Status: lastStatus}
}

var apiLinkTagRe = regexp.MustCompile(`(?i)<link[^>]+rel=["']https://api\.w\.org/["'][^>]*>`)
var hrefAttrRe = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)

func findAPILinkTag(body []byte) string {
	tag := apiLinkTagRe.Find(body)
	if tag == nil {
		return ""
	}
	if m := hrefAttrRe.FindSubmatch(tag); m != nil {
		return html.UnescapeString(string(m[1]))
	}
	return ""
}

// ------------------------------------------------------------------ REST types

type wpIndex struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	Home        string   `json:"home"`
	Namespaces  []string `json:"namespaces"`
}

type wpRendered struct {
	Rendered  string `json:"rendered"`
	Protected bool   `json:"protected"`
}

type wpItem struct {
	ID            int        `json:"id"`
	Date          string     `json:"date"`
	DateGMT       string     `json:"date_gmt"`
	Modified      string     `json:"modified"`
	ModifiedGMT   string     `json:"modified_gmt"`
	Slug          string     `json:"slug"`
	Type          string     `json:"type"`
	Link          string     `json:"link"`
	Title         wpRendered `json:"title"`
	Content       wpRendered `json:"content"`
	Excerpt       wpRendered `json:"excerpt"`
	Categories    []int      `json:"categories"`
	Tags          []int      `json:"tags"`
	FeaturedMedia int        `json:"featured_media"`
	Parent        int        `json:"parent"`
	MenuOrder     int        `json:"menu_order"`
}

type wpTerm struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Link  string `json:"link"`
	Count int    `json:"count"`
}

type wpMedia struct {
	ID        int        `json:"id"`
	SourceURL string     `json:"source_url"`
	AltText   string     `json:"alt_text"`
	Title     wpRendered `json:"title"`
}

type wpType struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	RestBase string `json:"rest_base"`
}

const wpItemFields = "id,date,date_gmt,modified,modified_gmt,slug,type,link,title,content,excerpt,categories,tags,featured_media,parent,menu_order"

// builtInTypes are WordPress's own post types: not user content, or converted
// already (post, page), or handled as images (attachment).
var builtInTypes = map[string]bool{
	"post": true, "page": true, "attachment": true, "nav_menu_item": true,
	"wp_block": true, "wp_template": true, "wp_template_part": true,
	"wp_navigation": true, "wp_global_styles": true, "wp_font_family": true,
	"wp_font_face": true,
}

// pluginInternalTypes are plugin post types that hold no public content
// (Jetpack form responses, activity log, payments, search overlay). Probing
// each one costs a request, and a burst of them gets us throttled.
var pluginInternalTypes = map[string]bool{
	"feedback": true, "jetpack_form": true, "jp_act_log_event": true,
	"jp_pay_order": true, "jp_pay_product": true, "jp_search_overlay": true,
}

// ------------------------------------------------------------------ conversion

// wpEntry is one post or page on its way into the project.
type wpEntry struct {
	item       wpItem
	kind       string // "post" or "page"
	title      string
	permalink  string // decoded path, e.g. /2024/05/hello-world/
	file       string // project path of the Markdown file
	date       string
	updated    string
	excerpt    string
	categories []string
	tags       []string
	featured   string // absolute URL until images are resolved
	featAlt    string
	postsIndex bool
	md         mdResult
}

// wpConversion holds the state of one run.
type wpConversion struct {
	conv       *Converter
	lim        Limits
	rest       *wpREST
	start      *url.URL // the URL the user gave us
	index      *wpIndex
	home       *url.URL
	report     *Report
	entries    []*wpEntry
	byOldURL   map[string]string // old path?query -> new permalink
	paths      map[string]bool   // every route the project serves
	redirects  map[string]string
	terms      map[string][]wpTermOut
	categories map[int]wpTerm
	tags       map[int]wpTerm
	media      map[int]wpMedia
	images     map[string]string // absolute image URL -> local public path ("" = not downloaded)
	imageOrder []string
	// imageFetch is where each image is downloaded from. Usually the image
	// itself; for a Jetpack CDN reference it is the CDN's resized copy, which
	// is the size the post displays and often a tenth of the original.
	imageFetch map[string]string
	files      []File
	imgBytes   int64
}

type wpTermOut struct {
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Permalink string `json:"permalink"`
	Taxonomy  string `json:"taxonomy"`
	Count     int    `json:"count"`
}

// Convert reads the site and builds the full Astro project.
func (w *Converter) Convert(ctx context.Context, rawURL string, opts Options) (*Project, error) {
	start, err := normalizeStartURL(rawURL)
	if err != nil {
		return nil, err
	}
	lim := w.limits()
	rest, idx, err := w.discover(ctx, start, lim)
	if err != nil {
		return nil, err
	}
	c := newWPConversion(w, lim, rest, idx, start)
	if w.OnDiscover != nil {
		if err := w.OnDiscover(c.report.Site); err != nil {
			return nil, err
		}
	}

	maxPosts := opts.maxPosts(lim)
	posts, err := c.fetchItems(ctx, "/wp/v2/posts", "post", maxPosts)
	if err != nil {
		if !errors.Is(err, errWPBudget) || c.report.Converted.Posts == 0 {
			return nil, wpListError(err, start)
		}
		// Out of time or bytes part-way through: keep what we have and say so,
		// rather than failing a conversion that already holds real posts.
		c.report.Truncated = true
		c.report.Notes = append(c.report.Notes, fmt.Sprintf(
			"Reading posts hit the conversion's time or size limit after %d posts. Lower the post count, or add the rest by hand.",
			c.report.Converted.Posts))
	}
	c.report.Found.Posts = posts
	pages, err := c.fetchItems(ctx, "/wp/v2/pages", "page", lim.MaxPages)
	if err != nil && !errors.Is(err, errWPBudget) {
		// A site can turn pages off in the API and still serve posts.
		c.report.Notes = append(c.report.Notes, "We could not read your pages through the REST API ("+err.Error()+"), so only posts were converted.")
	}
	c.report.Found.Pages = pages
	if c.report.Found.Posts > c.report.Converted.Posts || c.report.Found.Pages > c.report.Converted.Pages {
		c.report.Truncated = true
	}
	c.fetchSiteFacts(ctx)

	c.fetchTerms(ctx, false)
	c.fetchFeaturedMedia(ctx)
	c.assignPaths()
	c.convertContent(nil) // pass 1: collect image URLs
	c.downloadImages(ctx)
	c.convertContent(c.images) // pass 2: write final references
	c.finishReport()

	return &Project{
		SourceURL: start.String(),
		Host:      c.home.Host,
		Files:     c.projectFiles(),
		Report:    c.report,
	}, nil
}

// Preview reads just enough to answer "what will I get?": counts, one sample
// post converted, and what will not come across. It downloads no images.
func (w *Converter) Preview(ctx context.Context, rawURL string) (*Report, error) {
	start, err := normalizeStartURL(rawURL)
	if err != nil {
		return nil, err
	}
	rest, idx, err := w.discover(ctx, start, w.previewLimits())
	if err != nil {
		return nil, err
	}
	// Truncation and the notes describe what a full Convert would include.
	c := newWPConversion(w, w.limits(), rest, idx, start)

	// Posts first: they are the answer. Comments and custom types only feed
	// the "won't come across" list, so they go last, when a throttling host
	// costs us the least.
	posts, err := c.fetchSample(ctx, "/wp/v2/posts", "post", wpPreviewSamplePosts)
	if err != nil {
		return nil, wpListError(err, start)
	}
	c.report.Found.Posts = posts
	pages, _ := c.fetchSample(ctx, "/wp/v2/pages", "page", 1)
	c.report.Found.Pages = pages
	c.report.Found.Categories = rest.count(ctx, "/wp/v2/categories")
	c.report.Found.Tags = rest.count(ctx, "/wp/v2/tags")
	c.fetchSiteFacts(ctx)
	c.report.Truncated = c.report.Found.Posts > c.lim.DefaultPosts || c.report.Found.Pages > c.lim.MaxPages

	// Pick the first entry with real content as the sample, preferring a post.
	var sample *wpEntry
	for _, e := range c.entries {
		if strings.TrimSpace(e.item.Content.Rendered) != "" && !e.item.Content.Protected {
			sample = e
			break
		}
	}
	if sample != nil {
		c.entries = []*wpEntry{sample}
		c.fetchTerms(ctx, true)
		c.fetchFeaturedMedia(ctx)
	}
	c.assignPaths()
	c.convertContent(nil)
	if sample != nil {
		// Show references as they will be once downloaded.
		planned := map[string]string{}
		for _, u := range c.imageOrder {
			planned[u] = localImagePath(u)
		}
		c.convertContent(planned)
		c.report.Sample = &Sample{
			Title:       sample.title,
			SourceURL:   sample.item.Link,
			Permalink:   sample.permalink,
			Path:        sample.file,
			Frontmatter: c.frontmatter(sample, planned),
			Markdown:    shortenSample(sample.md.Markdown),
			Images:      len(c.imageOrder),
			RawHTML:     sample.md.RawBlocks,
		}
		c.report.Shortcodes = nonNil(sample.md.Shortcodes)
		if sample.md.Forms > 0 {
			c.report.PostsWithForms = 1
		}
	}
	c.report.Converted = Converted{}
	c.report.Permalinks.Kept, c.report.Permalinks.Redirected = 0, 0
	if !c.report.Site.PrettyPermalinks {
		c.report.Permalinks.Redirected = c.report.Found.Posts + c.report.Found.Pages
	} else if c.report.Found.Posts >= 0 && c.report.Found.Pages >= 0 {
		c.report.Permalinks.Kept = c.report.Found.Posts + c.report.Found.Pages
	}
	c.previewNotes()
	return c.report, nil
}

// shortenSample keeps the preview response (and the per-site cache entry
// behind it) small whatever the sample post weighs.
func shortenSample(s string) string {
	if len(s) <= wpPreviewSampleBytes {
		return s
	}
	cut := wpPreviewSampleBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n\n[... the rest of this post is in the full conversion]\n"
}

func wpListError(err error, start *url.URL) error {
	if isThrottled(err) {
		return &ThrottledError{URL: start.String()}
	}
	var herr *wpHTTPError
	if errors.As(err, &herr) && (herr.Status == http.StatusUnauthorized || herr.Status == http.StatusForbidden || herr.Status == http.StatusNotFound) {
		return &RESTError{URL: start.String(), IsWordPress: true, Status: herr.Status}
	}
	return fmt.Errorf("reading posts: %w", err)
}

func newWPConversion(w *Converter, lim Limits, rest *wpREST, idx *wpIndex, start *url.URL) *wpConversion {
	home := start
	if u, err := url.Parse(strings.TrimSpace(idx.Home)); err == nil && u.Host != "" {
		home = u
	}
	name := strings.TrimSpace(html.UnescapeString(idx.Name))
	if name == "" {
		name = home.Hostname()
	}
	homeStr := strings.TrimSuffix(home.String(), "/")
	if home.Path == "" {
		homeStr = home.Scheme + "://" + home.Host
	}
	return &wpConversion{
		conv:  w,
		lim:   lim,
		rest:  rest,
		start: start,
		index: idx,
		home:  home,
		report: &Report{
			Site: Site{
				Name:             name,
				Description:      strings.TrimSpace(html.UnescapeString(idx.Description)),
				Home:             homeStr,
				RESTAPI:          rest.root.String(),
				PrettyPermalinks: !rest.queryMode(),
			},
			Found:       Counts{Posts: -1, Pages: -1, Categories: -1, Tags: -1, Comments: -1},
			CustomTypes: []string{},
			Shortcodes:  []string{},
			Notes:       []string{},
		},
		byOldURL:   map[string]string{},
		paths:      map[string]bool{},
		redirects:  map[string]string{},
		terms:      map[string][]wpTermOut{"categories": {}, "tags": {}},
		categories: map[int]wpTerm{},
		tags:       map[int]wpTerm{},
		media:      map[int]wpMedia{},
	}
}

// fetchSiteFacts reads the things that only feed the "what won't come across"
// list: comment count and custom content types. Failures are not fatal.
func (c *wpConversion) fetchSiteFacts(ctx context.Context) {
	c.report.Found.Comments = c.rest.count(ctx, "/wp/v2/comments")
	var types map[string]wpType
	// No _fields here: the response is an object keyed by type slug, and
	// _fields filters those keys away, leaving an empty list.
	if _, err := c.rest.getJSON(ctx, "/wp/v2/types", nil, &types); err != nil {
		return
	}
	slugs := make([]string, 0, len(types))
	for slug := range types {
		if !builtInTypes[slug] && !pluginInternalTypes[slug] && !strings.HasPrefix(slug, "wp_") {
			slugs = append(slugs, slug)
		}
	}
	sort.Strings(slugs)
	// Plugins register plenty of internal types (form responses, activity
	// logs, templates). Only the ones with public items are content a reader
	// would miss, so ask each for a count; private ones answer 401.
	for i, slug := range slugs {
		if i == 8 {
			break
		}
		t := types[slug]
		base := t.RestBase
		if base == "" {
			base = slug
		}
		if strings.ContainsAny(base, "()?[") {
			continue
		}
		n := c.rest.count(ctx, "/wp/v2/"+base)
		if n <= 0 {
			continue
		}
		name := strings.TrimSpace(html.UnescapeString(t.Name))
		if name == "" {
			name = slug
		}
		c.report.CustomTypes = append(c.report.CustomTypes, fmt.Sprintf("%s (%d)", name, n))
	}
}

func (c *wpConversion) addItem(raw json.RawMessage, kind string) {
	var it wpItem
	if json.Unmarshal(raw, &it) != nil || it.ID == 0 {
		return
	}
	c.entries = append(c.entries, &wpEntry{item: it, kind: kind})
	if kind == "post" {
		c.report.Converted.Posts++
		c.progress("posts", c.report.Converted.Posts, -1)
	} else {
		c.report.Converted.Pages++
		c.progress("pages", c.report.Converted.Pages, -1)
	}
}

func (c *wpConversion) progress(stage string, done, total int) {
	if c.conv.Progress != nil {
		c.conv.Progress(stage, done, total)
	}
}

func (c *wpConversion) fetchItems(ctx context.Context, route, kind string, max int) (int, error) {
	return c.rest.list(ctx, route, url.Values{"_fields": {wpItemFields}}, wpPerPage, max,
		func(raw json.RawMessage) { c.addItem(raw, kind) })
}

func (c *wpConversion) fetchSample(ctx context.Context, route, kind string, n int) (int, error) {
	var items []json.RawMessage
	hdr, err := c.rest.getJSON(ctx, route, url.Values{"_fields": {wpItemFields}, "per_page": {strconv.Itoa(n)}}, &items)
	if err != nil {
		return -1, err
	}
	for _, raw := range items {
		c.addItem(raw, kind)
	}
	total := headerInt(hdr, "X-WP-Total")
	if total < 0 {
		total = len(items)
	}
	return total, nil
}

// fetchTerms loads categories and tags. In preview mode only the sample's
// terms are fetched (by id); a full run reads every term.
func (c *wpConversion) fetchTerms(ctx context.Context, onlyUsed bool) {
	for _, tax := range []struct {
		route, key string
		dst        map[int]wpTerm
		ids        func(*wpEntry) []int
	}{
		{"/wp/v2/categories", "categories", c.categories, func(e *wpEntry) []int { return e.item.Categories }},
		{"/wp/v2/tags", "tags", c.tags, func(e *wpEntry) []int { return e.item.Tags }},
	} {
		q := url.Values{"_fields": {"id,name,slug,link,count"}}
		max := c.lim.MaxTerms
		if onlyUsed {
			var ids []string
			for _, e := range c.entries {
				for _, id := range tax.ids(e) {
					ids = append(ids, strconv.Itoa(id))
				}
			}
			if len(ids) == 0 {
				continue
			}
			if len(ids) > 100 {
				ids = ids[:100]
			}
			q.Set("include", strings.Join(ids, ","))
			max = len(ids)
		}
		total, err := c.rest.list(ctx, tax.route, q, 100, max, func(raw json.RawMessage) {
			var t wpTerm
			if json.Unmarshal(raw, &t) == nil && t.ID != 0 {
				tax.dst[t.ID] = t
			}
		})
		if err != nil && !onlyUsed {
			c.report.Notes = append(c.report.Notes, "We could not read your "+tax.key+" ("+err.Error()+"); posts keep their content but lose those labels.")
		}
		if !onlyUsed {
			if tax.key == "categories" {
				c.report.Found.Categories = total
			} else {
				c.report.Found.Tags = total
			}
		}
	}
}

func (c *wpConversion) fetchFeaturedMedia(ctx context.Context) {
	var ids []string
	seen := map[int]bool{}
	for _, e := range c.entries {
		if id := e.item.FeaturedMedia; id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, strconv.Itoa(id))
		}
	}
	for i := 0; i < len(ids); i += 100 {
		j := i + 100
		if j > len(ids) {
			j = len(ids)
		}
		q := url.Values{"include": {strings.Join(ids[i:j], ",")}, "_fields": {"id,source_url,alt_text,title"}}
		var items []wpMedia
		if _, err := c.rest.getJSON(ctx, "/wp/v2/media", q, &items); err != nil {
			c.report.Notes = append(c.report.Notes, "We could not read some featured images ("+err.Error()+"); those posts have no featured image in the project.")
			return
		}
		for _, m := range items {
			c.media[m.ID] = m
		}
	}
}

// assignPaths decides where every entry lives. The rule: a post or page is
// built at exactly the path it has today. Only when that is impossible (plain
// ?p=123 permalinks, or two items on one path) does it get a new path, and
// then the old URL gets a 301.
func (c *wpConversion) assignPaths() {
	usedFiles := map[string]bool{}
	frontSet := false
	for _, e := range c.entries {
		it := e.item
		e.title = plainText(it.Title.Rendered)
		if e.title == "" {
			e.title = "Untitled"
		}
		e.date = wpTime(it.DateGMT, it.Date)
		e.updated = wpTime(it.ModifiedGMT, it.Modified)
		if !it.Excerpt.Protected {
			e.excerpt = cleanExcerpt(it.Excerpt.Rendered)
		}
		for _, id := range it.Categories {
			if t, ok := c.categories[id]; ok && t.Name != "" {
				e.categories = append(e.categories, plainText(t.Name))
			}
		}
		for _, id := range it.Tags {
			if t, ok := c.tags[id]; ok && t.Name != "" {
				e.tags = append(e.tags, plainText(t.Name))
			}
		}
		if m, ok := c.media[it.FeaturedMedia]; ok && m.SourceURL != "" {
			e.featured = m.SourceURL
			e.featAlt = strings.TrimSpace(m.AltText)
		}

		slug := it.Slug
		if s, err := url.PathUnescape(slug); err == nil {
			slug = s
		}
		if strings.TrimSpace(slug) == "" {
			slug = e.kind + "-" + strconv.Itoa(it.ID)
		}

		link, err := url.Parse(it.Link)
		var p string
		oldKey := ""
		switch {
		case err != nil || link.Path == "" && link.RawQuery == "":
			p = "/" + slug + "/"
		case link.RawQuery != "":
			// Plain permalinks: /?p=12 or /?page_id=5. A static site cannot route
			// on a query string, so give it a clean path and redirect the old one.
			oldKey = link.EscapedPath() + "?" + link.RawQuery
			if link.Path == "" {
				oldKey = "/?" + link.RawQuery
			}
			p = "/" + slug + "/"
		default:
			p = link.Path
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		// The site's front page.
		if e.kind == "page" && link != nil && link.RawQuery == "" && sameURLPath(link.Path, c.home.Path) {
			if !frontSet {
				p = "/"
				frontSet = true
			}
		}
		if c.paths[p] && p != "/" {
			// Two items on one path. Keep the first; give this one a suffix.
			p = strings.TrimSuffix(p, "/") + "-" + strconv.Itoa(it.ID) + "/"
			c.report.Notes = append(c.report.Notes, fmt.Sprintf("%q shared its URL with another item, so it moved to %s.", e.title, p))
		}
		c.paths[p] = true
		e.permalink = p
		if oldKey != "" {
			c.redirects[oldKey] = escapePath(p)
			c.byOldURL[oldKey] = p
			c.report.Permalinks.Redirected++
		} else {
			c.report.Permalinks.Kept++
			if c.report.Permalinks.Example == "" && e.kind == "post" {
				c.report.Permalinks.Example = p
			}
		}

		dir := "posts"
		if e.kind == "page" {
			dir = "pages"
		}
		base := sanitizeOutputPath(strings.ReplaceAll(slug, "/", "-"))
		file := "src/content/" + dir + "/" + base + ".md"
		if usedFiles[file] {
			file = "src/content/" + dir + "/" + base + "-" + strconv.Itoa(it.ID) + ".md"
		}
		usedFiles[file] = true
		e.file = file
	}

	// An empty page (and there are posts) is almost always the "posts page"
	// WordPress fills with the blog index. Render the post list there.
	hasPosts := false
	for _, e := range c.entries {
		if e.kind == "post" {
			hasPosts = true
			break
		}
	}
	for _, e := range c.entries {
		if e.kind == "page" && hasPosts && strings.TrimSpace(plainText(e.item.Content.Rendered)) == "" && !e.item.Content.Protected {
			e.postsIndex = true
		}
	}

	// Category and tag archives, at their current paths. Only terms that a
	// converted post uses get one: an empty archive is a page nobody needs.
	used := map[string]map[int]bool{"categories": {}, "tags": {}}
	for _, e := range c.entries {
		for _, id := range e.item.Categories {
			used["categories"][id] = true
		}
		for _, id := range e.item.Tags {
			used["tags"][id] = true
		}
	}
	for _, tax := range []struct {
		key  string
		name string
		src  map[int]wpTerm
	}{{"categories", "category", c.categories}, {"tags", "tag", c.tags}} {
		ids := make([]int, 0, len(tax.src))
		for id := range tax.src {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			t := tax.src[id]
			if t.Count == 0 || !used[tax.key][id] {
				continue
			}
			lu, err := url.Parse(t.Link)
			if err != nil || lu.Path == "" || lu.RawQuery != "" {
				continue // plain permalinks: /?cat=3, no clean path to keep
			}
			if c.paths[lu.Path] {
				continue
			}
			c.paths[lu.Path] = true
			c.terms[tax.key] = append(c.terms[tax.key], wpTermOut{
				Name: plainText(t.Name), Slug: t.Slug, Permalink: lu.Path, Taxonomy: tax.name, Count: t.Count,
			})
		}
	}
	c.report.Converted.Categories = len(c.terms["categories"])
	c.report.Converted.Tags = len(c.terms["tags"])

	// Feeds and sitemaps move; point the old addresses at the new ones. A site
	// installed in a subfolder (example.com/blog) had them under that folder.
	prefixes := []string{""}
	if hp := strings.TrimSuffix(c.home.Path, "/"); hp != "" {
		prefixes = append(prefixes, hp)
	}
	for _, prefix := range prefixes {
		for old, to := range map[string]string{
			"/feed/": "/rss.xml", "/feed": "/rss.xml", "/rss/": "/rss.xml",
			"/wp-sitemap.xml": "/sitemap-index.xml", "/sitemap.xml": "/sitemap-index.xml",
			"/sitemap_index.xml": "/sitemap-index.xml",
		} {
			if !c.paths[prefix+old] {
				c.redirects[prefix+old] = to
			}
		}
	}
	if !c.report.Site.PrettyPermalinks {
		c.redirects["/?feed=rss2"] = "/rss.xml"
	}
}

// convertContent turns every entry's HTML into Markdown. With images == nil it
// only collects image URLs; with a map it writes local paths for downloaded
// images and leaves the rest at their absolute URL.
func (c *wpConversion) convertContent(images map[string]string) {
	collect := images == nil
	seen := map[string]bool{}
	if collect {
		c.imageOrder = nil
		c.imageFetch = map[string]string{}
	}
	addImage := func(abs, fetch string) {
		if collect && !seen[abs] {
			seen[abs] = true
			c.imageOrder = append(c.imageOrder, abs)
			c.imageFetch[abs] = fetch
		}
	}
	c.report.Converted.RawHTMLBlocks = 0
	c.report.PostsWithForms = 0
	shortcodes := map[string]bool{}

	for _, e := range c.entries {
		base, _ := url.Parse(e.item.Link)
		if base == nil {
			base = c.home
		}
		if e.featured != "" {
			if u, err := url.Parse(e.featured); err == nil {
				if u = unPhoton(u, c.rest); c.rest.ownHost(u) {
					e.featured = u.String()
					addImage(stripImageURL(u), stripImageURL(u))
				}
			}
		}
		if e.item.Content.Protected {
			e.md = mdResult{Markdown: "This post was password-protected on WordPress, so its content is not public and was not converted.\n"}
			continue
		}
		e.md = htmlToMarkdown(e.item.Content.Rendered, func(kind, raw string) string {
			u := resolveRef(base, raw)
			if u == nil {
				return raw
			}
			if kind == "img" {
				orig := u
				u = unPhoton(u, c.rest)
				if !c.rest.ownHost(u) {
					return u.String()
				}
				abs := stripImageURL(u)
				fetch := abs
				if orig != u {
					fetch = orig.String()
				}
				addImage(abs, fetch)
				if local := images[abs]; local != "" {
					return local
				}
				return u.String()
			}
			return c.rewriteLink(u, images)
		})
		c.report.Converted.RawHTMLBlocks += e.md.RawBlocks
		if e.md.Forms > 0 {
			c.report.PostsWithForms++
		}
		for _, s := range e.md.Shortcodes {
			shortcodes[s] = true
		}
	}
	c.report.Shortcodes = c.report.Shortcodes[:0]
	for s := range shortcodes {
		c.report.Shortcodes = append(c.report.Shortcodes, s)
	}
	sort.Strings(c.report.Shortcodes)
}

// rewriteLink keeps links to things the new site serves root-relative (so they
// work on any domain) and leaves everything else exactly where it points now.
func (c *wpConversion) rewriteLink(u *url.URL, images map[string]string) string {
	if !c.rest.ownHost(u) {
		return u.String()
	}
	frag := ""
	if u.Fragment != "" {
		frag = "#" + u.EscapedFragment()
	}
	if u.RawQuery != "" {
		key := u.EscapedPath() + "?" + u.RawQuery
		if u.Path == "" {
			key = "/?" + u.RawQuery
		}
		if p, ok := c.byOldURL[key]; ok {
			return escapePath(p) + frag
		}
		return u.String()
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	if c.paths[p] || p == "/" {
		return u.EscapedPath() + frag
	}
	if local := images[stripImageURL(u)]; local != "" {
		return local
	}
	return u.String()
}

// downloadImages saves same-site images into public/, keeping their original
// path (/wp-content/uploads/2024/05/x.jpg stays at that URL). It stops at the
// shared byte budget and leaves the rest pointing at the old site.
func (c *wpConversion) downloadImages(ctx context.Context) {
	c.images = map[string]string{}
	type result struct {
		abs  string
		body []byte
	}
	jobs := make(chan string)
	results := make(chan result)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var budget = c.lim.ImageBytes
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for abs := range jobs {
				mu.Lock()
				left := budget
				mu.Unlock()
				if left <= 0 || time.Now().After(c.rest.deadline) {
					results <- result{abs: abs}
					continue
				}
				fetch := c.imageFetch[abs]
				if fetch == "" {
					fetch = abs
				}
				// A CDN copy that fails goes straight to the original: no
				// point waiting out the CDN's rate limit.
				body := c.fetchImage(ctx, fetch, left, fetch == abs)
				if body == nil && fetch != abs {
					// The image CDN's resized copy failed (Jetpack's CDN
					// rate-limits a burst): the original on the site is the
					// same picture, just bigger.
					body = c.fetchImage(ctx, abs, left, true)
				}
				if body != nil {
					mu.Lock()
					if int64(len(body)) > budget {
						body = nil
					} else {
						budget -= int64(len(body))
					}
					mu.Unlock()
				}
				results <- result{abs: abs, body: body}
			}
		}()
	}
	go func() {
		for _, abs := range c.imageOrder {
			jobs <- abs
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	got := map[string][]byte{}
	done := 0
	for r := range results {
		if r.body != nil {
			got[r.abs] = r.body
		}
		done++
		c.progress("images", done, len(c.imageOrder))
	}
	usedPath := map[string]bool{}
	for _, abs := range c.imageOrder {
		body, ok := got[abs]
		if !ok {
			c.report.Converted.ImagesRemote++
			continue
		}
		local := localImagePath(abs)
		if usedPath[local] {
			// Another URL for the same file (www. and bare host, say).
			c.images[abs] = local
			continue
		}
		usedPath[local] = true
		c.images[abs] = local
		c.imgBytes += int64(len(body))
		c.files = append(c.files, File{Path: "public" + localFilePath(local), Content: string(body), Size: len(body)})
		delete(got, abs) // the string is a copy: let the bytes go
		c.report.Converted.ImagesDownloaded++
	}
}

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".svg": true, ".avif": true, ".ico": true, ".bmp": true,
}

// fetchImage downloads one image. With retry, an HTTP 429 is retried from
// the same budget as the REST calls. It returns nil on any failure.
func (c *wpConversion) fetchImage(ctx context.Context, fetchURL string, left int64, retry bool) []byte {
	body, hdr, status := c.fetchImageOnce(ctx, fetchURL, left)
	if retry && status == http.StatusTooManyRequests && c.rest.backoff(ctx, hdr) {
		body, _, _ = c.fetchImageOnce(ctx, fetchURL, left)
	}
	return body
}

func (c *wpConversion) fetchImageOnce(ctx context.Context, fetchURL string, left int64) ([]byte, http.Header, int) {
	limit := c.lim.ImageFileBytes
	if left < limit {
		limit = left
	}
	ctx, cancel := context.WithDeadline(ctx, c.rest.deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, nil, 0
	}
	req.Header.Set("User-Agent", c.rest.userAgent)
	// No webp/avif: a CDN would answer a .jpg path with webp bytes.
	req.Header.Set("Accept", "image/jpeg,image/png,image/gif,image/svg+xml;q=0.9,*/*;q=0.5")
	resp, err := c.rest.client.Do(req)
	if err != nil {
		return nil, nil, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.Header, resp.StatusCode
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	u, _ := url.Parse(fetchURL)
	if !strings.HasPrefix(mt, "image/") && !(u != nil && imageExts[strings.ToLower(path.Ext(u.Path))] && (mt == "" || mt == "application/octet-stream")) {
		return nil, resp.Header, resp.StatusCode
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, resp.Header, resp.StatusCode
	}
	return body, resp.Header, resp.StatusCode
}

// commonNotes are shared by the preview and the full conversion.
func (c *wpConversion) commonNotes() {
	r := c.report
	if c.rest.wasThrottled() {
		r.Throttled = true
		r.Notes = append(r.Notes, c.home.Host+" rate-limited some of our requests (HTTP 429), so some counts here may be missing. Try again in a minute.")
	}
	// A URL with a path whose API lists nothing is most often a sub-directory
	// install read through the wrong API. Say so rather than look empty.
	if strings.Trim(c.start.Path, "/") != "" && r.Found.Posts == 0 && r.Found.Pages <= 0 {
		own := url.URL{Scheme: c.start.Scheme, Host: c.start.Host, Path: strings.TrimSuffix(c.start.Path, "/") + "/wp-json/"}
		r.Notes = append(r.Notes, fmt.Sprintf(
			"The WordPress REST API we read (%s) lists no published posts or pages. If %s is a blog in a sub-directory, it may be a separate WordPress install from the one at that API: check that %s is publicly readable.",
			r.Site.RESTAPI, c.start.String(), own.String()))
	}
}

func (c *wpConversion) finishReport() {
	c.commonNotes()
	if c.report.Truncated {
		c.report.Notes = append(c.report.Notes, fmt.Sprintf(
			"Your site has %s and %s; this conversion includes the newest %d posts and %d pages. Raise the post limit (up to %d) or add the rest by hand.",
			countPhrase(c.report.Found.Posts, "post"), countPhrase(c.report.Found.Pages, "page"),
			c.report.Converted.Posts, c.report.Converted.Pages, c.lim.MaxPosts))
	}
	if c.report.Converted.ImagesRemote > 0 {
		c.report.Notes = append(c.report.Notes, fmt.Sprintf(
			"%d %s still load from their current address (hosted on another domain, over the %s image budget, or failed to download). They stop working if that address goes away.",
			c.report.Converted.ImagesRemote, pluralizeWord("image", c.report.Converted.ImagesRemote), humanBytes(c.lim.ImageBytes)))
	}
	if c.report.Converted.RawHTMLBlocks > 0 {
		c.report.Notes = append(c.report.Notes, fmt.Sprintf(
			"%d %s cleanly to Markdown (tables, embeds, galleries, page-builder layouts) and %s kept as HTML inside the Markdown files, so nothing is lost. They may look plainer without your theme's styles.",
			c.report.Converted.RawHTMLBlocks, pluralizeWordVerb(c.report.Converted.RawHTMLBlocks, "block did not convert", "blocks did not convert"),
			pluralizeWordVerb(c.report.Converted.RawHTMLBlocks, "was", "were")))
	}
	if len(c.report.Shortcodes) > 0 {
		c.report.Notes = append(c.report.Notes, "Unrendered shortcodes were left in the text as-is: ["+strings.Join(limitList(c.report.Shortcodes, 8), "], [")+"]. Search the Markdown for them and replace each with real content.")
	}
	if c.report.PostsWithForms > 0 {
		c.report.Notes = append(c.report.Notes, fmt.Sprintf(
			"%d %s a form that posted back to WordPress. The form is still in the page but will not submit; point it at a form service.",
			c.report.PostsWithForms, pluralizeWordVerb(c.report.PostsWithForms, "item contains", "items contain")))
	}
	for _, e := range c.entries {
		if e.item.Content.Protected {
			c.report.Protected++
		}
	}
	if c.report.Protected > 0 {
		c.report.Notes = append(c.report.Notes, fmt.Sprintf(
			"%d password-protected %s converted as a placeholder only, because the content is not public.",
			c.report.Protected, pluralizeWordVerb(c.report.Protected, "item was", "items were")))
	}
	if c.report.Permalinks.Redirected > 0 {
		c.report.Notes = append(c.report.Notes, fmt.Sprintf(
			"%d old %s now 301-redirect to a clean path (see redirects.json).",
			c.report.Permalinks.Redirected, pluralizeWord("URL", c.report.Permalinks.Redirected)))
	}
}

func (c *wpConversion) previewNotes() {
	c.commonNotes()
	r := c.report
	if r.Truncated {
		r.Notes = append(r.Notes, fmt.Sprintf("Your site has %s and %s. A conversion includes the newest %d posts (up to %d on request) and %d pages.",
			countPhrase(r.Found.Posts, "post"), countPhrase(r.Found.Pages, "page"), c.lim.DefaultPosts, c.lim.MaxPosts, c.lim.MaxPages))
	}
	if !r.Site.PrettyPermalinks {
		r.Notes = append(r.Notes, "Your site uses plain ?p=123 links. Each post gets a clean path and the old links 301-redirect to it.")
	}
	if r.Sample != nil && r.Sample.RawHTML > 0 {
		r.Notes = append(r.Notes, "Some blocks in the sample are kept as HTML inside the Markdown because they don't convert cleanly; nothing is dropped.")
	}
	if len(r.Shortcodes) > 0 {
		r.Notes = append(r.Notes, "The sample contains unrendered shortcodes: ["+strings.Join(limitList(r.Shortcodes, 8), "], [")+"]. They come across as plain text.")
	}
}

// ------------------------------------------------------------------ helpers

var tagRe = regexp.MustCompile(`<[^>]*>`)

// plainText strips tags and decodes entities: WordPress titles are HTML.
func plainText(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

var readMoreRe = regexp.MustCompile(`(?is)<a[^>]*class="[^"]*more-link[^"]*"[^>]*>.*?</a>`)

func cleanExcerpt(s string) string {
	s = readMoreRe.ReplaceAllString(s, "")
	t := plainText(s)
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(t, "[…]"), "[...]"))
	t = strings.TrimSuffix(t, "Continue reading")
	t = strings.TrimSpace(t)
	if r := []rune(t); len(r) > 300 {
		t = strings.TrimSpace(string(r[:297])) + "..."
	}
	return t
}

// wpTime turns a REST date into RFC 3339. *_gmt fields carry no zone.
func wpTime(gmt, local string) string {
	if gmt != "" && !strings.HasPrefix(gmt, "0000") {
		if t, err := time.Parse("2006-01-02T15:04:05", gmt); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	if t, err := time.Parse("2006-01-02T15:04:05", local); err == nil {
		return t.Format("2006-01-02T15:04:05") + "Z"
	}
	return ""
}

func sameURLPath(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

var photonHostRe = regexp.MustCompile(`^i[0-3]\.wp\.com$`)

// unPhoton maps a Jetpack image CDN URL (https://i0.wp.com/example.com/wp-content/...)
// back to the original file on the site, so it is downloaded like any other
// same-site image instead of staying on Jetpack's CDN.
func unPhoton(u *url.URL, rest *wpREST) *url.URL {
	if !photonHostRe.MatchString(strings.ToLower(u.Hostname())) {
		return u
	}
	host, rest2, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if !ok || !rest.hosts[strings.ToLower(host)] {
		return u
	}
	scheme := "https"
	if rest.root != nil {
		scheme = rest.root.Scheme
	}
	return &url.URL{Scheme: scheme, Host: host, Path: "/" + rest2}
}

// stripImageURL drops the query (Jetpack/Photon ?resize= variants are the
// same file) and the fragment.
func stripImageURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.Fragment = ""
	c.RawFragment = ""
	return c.String()
}

// localImagePath is the URL path an image is served at in the new site: its
// original path, cleaned.
func localImagePath(abs string) string {
	u, err := url.Parse(abs)
	if err != nil {
		return ""
	}
	return escapePath("/" + sanitizeOutputPath(u.Path))
}

// localFilePath converts a served URL path back to the file path under public/.
func localFilePath(served string) string {
	if p, err := url.PathUnescape(served); err == nil {
		return p
	}
	return served
}

func escapePath(p string) string {
	return (&url.URL{Path: p}).EscapedPath()
}

func countPhrase(n int, noun string) string {
	if n < 0 {
		return "an unknown number of " + noun + "s"
	}
	return strconv.Itoa(n) + " " + pluralizeWord(noun, n)
}

func pluralizeWordVerb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func limitList(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
