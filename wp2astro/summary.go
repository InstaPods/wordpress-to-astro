package wp2astro

import "strings"

// Summary is what a conversion carries across and what it leaves behind. It is
// written to MIGRATION-NOTES.md in every project.
type Summary struct {
	Transfers       []SummaryItem `json:"transfers"`
	DoesNotTransfer []SummaryItem `json:"does_not_transfer"`
	Notes           []string      `json:"notes,omitempty"`
}

type SummaryItem struct {
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// DefaultSummary describes a conversion. r may be nil (before any site is
// read); with a report the counts and per-run notes are filled in.
func DefaultSummary(r *Report) Summary {
	posts, images := "Published posts and pages", "Images stored on your site"
	if r != nil && (r.Converted.Posts > 0 || r.Converted.Pages > 0) {
		posts = "Published posts and pages (" + countPhrase(r.Converted.Posts, "post") + ", " + countPhrase(r.Converted.Pages, "page") + ")"
	}
	if r != nil && r.Converted.ImagesDownloaded > 0 {
		images = "Images stored on your site (" + countPhrase(r.Converted.ImagesDownloaded, "image") + ")"
	}
	s := Summary{
		Transfers: []SummaryItem{
			{
				Label: posts,
				Detail: "As Markdown files with title, date, last update, slug, categories, tags, excerpt " +
					"and featured image in the frontmatter. Blocks that don't convert cleanly are kept as HTML, not dropped.",
			},
			{
				Label: images,
				Detail: "Downloaded into the project at the same /wp-content/uploads/ path and re-linked. " +
					"Images hosted on another domain keep loading from there.",
			},
			{
				Label: "Your URLs",
				Detail: "Every post and page is built at the path it has today. When a path can't be kept " +
					"(plain ?p=123 links), the old URL gets a 301 redirect to the new one.",
			},
			{
				Label:  "Category and tag archives",
				Detail: "Rebuilt at their current paths, listing the posts in each.",
			},
			{
				Label:  "RSS feed and sitemap",
				Detail: "Generated on every build. The old /feed/ and sitemap addresses redirect to them.",
			},
		},
		DoesNotTransfer: []SummaryItem{
			{
				Label: "Your WordPress database",
				Detail: "Only what the public REST API shows is read; the database is never copied. Settings, " +
					"plugin data and anything not published stay in WordPress. Keep a backup before you shut it down.",
			},
			{
				Label: "Comments",
				Detail: "Comments are not converted. Export them from WordPress (Tools, then Export) if you " +
					"want to keep them, and add a comment service to the new site if you want new ones.",
			},
			{
				Label:  "Drafts, private, scheduled and password-protected posts",
				Detail: "Only what anyone can read publicly comes across. Copy unpublished posts across by hand.",
			},
			{
				Label: "Forms, search, shops and logins",
				Detail: "The new site is static: no PHP runs. Contact forms, site search, carts and member " +
					"areas stop working; replace them with a form or search service, or keep WordPress for those parts.",
			},
			{
				Label: "Shortcodes and plugin output",
				Detail: "The converter saves the HTML WordPress renders. Shortcodes from inactive plugins show up as plain " +
					"text like [gallery]; page-builder layouts are kept as HTML and look plainer without the theme.",
			},
			{
				Label: "Your theme, menus and widgets",
				Detail: "The REST API does not expose them. The project ships a plain, readable layout and a " +
					"menu built from your top-level pages; restyle it in src/layouts/Base.astro.",
			},
			{
				Label: "Secrets, API keys and auth settings",
				Detail: "Nothing from wp-config.php, plugin settings or logins is read. Re-enter any analytics or " +
					"service keys the new site needs, and users and passwords do not carry over.",
			},
			{
				Label:  "Your domain and DNS",
				Detail: "Your domain keeps pointing at WordPress until you change the DNS record yourself.",
			},
		},
	}
	if r != nil {
		if r.Found.Comments > 0 {
			s.DoesNotTransfer[1].Label = "Comments (" + countPhrase(r.Found.Comments, "comment") + ")"
		}
		if len(r.CustomTypes) > 0 {
			s.DoesNotTransfer = append(s.DoesNotTransfer, SummaryItem{
				Label: "Other content types: " + strings.Join(limitList(r.CustomTypes, 6), ", "),
				Detail: "Only posts and pages are converted. Content from plugins that add their own types " +
					"(products, portfolio items, events) needs to be rebuilt by hand.",
			})
		}
		s.Notes = append(s.Notes, r.Notes...)
	}
	return s
}
