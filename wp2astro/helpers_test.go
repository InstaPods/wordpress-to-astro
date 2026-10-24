package wp2astro

import "testing"

func TestSanitizeOutputPathIsSafeOnWindows(t *testing.T) {
	for in, want := range map[string]string{
		"wp-content/uploads/CON.jpg": "wp-content/uploads/_CON.jpg",
		"aux/x.png":                  "_aux/x.png",
		"nul":                        "_nul",
		"foo./bar..":                 "foo/bar",
		"console.png":                "console.png",
		"a/com10.md":                 "a/com10.md",
	} {
		if got := sanitizeOutputPath(in); got != want {
			t.Errorf("sanitizeOutputPath(%q) = %q, want %q", in, got, want)
		}
	}
	for p, want := range map[string]bool{"C:foo": false, "a/b:c": false, "a/b.md": true, `a\b`: false} {
		if SafeRelativePath(p) != want {
			t.Errorf("SafeRelativePath(%q) = %v, want %v", p, !want, want)
		}
	}
}
