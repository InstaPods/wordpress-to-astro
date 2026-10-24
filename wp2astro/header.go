// Package wp2astro converts a public WordPress site into an Astro 5 project.
//
// It reads the site through its REST API (the same JSON any visitor can
// fetch) and turns the published posts and pages into Markdown content
// collections, the site's own images, and every post at the path it has
// today. It never logs in and never touches the database, so it can only
// ever see what is already public.
//
//	c := &wp2astro.Converter{}
//	p, err := c.Convert(ctx, "https://example.com", wp2astro.Options{})
//	// p.Files is the project; p.Report says what came across.
package wp2astro

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Version is the library version, used in the default User-Agent.
const Version = "0.1.0"

// DefaultUserAgent identifies the converter to the sites it reads.
const DefaultUserAgent = "wordpress-to-astro/" + Version + " (+https://github.com/InstaPods/wordpress-to-astro)"

const (
	// The preview reads one page of posts and converts the first one.
	wpPreviewSamplePosts = 3
	wpPreviewSampleBytes = 32 << 10

	wpPerPage          = 50
	wpMaxResponseBytes = 16 << 20
	wpHTMLProbeBytes   = 512 << 10
)

// Limits bounds one run. A zero field means "use the default" from
// DefaultLimits (or DefaultPreviewLimits for a preview). Those defaults are
// sized for a run on your own machine and are generous: a server converting
// sites for other people should set every field.
type Limits struct {
	// DefaultPosts is how many posts are converted when Options.MaxPosts is 0.
	DefaultPosts int
	// MaxPosts is the most Options.MaxPosts may ask for.
	MaxPosts int
	MaxPages int
	MaxTerms int
	// JSONBytes bounds all REST responses read in one run.
	JSONBytes int64
	// ImageBytes bounds all images downloaded in one run; ImageFileBytes one image.
	ImageBytes     int64
	ImageFileBytes int64
	// Timeout bounds the whole run.
	Timeout time.Duration
	// MaxRequests caps outbound requests in one run. 0 means the default,
	// negative means no cap.
	MaxRequests int
	// MaxRetries is how many HTTP 429 answers are retried in one run, each
	// after RetryBackoff (or the site's Retry-After, up to MaxRetryWait).
	// 0 means the default, negative means no retries.
	MaxRetries   int
	RetryBackoff time.Duration
	MaxRetryWait time.Duration
}

// DefaultLimits suit a run on your own machine: every post, every image.
func DefaultLimits() Limits {
	return Limits{
		DefaultPosts:   100000,
		MaxPosts:       100000,
		MaxPages:       10000,
		MaxTerms:       10000,
		JSONBytes:      2 << 30,
		ImageBytes:     2 << 30,
		ImageFileBytes: 64 << 20,
		Timeout:        30 * time.Minute,
		MaxRequests:    0,
		MaxRetries:     30,
		RetryBackoff:   2 * time.Second,
		MaxRetryWait:   60 * time.Second,
	}
}

// DefaultPreviewLimits keep a preview small: a few requests, no images.
func DefaultPreviewLimits() Limits {
	l := DefaultLimits()
	l.JSONBytes = 4 << 20
	l.Timeout = 20 * time.Second
	l.MaxRequests = 25
	return l
}

// withDefaults fills zero fields from d.
func (l Limits) withDefaults(d Limits) Limits {
	if l.DefaultPosts <= 0 {
		l.DefaultPosts = d.DefaultPosts
	}
	if l.MaxPosts <= 0 {
		l.MaxPosts = d.MaxPosts
	}
	if l.MaxPages <= 0 {
		l.MaxPages = d.MaxPages
	}
	if l.MaxTerms <= 0 {
		l.MaxTerms = d.MaxTerms
	}
	if l.JSONBytes <= 0 {
		l.JSONBytes = d.JSONBytes
	}
	if l.ImageBytes <= 0 {
		l.ImageBytes = d.ImageBytes
	}
	if l.ImageFileBytes <= 0 {
		l.ImageFileBytes = d.ImageFileBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = d.Timeout
	}
	switch {
	case l.MaxRequests == 0:
		l.MaxRequests = d.MaxRequests
	case l.MaxRequests < 0:
		l.MaxRequests = 0 // no cap
	}
	switch {
	case l.MaxRetries == 0:
		l.MaxRetries = d.MaxRetries
	case l.MaxRetries < 0:
		l.MaxRetries = 0 // no retries
	}
	if l.RetryBackoff <= 0 {
		l.RetryBackoff = d.RetryBackoff
	}
	if l.MaxRetryWait <= 0 {
		l.MaxRetryWait = d.MaxRetryWait
	}
	return l
}

// Options tunes one conversion. Zero values mean defaults.
type Options struct {
	// MaxPosts caps how many posts are converted, newest first.
	MaxPosts int
}

func (o Options) maxPosts(l Limits) int {
	switch {
	case o.MaxPosts <= 0:
		return l.DefaultPosts
	case o.MaxPosts > l.MaxPosts:
		return l.MaxPosts
	}
	return o.MaxPosts
}

// Converter reads WordPress sites. The zero value is ready to use.
type Converter struct {
	// Client makes every request. nil means a plain client with a per-request
	// timeout. A server that converts sites for anonymous users should pass a
	// client that refuses private addresses (an SSRF guard).
	Client *http.Client
	// UserAgent is sent with every request. Empty means DefaultUserAgent.
	UserAgent string
	// Limits bounds Convert; PreviewLimits bounds Preview.
	Limits        Limits
	PreviewLimits Limits
	// Generator names the tool in the project README, e.g. "the hosted
	// converter at example.com". Empty means a link to this project.
	Generator string
	// DeploySection replaces the README's "## Deploy" section body.
	DeploySection string
	// Summary writes MIGRATION-NOTES.md. nil means DefaultSummary.
	Summary func(*Report) Summary
	// OnDiscover, when set, is called once the site's REST API is found and
	// before any content is read. Returning an error stops the conversion.
	OnDiscover func(Site) error
	// Progress, when set, is called as items are read and images downloaded.
	// stage is "posts", "pages" or "images" (total is -1 when unknown), or
	// "throttled" when the site answered HTTP 429 and the converter waits
	// done milliseconds before retrying. Calls never overlap for "posts",
	// "pages" and "images"; "throttled" may come from any goroutine.
	Progress func(stage string, done, total int)
}

