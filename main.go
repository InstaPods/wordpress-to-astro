// Command wordpress-to-astro converts a public WordPress site into an Astro
// project on your own machine.
//
//	wordpress-to-astro https://example.com [--out dir] [--max-posts N]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/instapods/wordpress-to-astro/wp2astro"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `wordpress-to-astro converts a public WordPress site into an Astro project.

Usage:
  wordpress-to-astro <site-url> [flags]

Examples:
  wordpress-to-astro https://example.com
  wordpress-to-astro https://example.com/blog/ --out my-blog --max-posts 200
  wordpress-to-astro https://example.com --dry-run

Flags:
  -o, --out <dir>       Where to write the project (default: a folder named after the site)
  -n, --max-posts <N>   Convert only the newest N posts (default: all)
      --dry-run         Read a few posts and show what would convert; write nothing
      --force           Write into --out even if it is not empty
      --timeout <dur>   Give up after this long (default 30m)
      --json            Print the conversion report as JSON on stdout
  -q, --quiet           No progress output
  -v, --version         Print the version

It reads only what the site's public REST API shows: no login, no database.
Docs and issues: https://github.com/InstaPods/wordpress-to-astro
`

type options struct {
	timeoutSet bool
	url        string
	out        string
	maxPosts   int
	dryRun     bool
	force      bool
	timeout    time.Duration
	json       bool
	quiet      bool
	version    bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n\nRun wordpress-to-astro --help for usage.\n", err)
		return 2
	}
	if opts.version {
		fmt.Fprintln(stdout, "wordpress-to-astro", version)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	p := newProgress(stderr, opts.quiet || opts.json)
	conv := &wp2astro.Converter{Progress: p.update}
	if opts.timeout > 0 {
		conv.Limits.Timeout = opts.timeout
		if opts.timeoutSet {
			conv.PreviewLimits.Timeout = opts.timeout
		}
	}
	if version != "dev" {
		conv.UserAgent = "wordpress-to-astro/" + strings.TrimPrefix(version, "v") + " (+https://github.com/InstaPods/wordpress-to-astro)"
	}

	if opts.dryRun {
		p.say("Reading %s ...", opts.url)
		r, err := conv.Preview(ctx, opts.url)
		p.done()
		if interrupted(ctx) {
			fmt.Fprintln(stderr, "Interrupted.")
			return 130
		}
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", timeoutHint(err))
			return 1
		}
		if opts.json {
			return printJSON(stdout, stderr, r)
		}
		printPreview(stdout, r)
		return 0
	}

	// Check the output folder as soon as the site's name is known, before
	// anything is downloaded.
	out := opts.out
	conv.OnDiscover = func(s wp2astro.Site) error {
		if out == "" {
			out = defaultOutDir(s)
		}
		return checkOutDir(out, opts.force)
	}
	p.say("Reading %s ...", opts.url)
	proj, err := conv.Convert(ctx, opts.url, wp2astro.Options{MaxPosts: opts.maxPosts})
	p.done()
	if interrupted(ctx) {
		// What was read before Ctrl-C would make a project that looks
		// complete and isn't. Write nothing.
		fmt.Fprintln(stderr, "Interrupted. Nothing was written.")
		return 130
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", timeoutHint(err))
		return 1
	}
	n, size, err := writeProject(out, proj.Files)
	if err != nil {
		fmt.Fprintf(stderr, "error: writing the project: %v\n", err)
		return 1
	}
	if opts.json {
		return printJSON(stdout, stderr, proj.Report)
	}
	printResult(stdout, proj.Report, out, n, size)
	return 0
}

// parseArgs accepts flags before or after the URL, as people type them.
func parseArgs(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("wordpress-to-astro", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.out, "out", "", "")
	fs.StringVar(&o.out, "o", "", "")
	fs.IntVar(&o.maxPosts, "max-posts", 0, "")
	fs.IntVar(&o.maxPosts, "n", 0, "")
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.BoolVar(&o.force, "force", false, "")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Minute, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.quiet, "quiet", false, "")
	fs.BoolVar(&o.quiet, "q", false, "")
	fs.BoolVar(&o.version, "version", false, "")
	fs.BoolVar(&o.version, "v", false, "")

	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return o, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			o.timeoutSet = true
		}
	})
	if o.version {
		return o, nil
	}
	switch len(positional) {
	case 0:
		return o, errors.New("give the address of a WordPress site, e.g. wordpress-to-astro https://example.com")
	case 1:
		o.url = positional[0]
	default:
		return o, fmt.Errorf("expected one site URL, got %d: %s", len(positional), strings.Join(positional, " "))
	}
	if o.maxPosts < 0 {
		return o, errors.New("--max-posts must be 0 (all) or more")
	}
	return o, nil
}

// interrupted reports whether the user pressed Ctrl-C (not a --timeout).
func interrupted(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

func timeoutHint(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") {
		return fmt.Errorf("%w (the site was too slow for the time limit; raise --timeout)", err)
	}
	return err
}

func defaultOutDir(s wp2astro.Site) string {
	name := wp2astro.Slugify(s.Name)
	if name == "" {
		if u, err := url.Parse(s.Home); err == nil {
			name = wp2astro.Slugify(u.Hostname())
		}
	}
	if name == "" {
		name = "wordpress-site"
	}
	return name + "-astro"
}

func checkOutDir(dir string, force bool) error {
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		return fmt.Errorf("%s is a file, not a folder. Pick another --out", dir)
	}
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case len(entries) > 0 && !force:
		return fmt.Errorf("%s already exists and is not empty. Pick another folder with --out, or pass --force to write into it", dir)
	}
	return nil
}

func writeProject(dir string, files []wp2astro.File) (int, int64, error) {
	// Check every path first, so one bad name can't leave half a project.
	for _, f := range files {
		if !wp2astro.SafeRelativePath(f.Path) || !filepath.IsLocal(filepath.FromSlash(f.Path)) {
			return 0, 0, fmt.Errorf("refusing unsafe path %q", f.Path)
		}
	}
	var size int64
	for _, f := range files {
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, 0, err
		}
		if err := os.WriteFile(dst, []byte(f.Content), 0o644); err != nil {
			return 0, 0, err
		}
		size += int64(len(f.Content))
	}
	return len(files), size, nil
}

func printJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func printResult(w io.Writer, r *wp2astro.Report, out string, n int, size int64) {
	c := r.Converted
	fmt.Fprintf(w, "\nConverted %s (%s)\n", r.Site.Name, r.Site.Home)
	fmt.Fprintf(w, "  %d posts, %d pages, %d categories, %d tags\n", c.Posts, c.Pages, c.Categories, c.Tags)
	fmt.Fprintf(w, "  %d images downloaded", c.ImagesDownloaded)
	if c.ImagesRemote > 0 {
		fmt.Fprintf(w, ", %d left at their current address", c.ImagesRemote)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %d URLs kept as they are", r.Permalinks.Kept)
	if r.Permalinks.Redirected > 0 {
		fmt.Fprintf(w, ", %d redirected (301)", r.Permalinks.Redirected)
	}
	fmt.Fprintln(w)
	printNotes(w, r.Notes)
	fmt.Fprintf(w, "\nWrote %d files (%s) to %s\n", n, humanSize(size), out)
	fmt.Fprintf(w, "\nNext:\n  cd %s\n  npm install\n  npm run dev\n\nMIGRATION-NOTES.md lists what did not come across.\n", shellQuote(out))
}

func printPreview(w io.Writer, r *wp2astro.Report) {
	f := r.Found
	fmt.Fprintf(w, "\n%s (%s)\n", r.Site.Name, r.Site.Home)
	fmt.Fprintf(w, "  REST API: %s\n", r.Site.RESTAPI)
	fmt.Fprintf(w, "  Found: %s posts, %s pages, %s categories, %s tags, %s comments (comments are not converted)\n",
		count(f.Posts), count(f.Pages), count(f.Categories), count(f.Tags), count(f.Comments))
	if r.Site.PrettyPermalinks {
		fmt.Fprintln(w, "  Permalinks: every post keeps its current URL")
	} else {
		fmt.Fprintln(w, "  Permalinks: plain ?p=123 links; each post gets a clean path and the old link 301-redirects")
	}
	if len(r.CustomTypes) > 0 {
		fmt.Fprintf(w, "  Not converted: %s\n", strings.Join(r.CustomTypes, ", "))
	}
	if s := r.Sample; s != nil {
		fmt.Fprintf(w, "\nSample: %q\n  %s -> %s\n  file: %s\n\n%s\n", s.Title, s.SourceURL, s.Permalink, s.Path, s.Frontmatter)
		md := s.Markdown
		if len(md) > 1200 {
			md = md[:1200] + "\n[...]"
		}
		fmt.Fprintln(w, strings.TrimSpace(md))
	}
	printNotes(w, r.Notes)
	fmt.Fprintln(w, "\nNothing was written. Run again without --dry-run to convert.")
}

func printNotes(w io.Writer, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(w, "\nNotes:")
	for _, n := range notes {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

func count(n int) string {
	if n < 0 {
		return "?"
	}
	return fmt.Sprint(n)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func shellQuote(s string) string {
	if strings.ContainsAny(s, " '\"$`\\") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// progress prints one updating status line on a terminal, and nothing when
