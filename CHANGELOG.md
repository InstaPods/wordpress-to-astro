# Changelog

## 0.1.1

- An image CDN copy that is rate-limited (Jetpack's `i0.wp.com` answers HTTP 429 to a burst) falls back to the original file straight away instead of waiting out the limit, which used up the retry budget on image-heavy sites.
- Rate-limit messages are printed at most every 15 seconds.
- `--version` reports the module version for `go install` builds.

## 0.1.0

First public release.

- `npx wordpress-to-astro <url>` converts a public WordPress site into an Astro 5 project: Markdown collections, images, permalinks kept, redirects, RSS, sitemap, MIGRATION-NOTES.md.
- Sub-directory and multisite installs read the REST API the page advertises.
- `--dry-run` previews a conversion without writing anything.
- Jetpack CDN images are downloaded from the CDN, or from the site itself when the CDN rate-limits. Image downloads retry HTTP 429 like the REST calls.
- Safe file names on Windows (reserved names like CON, trailing dots), and Ctrl-C writes nothing instead of a partial project.
- Go library `github.com/instapods/wordpress-to-astro/wp2astro`.
