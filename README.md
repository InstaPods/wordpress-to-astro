# wordpress-to-astro

Turn a public WordPress site into an Astro 5 project with one command: posts and pages as Markdown, your images, and every post at the URL it has today.

```sh
npx wordpress-to-astro https://your-site.com
```

No plugin to install, no login, no database export. It reads the same public REST API any visitor can, and writes a plain Astro project you can run, edit and host anywhere.

[![Deploy on InstaPods](https://instapods.com/deploy-button.svg)](https://app.instapods.com/dashboard/import?tab=wordpress&utm_source=github_wp2astro)

## What it looks like

```text
$ npx wordpress-to-astro https://wptavern.com --max-posts 20
Reading https://wptavern.com ...

Converted WP Tavern (https://wptavern.com)
  20 posts, 14 pages, 6 categories, 16 tags
  21 images downloaded
  34 URLs kept as they are

Notes:
  - Your site has 6637 posts and 14 pages; this conversion includes the newest 20 posts and 14 pages. Raise the post limit (up to 100000) or add the rest by hand.
  - 5 blocks did not convert cleanly to Markdown (tables, embeds, galleries, page-builder layouts) and were kept as HTML inside the Markdown files, so nothing is lost. They may look plainer without your theme's styles.
  - 3 items contain a form that posted back to WordPress. The form is still in the page but will not submit; point it at a form service.

Wrote 76 files (5.9 MB) to wp-tavern-astro

Next:
  cd wp-tavern-astro
  npm install
  npm run dev
```

A converted post, `src/content/posts/gutenberg-19-9-introduces-style-book-to-classic-themes.md`:

```markdown
---
title: "Gutenberg 19.9 Introduces Style Book to Classic Themes"
date: "2024-12-18T18:52:25Z"
updated: "2024-12-18T18:52:27Z"
slug: "gutenberg-19-9-introduces-style-book-to-classic-themes"
permalink: "/2024/12/18/gutenberg-19-9-introduces-style-book-to-classic-themes/"
categories: ["News"]
tags: ["gutenberg"]
excerpt: "The latest release of Gutenberg, version 19.9, brings a suite of powerful features, tools, and"
wordpressId: 182753
---

The latest release of [Gutenberg](https://make.wordpress.org/core/2024/12/18/whats-new-in-gutenberg-19-9-19-december/), version 19.9, brings a suite of powerful features, tools, and improvements for WordPress users. ...

### Style Book Comes to Classic Themes
```

## Install

`npx` needs only Node.js (16 or newer). The npm package ships a prebuilt binary for macOS, Linux and Windows (x64 and arm64).

```sh
npx wordpress-to-astro https://your-site.com          # run once
npm install -g wordpress-to-astro                     # or keep it around
go install github.com/instapods/wordpress-to-astro@latest   # or build it with Go 1.24+
```

Binaries for every platform are also on the [releases page](https://github.com/InstaPods/wordpress-to-astro/releases). On macOS, a binary downloaded with a browser is quarantined; clear it with `xattr -d com.apple.quarantine wordpress-to-astro` (npx and `go install` don't need this).

## Usage

```text
wordpress-to-astro <site-url> [flags]

  -o, --out <dir>       Where to write the project (default: a folder named after the site)
  -n, --max-posts <N>   Convert only the newest N posts (default: all)
      --dry-run         Read a few posts and show what would convert; write nothing
      --force           Write into --out even if it is not empty
      --timeout <dur>   Give up after this long (default 30m)
      --json            Print the conversion report as JSON on stdout
  -q, --quiet           No progress output
  -v, --version         Print the version
```

Give it the address of the blog itself. For WordPress installed in a sub-directory (`example.com/blog/`) or a multisite sub-site, pass that full address: the converter reads the REST API the page advertises, not the one at the domain root.

Then, with Node.js 18.20 or newer (what Astro 5 needs):

```sh
cd your-site-astro
npm install
npm run dev      # http://localhost:4321
npm run build    # static site in dist/
```

## What converts

| From WordPress | In the Astro project |
| --- | --- |
| Published posts and pages | Markdown files in `src/content/posts/` and `src/content/pages/`, with title, date, last update, slug, categories, tags, excerpt and featured image in the frontmatter |
| Images on your own domain | Downloaded to `public/` at the same `/wp-content/uploads/...` path, and re-linked. Jetpack CDN (`i0.wp.com`) images are mapped back to your site |
| Permalinks | Every post and page is built at the exact path it has today |
| Category and tag archives | Rebuilt at their current paths, listing their posts |
| RSS and sitemap | `/rss.xml` and `/sitemap-index.xml`, generated on every build. The old `/feed/` and sitemap addresses redirect to them |
| Site name, tagline, top-level pages | `src/data/site.json`, used for the header and menu |

Blocks that don't map cleanly to Markdown (tables, embeds, galleries, page-builder layouts) are kept as HTML inside the Markdown file. Nothing is silently dropped, and `MIGRATION-NOTES.md` in every project lists what came across and what did not, with counts for your site.

## What doesn't, and why

| Not converted | Why |
| --- | --- |
| Comments | They are personal data in your database, and a static site has nowhere to post new ones. Export them from WordPress (Tools, then Export) and add a comment service if you want them. The converter tells you how many you have. |
| Forms, search, shops, logins | They need PHP and a database at request time. The form markup is kept but will not submit; point it at a form service. |
| Shortcodes | The REST API returns content as WordPress renders it. Shortcodes from plugins that are no longer active come through as plain text like `[gallery]`, and the converter lists every one it finds. |
| Page builders (Elementor, Divi, WPBakery) | Their layout lives in theme CSS and JavaScript the API does not expose. The content comes across as HTML and looks plainer until you restyle it. |
| Theme, menus, widgets | Not in the public API. The project ships a plain, readable layout you can restyle in `src/layouts/Base.astro`. |
| Drafts, private and password-protected posts | Only public content is read. Protected posts get a placeholder file so you know they exist. |
| Custom post types (products, portfolio items, events) | Only posts and pages are converted. The converter names each public custom type it finds and how many items it has. |

## Permalinks and redirects

The rule: a post is built at exactly the URL it has on WordPress, so links and search rankings carry over. Only when that is impossible does a URL change, and then the old one gets a 301:

- **Plain permalinks** (`/?p=123`, `/?page_id=5`): a static site can't route on a query string, so each post gets a clean path from its slug and the old link redirects.
- **Two items on one path**: the second gets a `-<id>` suffix.
- **Feeds and sitemaps**: `/feed/`, `/wp-sitemap.xml` and friends redirect to the new `/rss.xml` and `/sitemap-index.xml`.

Redirects are written twice: `redirects.json`, applied by the included dependency-free `serve.mjs` (`npm start`), and `public/_redirects`, which Netlify and Cloudflare Pages read. On any other host, copy the entries into its redirect settings.

## The project it writes

```text
your-site-astro/
  README.md               how to run, edit and deploy it
  MIGRATION-NOTES.md      what came across, what did not, notes for your site
  astro.config.mjs        site is set to your WordPress home URL
  redirects.json          old URL -> new URL
  serve.mjs               serves dist/ and applies redirects.json
  public/
    _redirects            the same redirects for Netlify / Cloudflare Pages
    wp-content/uploads/   your images, at their original paths
  src/
    content/posts/*.md
    content/pages/*.md
    data/site.json        name, tagline, menu
    data/terms.json       categories and tags
    layouts/Base.astro    the look
    pages/[...path].astro rebuilds every original URL
    pages/rss.xml.js
```

Dependencies are `astro` 5, `@astrojs/rss` and `@astrojs/sitemap`. Nothing else.

## Other ways to move WordPress to Astro

Pick the tool that fits how you are moving:

- **You have the WordPress export file, or want drafts too:** [wordpress-export-to-markdown](https://github.com/lonekorean/wordpress-export-to-markdown) converts the XML export (posts, pages, drafts and custom post types) into Markdown with frontmatter and downloads the images. It writes Markdown for any static site generator, so routes and redirects are yours to set up.
- **You are keeping WordPress as your CMS:** [DeWP](https://delucis.github.io/dewp/) loads WordPress content into Astro content collections from the REST API, so editors keep using wp-admin.
- **You are leaving WordPress and want a working site today:** this tool. It reads the live site with nothing to export or install on the WordPress side, and writes a complete Astro project with every post at its current URL, a redirects map for the few that have to move, and HTML kept as-is where a block does not convert. It is a plain converter with no AI model in the loop, so there are no tokens to pay for and you can rerun it and diff the output.

## Hosted version

Prefer not to use a terminal? The same converter runs at [instapods.com/tools/wordpress-to-astro](https://instapods.com/tools/wordpress-to-astro/): paste your URL to see a preview of a converted post, then download the project as a zip.

## As a Go library

```go
import "github.com/instapods/wordpress-to-astro/wp2astro"

c := &wp2astro.Converter{} // or set Client, Limits, Progress
p, err := c.Convert(ctx, "https://example.com", wp2astro.Options{MaxPosts: 100})
// p.Files: the project; p.Report: what was found and converted
```

`Converter.Preview` reads a handful of requests and converts one sample post without downloading anything.

The default limits suit a run on your own machine (every post, 2 GB of images, 30 minutes). If you run the converter on a server for other people, set every field of `Limits` and `PreviewLimits`, and pass a `Client` that refuses private and loopback addresses: the library fetches whatever URL it is given.

## Contributing

Bug reports are most useful with the site URL and its WordPress version: see [CONTRIBUTING.md](CONTRIBUTING.md). Conversions that come out wrong are the main thing we want to hear about.

## License

[MIT](LICENSE). Made by [InstaPods](https://instapods.com).
