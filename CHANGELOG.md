# Changelog

## 0.1.0

First public release.

- `npx wordpress-to-astro <url>` converts a public WordPress site into an Astro 5 project: Markdown collections, images, permalinks kept, redirects, RSS, sitemap, MIGRATION-NOTES.md.
- Sub-directory and multisite installs read the REST API the page advertises.
- `--dry-run` previews a conversion without writing anything.
- Jetpack CDN images are downloaded from the CDN, or from the site itself when the CDN rate-limits. Image downloads retry HTTP 429 like the REST calls.
- Safe file names on Windows (reserved names like CON, trailing dots), and Ctrl-C writes nothing instead of a partial project.
- Go library `github.com/instapods/wordpress-to-astro/wp2astro`.