// stderr is a file or pipe (or with --quiet).
type progress struct {
	mu     sync.Mutex
	w      io.Writer
	on     bool
	tty    bool
	last   time.Time
	active bool
}

func newProgress(w io.Writer, quiet bool) *progress {
	p := &progress{w: w, on: !quiet}
	if f, ok := w.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			p.tty = true
		}
	}
	return p
}

func (p *progress) say(format string, a ...any) {
	if p.on {
		fmt.Fprintf(p.w, format+"\n", a...)
	}
}

func (p *progress) update(stage string, done, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.on || (!p.tty && stage != "throttled") {
		return
	}
	if time.Since(p.last) < 100*time.Millisecond && done != total {
		return
	}
	if stage == "throttled" {
		// Always shown: otherwise a long wait looks like a hang.
		p.done()
		fmt.Fprintf(p.w, "The site is rate-limiting requests (HTTP 429); waiting %s and retrying.\n", time.Duration(done)*time.Millisecond)
		return
	}
	p.last = time.Now()
	label := map[string]string{"posts": "Reading posts", "pages": "Reading pages", "images": "Downloading images"}[stage]
	if label == "" {
		label = stage
	}
	line := fmt.Sprintf("%s: %d", label, done)
	if total > 0 {
		line = fmt.Sprintf("%s: %d/%d", label, done, total)
	}
	// Plain \r and padding: legacy Windows consoles print escape codes literally.
	fmt.Fprintf(p.w, "\r%-50s", line)
	p.active = true
}

func (p *progress) done() {
	if p.active && p.tty {
		fmt.Fprintf(p.w, "\r%50s\r", "")
		p.active = false
	}
}
