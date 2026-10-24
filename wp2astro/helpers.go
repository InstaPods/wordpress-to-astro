package wp2astro

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func normalizeStartURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("a URL is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("that doesn't look like a URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http:// and https:// URLs can be converted, not %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("that URL has no hostname")
	}
	return stripFragment(u), nil
}

func stripFragment(u *url.URL) *url.URL {
	c := *u
	c.Fragment = ""
	c.RawFragment = ""
	if c.Path == "" {
		c.Path = "/"
	}
	return &c
}

// sanitizeOutputPath produces a relative, traversal-free path, cleaning every
// segment of characters that are invalid in a file name on some system.
func sanitizeOutputPath(p string) string {
	unescaped, err := url.PathUnescape(p)
	if err == nil {
		p = unescaped
	}
	p = path.Clean("/" + p)
	p = strings.TrimPrefix(p, "/")
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		s = strings.TrimSpace(s)
		s = strings.Map(func(r rune) rune {
			switch {
			case r < 0x20, r == 0x7f, r == ':', r == '*', r == '?', r == '"', r == '<', r == '>', r == '|', r == '\\':
				return '-'
			}
			return r
		}, s)
		// Windows drops trailing dots from a name, so "a." and "a" collide.
		s = strings.TrimRight(s, ".")
		if s == "" {
			continue
		}
		if windowsReserved(s) {
			s = "_" + s
		}
		if len(s) > 100 {
			ext := path.Ext(s)
			cut := 100 - len(ext)
			// Never split a multi-byte character: long non-ASCII slugs are
			// the norm on Russian, Chinese and Arabic sites.
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			s = s[:cut] + ext
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return "index.html"
	}
	return strings.Join(out, "/")
}

// windowsReserved reports whether a path segment is a device name Windows
// won't create as a file (CON, NUL, COM1...), with or without an extension.
func windowsReserved(seg string) bool {
	base, _, _ := strings.Cut(seg, ".")
	switch strings.ToUpper(strings.TrimRight(base, " ")) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

// SafeRelativePath reports whether p can be written under an output
// directory: a relative path with no ".." segment, no backslash and no colon
// (a Windows drive or NTFS stream). It allows ".." inside a name, because
// Astro's rest routes are files like "src/pages/[...path].astro".
func SafeRelativePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\:") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatInt(n/(1<<10), 10) + " KB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

func pluralizeWord(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// cleanFetchError strips Go's transport wrapping so the user sees the cause and
// not `Get "https://...": dial tcp ...`.
func cleanFetchError(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i > 0 && strings.HasPrefix(msg, "Get \"") {
		msg = msg[i+2:]
	}
	return fmt.Errorf("%s", msg)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// resolveRef turns an attribute value into an absolute http(s) URL, or nil for
// anything we must not touch (data:, mailto:, tel:, javascript:, #fragments).
func resolveRef(base *url.URL, raw string) *url.URL {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return nil
	}
	lower := strings.ToLower(raw)
	for _, bad := range []string{"data:", "mailto:", "tel:", "javascript:", "blob:", "about:", "sms:"} {
		if strings.HasPrefix(lower, bad) {
			return nil
		}
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	abs := base.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return nil
	}
	return stripFragment(abs)
}

// Slugify produces a short package-name slug: lowercase, ASCII,
// hyphen-separated, at most 32 characters.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case unicode.IsSpace(r), r == '-', r == '_', r == '.':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 32 {
		out = strings.TrimRight(out[:32], "-")
	}
	return out
}

func defaultSlug(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