func (w *Converter) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return defaultClient
}

var defaultClient = &http.Client{Timeout: 60 * time.Second}

func (w *Converter) userAgent() string {
	if w.UserAgent != "" {
		return w.UserAgent
	}
	return DefaultUserAgent
}

func (w *Converter) limits() Limits { return w.Limits.withDefaults(DefaultLimits()) }

func (w *Converter) previewLimits() Limits {
	return w.PreviewLimits.withDefaults(DefaultPreviewLimits())
}

// File is one file of the generated project. Path is relative, with forward
// slashes and no "..": safe to join under an output directory.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Size    int    `json:"size,omitempty"`
}

// Project is a converted site.
type Project struct {
	// SourceURL is the URL that was read, normalized.
	SourceURL string
	// Host is the WordPress site's host, e.g. example.com.
	Host string
	// Files is the Astro project: config, layouts, Markdown and images.
	Files  []File
	Report *Report
}

// Report is what we found on the site and what the conversion did with it.
// Preview returns it on its own; Convert attaches it to the project.
type Report struct {
	Site Site `json:"site"`
	// Found is what the REST API reports as published (X-WP-Total). -1 means
	// the count was not available.
	Found Counts `json:"found"`
	// Converted is what ended up in the project. Zero in a preview.
	Converted  Converted  `json:"converted"`
	Permalinks Permalinks `json:"permalinks"`
	// CustomTypes are public content types other than posts and pages
	// (WooCommerce products, portfolio items...). They are not converted.
	CustomTypes []string `json:"custom_types"`
	// Shortcodes are unrendered shortcode names left in the content.
	Shortcodes []string `json:"shortcodes"`
	// PostsWithForms counts converted items containing a <form>.
	PostsWithForms int `json:"posts_with_forms"`
	// Protected counts password-protected items (their content is not public).
	Protected int `json:"protected"`
	// Truncated is true when the site has more posts or pages than we convert.
	Truncated bool     `json:"truncated"`
	Sample    *Sample  `json:"sample,omitempty"`
	Notes     []string `json:"notes"`
	// Throttled is true when the site answered HTTP 429 to some request, so
	// some counts may be missing. Such a report should not be cached.
	Throttled bool `json:"-"`
}

type Site struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Home        string `json:"home"`
	// RESTAPI is the API root we read from, e.g. https://example.com/wp-json/.
	RESTAPI string `json:"rest_api"`
	// PrettyPermalinks is false when the site uses ?p=123 style URLs.
	PrettyPermalinks bool `json:"pretty_permalinks"`
}

type Counts struct {
	Posts      int `json:"posts"`
	Pages      int `json:"pages"`
	Categories int `json:"categories"`
	Tags       int `json:"tags"`
	Comments   int `json:"comments"`
}

type Converted struct {
	Posts            int `json:"posts"`
	Pages            int `json:"pages"`
	Categories       int `json:"categories"`
	Tags             int `json:"tags"`
	ImagesDownloaded int `json:"images_downloaded"`
	// ImagesRemote are images left pointing at their current URL (hosted on
	// another domain, over the size budget, or failed to download).
	ImagesRemote  int `json:"images_remote"`
	RawHTMLBlocks int `json:"raw_html_blocks"`
}

type Permalinks struct {
	// Kept counts posts and pages built at exactly their current path.
	Kept int `json:"kept"`
	// Redirected counts old URLs that get a 301 to a new path.
	Redirected int    `json:"redirected"`
	Example    string `json:"example,omitempty"`
}

// Sample is one post as it will look in the project.
type Sample struct {
	Title       string `json:"title"`
	SourceURL   string `json:"source_url"`
	Permalink   string `json:"permalink"`
	Path        string `json:"path"` // file path inside the project
	Frontmatter string `json:"frontmatter"`
	Markdown    string `json:"markdown"`
	Images      int    `json:"images"`
	RawHTML     int    `json:"raw_html_blocks"`
}

// RESTError means the site could not be read through the REST API.
type RESTError struct {
	URL         string
	IsWordPress bool
	Status      int
}

func (e *RESTError) Error() string {
	host := hostOf(e.URL)
	if e.IsWordPress {
		return fmt.Sprintf("%s runs WordPress, but its REST API is turned off or blocked (often by a security plugin), so its posts can't be read. "+
			"Allow public read access to /wp-json/wp/v2/ while you convert.", host)
	}
	return fmt.Sprintf("No WordPress REST API found at %s. Is it a WordPress site, and is it public?", host)
}

// ThrottledError means the site rate-limited us (HTTP 429) before we could
// read it. It is transient: the same request usually works a minute later.
type ThrottledError struct {
	URL string
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("%s is rate-limiting our requests right now (HTTP 429), so we couldn't read it. Try again in a minute.", hostOf(e.URL))
}

// UnavailableError means the REST API the site advertises on its own host
// failed (5xx, timeout, connection error). Transient, like a 429.
type UnavailableError struct {
	URL string
	API string
	Err error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("The REST API %s advertises (%s) did not answer (%v). Try again in a minute.", hostOf(e.URL), e.API, e.Err)
}

func (e *UnavailableError) Unwrap() error { return e.Err }

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
