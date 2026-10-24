# Contributing

Thanks for helping. The most useful thing you can send is a site that converts badly.

## Reporting a conversion problem

Open an issue with the **"A conversion came out wrong"** template. Include:

- the site URL you passed (a sub-directory install like `example.com/blog/` matters),
- the WordPress version,
- the exact command and the terminal output,
- which post or page came out wrong, and a link to the original.

The converter only reads the public REST API, so we can usually reproduce a problem from the URL alone.

## Working on the code

You need Go 1.24 or newer.

```sh
go test ./...                 # all tests, no network
go run . https://example.com  # run the CLI from source
go run . https://example.com --dry-run
```

Layout:

- `wp2astro/` is the library. `convert.go` reads the REST API and decides paths, `markdown.go` turns WordPress HTML into Markdown, `project.go` writes the Astro project.
- `main.go` is the CLI.
- `npm/` is the npm wrapper: a small Node script that runs the prebuilt binary for your platform.

Tests run against a recording of a real site in `wp2astro/testdata/wp-rest/`, served from a local server. When you fix a conversion bug, add a test that fails without the fix: either a case in `TestHTMLToMarkdown` for markup, or a fixture variation in `convert_test.go` for discovery and paths.

## Ground rules

- The converter reads only what is public. No login, no admin API, no scraping of `wp-admin`.
- Never drop content silently. If something can't convert cleanly, keep it as HTML and say so in the report.
- Keep the generated project plain: Astro plus `@astrojs/rss` and `@astrojs/sitemap`, no extra dependencies, nothing tied to one host.
- User-facing text (notes, README, errors) is short and plain. No em dashes.

## Pull requests

Keep a PR to one change, run `gofmt` and `go test ./...`, and describe what a user will see differently. CI runs the same checks.

## Releases

Maintainers tag `vX.Y.Z`. The release workflow builds the binaries, attaches them to a GitHub release and publishes the npm packages.
