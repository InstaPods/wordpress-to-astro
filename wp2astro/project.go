package wp2astro

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// The Astro project the WordPress converter writes.
//
// Kept deliberately small: one layout, one catch-all route that rebuilds every
// original URL, an RSS feed, a sitemap, and a dependency-free server
// (serve.mjs) that applies the redirect map. Everything a reader might want to
// change is plain data (src/data/*.json, src/content/**.md), so the project is
// easy to edit without knowing Astro.

// Pinned to Astro 5: it runs on any current Node LTS (Node 22
// included), where newer majors require a recent 22.x minor.
const (
	wpAstroVersion   = "^5.18.2"
	wpRSSVersion     = "^4.0.12"
	wpSitemapVersion = "^3.6.0"
)

func (c *wpConversion) projectFiles() []File {
	var files []File
	add := func(p, content string) {
		files = append(files, File{Path: p, Content: content, Size: len(content)})
	}
	r := c.report

	add("README.md", c.readme())
	add("MIGRATION-NOTES.md", c.migrationNotes())
	add("package.json", c.packageJSON())
	add("astro.config.mjs", fmt.Sprintf(wpAstroConfig, jsonString(r.Site.Home)))
	add("tsconfig.json", wpTSConfig)
	add(".gitignore", "node_modules/\ndist/\n.astro/\n")
	add("serve.mjs", wpServeMJS)
	add("redirects.json", jsonIndent(c.redirects))
	add("public/_redirects", c.netlifyRedirects())
	// The sitemap is written at the root of the built site, also for a
	// WordPress that lived in a sub-directory.
	add("public/robots.txt", "User-agent: *\nAllow: /\n\nSitemap: "+c.home.Scheme+"://"+c.home.Host+"/sitemap-index.xml\n")
	add("src/content.config.ts", wpContentConfig)
	add("src/data/site.json", c.siteJSON())
	add("src/data/terms.json", jsonIndent(c.terms))
	add("src/lib/content.ts", wpLibContent)
	add("src/layouts/Base.astro", wpBaseLayout)
	add("src/components/PostList.astro", wpPostList)
	add("src/components/Entry.astro", wpEntryComponent)
	add("src/pages/index.astro", wpIndexPage)
	add("src/pages/[...path].astro", wpCatchAllPage)
	add("src/pages/rss.xml.js", wpRSS)
	add("src/pages/404.astro", wp404)

	hasPosts, hasPages := false, false
	for _, e := range c.entries {
		add(e.file, c.frontmatter(e, c.images)+"\n"+e.md.Markdown)
		if e.kind == "post" {
			hasPosts = true
		} else {
			hasPages = true
		}
	}
	// The glob loader warns on a missing folder; keep both present.
	if !hasPosts {
		add("src/content/posts/.gitkeep", "")
	}
	if !hasPages {
		add("src/content/pages/.gitkeep", "")
	}
	return append(files, c.files...)
}

func (c *wpConversion) frontmatter(e *wpEntry, images map[string]string) string {
	var b strings.Builder
	b.WriteString("---\n")
	line := func(k string, v any) {
		b.WriteString(k + ": " + jsonValue(v) + "\n")
	}
	line("title", e.title)
	if e.date != "" {
		line("date", e.date)
	}
	if e.updated != "" && e.updated != e.date {
		line("updated", e.updated)
	}
	slug := e.item.Slug
	if s, err := url.PathUnescape(slug); err == nil {
		slug = s
	}
	line("slug", slug)
	line("permalink", e.permalink)
	line("categories", nonNil(e.categories))
	line("tags", nonNil(e.tags))
	if e.excerpt != "" {
		line("excerpt", e.excerpt)
	}
	if e.featured != "" {
		img := e.featured
		if u, err := url.Parse(e.featured); err == nil {
			if local := images[stripImageURL(u)]; local != "" {
				img = local
			}
		}
		line("featuredImage", img)
		if e.featAlt != "" {
			line("featuredImageAlt", e.featAlt)
		}
	}
	line("wordpressId", e.item.ID)
	if e.postsIndex {
		line("postsIndex", true)
	}
	b.WriteString("---\n")
	return b.String()
}

func (c *wpConversion) siteJSON() string {
	type navItem struct {
		Title string `json:"title"`
		Href  string `json:"href"`
	}
	var pages []*wpEntry
	for _, e := range c.entries {
		if e.kind == "page" && e.item.Parent == 0 && e.permalink != "/" {
			pages = append(pages, e)
		}
	}
	sort.SliceStable(pages, func(i, j int) bool { return pages[i].item.MenuOrder < pages[j].item.MenuOrder })
	nav := []navItem{}
	for _, e := range pages {
		if len(nav) == 7 {
			break
		}
		nav = append(nav, navItem{Title: e.title, Href: escapePath(e.permalink)})
	}
	return jsonIndent(map[string]any{
		"name":        c.report.Site.Name,
		"description": c.report.Site.Description,
		"home":        c.report.Site.Home,
		"language":    "en",
		"nav":         nav,
	})
}

func (c *wpConversion) packageJSON() string {
	return jsonIndent(map[string]any{
		"name":    defaultSlug(Slugify(c.report.Site.Name), "wordpress-astro"),
		"type":    "module",
		"version": "0.1.0",
		"private": true,
		"scripts": map[string]string{
			"dev":     "astro dev",
			"build":   "astro build",
			"preview": "astro preview",
			"start":   "node serve.mjs",
		},
		"dependencies": map[string]string{
			"astro":            wpAstroVersion,
			"@astrojs/rss":     wpRSSVersion,
			"@astrojs/sitemap": wpSitemapVersion,
		},
	})
}

// netlifyRedirects writes the same map in the _redirects format Netlify and
// Cloudflare Pages read, so the project keeps its redirects on those hosts too.
func (c *wpConversion) netlifyRedirects() string {
	keys := make([]string, 0, len(c.redirects))
	for k := range c.redirects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Old WordPress URLs and where they live now. Read by Netlify and Cloudflare Pages.\n")
	b.WriteString("# serve.mjs applies the same list from redirects.json.\n")
	for _, k := range keys {
		if strings.Contains(k, "?") {
			// _redirects matches query strings with a separate syntax:
			//   /  p=12  /hello-world/  301
			pathPart, q, _ := strings.Cut(k, "?")
			if name, val, ok := strings.Cut(q, "="); ok && !strings.Contains(q, "&") {
				fmt.Fprintf(&b, "%s %s=%s %s 301\n", pathPart, name, val, c.redirects[k])
			}
			continue
		}
		fmt.Fprintf(&b, "%s %s 301\n", k, c.redirects[k])
	}
	return b.String()
}

func (c *wpConversion) readme() string {
	r := c.report
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", r.Site.Name)
	gen := c.conv.Generator
	if gen == "" {
		gen = DefaultGenerator
	}
	fmt.Fprintf(&b, "This is an [Astro](https://astro.build) site converted from the WordPress site at %s by %s.\n\n", r.Site.Home, gen)
	fmt.Fprintf(&b, "It contains %s, %s and %s. Every post and page is built at the same URL it has on WordPress, so links and search rankings carry over. Read MIGRATION-NOTES.md for what did not come across.\n\n",
		countPhrase(r.Converted.Posts, "post"), countPhrase(r.Converted.Pages, "page"), countPhrase(r.Converted.ImagesDownloaded, "image"))
	b.WriteString(`## Run it on your computer

You need Node.js 18.20 or newer.

` + "```" + `sh
npm install
npm run dev
` + "```" + `

Open http://localhost:4321. Pages reload as you edit.

## Edit content

- Posts are Markdown files in ` + "`src/content/posts/`" + `, pages in ` + "`src/content/pages/`" + `. The block between the ` + "`---`" + ` lines at the top (title, date, categories, tags, excerpt, featured image) is the frontmatter.
- ` + "`permalink`" + ` in the frontmatter is the URL the post is built at. Change it and add the old one to ` + "`redirects.json`" + ` if you want to keep incoming links.
- To add a post, copy an existing file, change the frontmatter and write.
- Some blocks are HTML instead of Markdown (tables, embeds, galleries). That is on purpose: they did not convert cleanly, and HTML works fine inside Markdown.
- Images live in ` + "`public/`" + ` at the same path they had on WordPress (` + "`/wp-content/uploads/...`" + `).
- The site name, tagline and menu are in ` + "`src/data/site.json`" + `. The look is in ` + "`src/layouts/Base.astro`" + `.

## Build

` + "```" + `sh
npm run build
` + "```" + `

The finished site is in ` + "`dist/`" + `: plain HTML, CSS and images. It also writes ` + "`/rss.xml`" + ` and ` + "`/sitemap-index.xml`" + `.

## Redirects

` + "`redirects.json`" + ` lists old URLs that moved (plain ` + "`?p=123`" + ` links, the old feed and sitemap addresses). Two copies are kept in sync:

- ` + "`serve.mjs`" + ` reads ` + "`redirects.json`" + ` and answers with a 301.
- ` + "`public/_redirects`" + ` is the same list in the format Netlify and Cloudflare Pages read.

On any other host, add the entries from ` + "`redirects.json`" + ` to that host's redirect settings.

## Deploy

` + c.deploySection() + `**Your domain**: ` + "`site`" + ` in ` + "`astro.config.mjs`" + ` is set to ` + "`" + r.Site.Home + "`" + ` so the sitemap, RSS feed and canonical links use your real address. Change it if the site moves to a new domain, then point your DNS at the new host.
`)
	return b.String()
}

func (c *wpConversion) migrationNotes() string {
	sum := c.conv.Summary
	if sum == nil {
		sum = DefaultSummary
	}
	s := sum(c.report)
	var b strings.Builder
	b.WriteString("# Migration notes\n\n")
	fmt.Fprintf(&b, "Converted from %s using its public REST API (%s).\n\n", c.report.Site.Home, c.report.Site.RESTAPI)
	b.WriteString("## What came across\n\n")
	for _, it := range s.Transfers {
		fmt.Fprintf(&b, "- **%s.** %s\n", it.Label, it.Detail)
	}
	b.WriteString("\n## What did not\n\n")
	for _, it := range s.DoesNotTransfer {
		fmt.Fprintf(&b, "- **%s.** %s\n", it.Label, it.Detail)
	}
	if len(s.Notes) > 0 {
		b.WriteString("\n## Notes from this conversion\n\n")
		for _, n := range s.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}

// DefaultGenerator credits the converter in the project README.
const DefaultGenerator = "[wordpress-to-astro](https://github.com/InstaPods/wordpress-to-astro)"

// DefaultDeploySection is the README's "## Deploy" body, minus the closing
// paragraph about the domain, which every README gets.
const DefaultDeploySection = "**With Node**: `npm start` serves `dist/` with the redirects applied, on port 3000 " +
	"(or `$PORT`). Any host that runs Node can run the site this way.\n\n" +
	"**On a static host**: it is a standard Astro project. Netlify, Cloudflare Pages, Vercel, GitHub Pages " +
	"or any static host can build it with `npm run build` and publish `dist/`. Netlify and Cloudflare Pages " +
	"also apply `public/_redirects`.\n\n"

func (c *wpConversion) deploySection() string {
	if d := strings.TrimRight(c.conv.DeploySection, "\n"); d != "" {
		return d + "\n\n"
	}
	return DefaultDeploySection
}

// ------------------------------------------------------------------ helpers

func jsonValue(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSpace(buf.String())
}

func jsonString(s string) string { return jsonValue(s) }

func jsonIndent(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return buf.String()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ------------------------------------------------------------------ templates

const wpAstroConfig = `import { defineConfig } from 'astro/config';
import sitemap from '@astrojs/sitemap';

// site is your WordPress address, so the sitemap, RSS feed and canonical links
// point at your real domain. Change it if the site moves.
export default defineConfig({
  site: %s,
  build: { format: 'directory' },
  integrations: [sitemap()],
});
`

const wpTSConfig = `{
  "extends": "astro/tsconfigs/base",
  "include": [".astro/types.d.ts", "**/*"],
  "exclude": ["dist"]
}
`

const wpContentConfig = `import { defineCollection, z } from 'astro:content';
import { glob } from 'astro/loaders';

// One schema for posts and pages. These are the frontmatter fields the
// converter writes; add your own as you need them.
const entry = z.object({
  title: z.string(),
  date: z.coerce.date(),
  updated: z.coerce.date().optional(),
  slug: z.string(),
  // The URL this entry is built at. It matches the WordPress URL.
  permalink: z.string(),
  categories: z.array(z.string()).default([]),
  tags: z.array(z.string()).default([]),
  excerpt: z.string().optional(),
  featuredImage: z.string().optional(),
  featuredImageAlt: z.string().optional(),
  wordpressId: z.number().optional(),
  // Set on the page WordPress used as the blog index: it lists the posts.
  postsIndex: z.boolean().optional(),
});

// Entries are keyed by file name, so two pages may share a WordPress slug.
const folder = (name: string) =>
  glob({
    pattern: '**/*.md',
    base: './src/content/' + name,
    generateId: ({ entry }) => entry.replace(/\.md$/, ''),
  });

export const collections = {
  posts: defineCollection({ loader: folder('posts'), schema: entry }),
  pages: defineCollection({ loader: folder('pages'), schema: entry }),
};
`

const wpLibContent = `import { getCollection } from 'astro:content';

/** All posts, newest first. */
export async function allPosts() {
  const posts = await getCollection('posts');
  return posts.sort((a, b) => b.data.date.valueOf() - a.data.date.valueOf());
}

/** Turns "/2024/05/hello/" into the route parameter "2024/05/hello". */
export function routeParam(permalink: string) {
  return permalink.replace(/^\/+|\/+$/g, '');
}
`

const wpBaseLayout = `---
import site from '../data/site.json';

interface Props {
  title?: string;
  description?: string;
  permalink?: string;
  image?: string;
}

const { title, description, permalink, image } = Astro.props;
const fullTitle = title ? title + ' | ' + site.name : site.name;
const canonical = permalink && Astro.site ? new URL(permalink, Astro.site).href : undefined;
const metaDescription = description || site.description;
---
<!doctype html>
<html lang={site.language}>
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>{fullTitle}</title>
    {metaDescription && <meta name="description" content={metaDescription} />}
    {canonical && <link rel="canonical" href={canonical} />}
    <meta property="og:title" content={title || site.name} />
    {metaDescription && <meta property="og:description" content={metaDescription} />}
    {image && Astro.site && <meta property="og:image" content={new URL(image, Astro.site).href} />}
    <link rel="alternate" type="application/rss+xml" title={site.name} href="/rss.xml" />
    <link rel="sitemap" href="/sitemap-index.xml" />
    <style is:global>
      :root { color-scheme: light dark; --text: #1f2328; --muted: #59636e; --bg: #ffffff; --line: #d1d9e0; --link: #0b57d0; }
      @media (prefers-color-scheme: dark) { :root { --text: #e6e6e6; --muted: #a0a8b0; --bg: #14161a; --line: #30363d; --link: #8ab4f8; } }
      * { box-sizing: border-box; }
      body { margin: 0; background: var(--bg); color: var(--text); font: 18px/1.65 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
      .wrap { max-width: 44rem; margin: 0 auto; padding: 0 1.25rem; }
      a { color: var(--link); }
      .site-header { border-bottom: 1px solid var(--line); padding: 1.5rem 0 1rem; margin-bottom: 2rem; }
      .site-title { font-size: 1.4rem; font-weight: 700; color: var(--text); text-decoration: none; }
      .tagline { margin: 0.25rem 0 0; color: var(--muted); }
      nav { display: flex; flex-wrap: wrap; gap: 0.25rem 1.25rem; margin-top: 0.75rem; }
      h1, h2, h3, h4 { line-height: 1.25; }
      img, video, iframe { max-width: 100%; height: auto; }
      figure { margin: 1.5rem 0; }
      figcaption { color: var(--muted); font-size: 0.9rem; }
      pre { overflow-x: auto; padding: 1rem; border: 1px solid var(--line); border-radius: 6px; font-size: 0.9rem; }
      code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
      table { border-collapse: collapse; width: 100%; display: block; overflow-x: auto; }
      th, td { border: 1px solid var(--line); padding: 0.4rem 0.6rem; }
      blockquote { margin: 1.5rem 0; padding-left: 1rem; border-left: 3px solid var(--line); color: var(--muted); }
      .meta { color: var(--muted); font-size: 0.9rem; }
      .post-list { list-style: none; padding: 0; }
      .post-list li { margin: 0 0 1.5rem; }
      .post-list a { font-size: 1.15rem; font-weight: 600; text-decoration: none; }
      .post-list p { margin: 0.25rem 0 0; }
      .terms a { margin-right: 0.5rem; }
      .site-footer { border-top: 1px solid var(--line); margin-top: 3rem; padding: 1.5rem 0; color: var(--muted); font-size: 0.9rem; }
    </style>
  </head>
  <body>
    <header class="site-header">
      <div class="wrap">
        <a class="site-title" href="/">{site.name}</a>
        {site.description && <p class="tagline">{site.description}</p>}
        {site.nav.length > 0 && (
          <nav>{site.nav.map((item) => <a href={item.href}>{item.title}</a>)}</nav>
        )}
      </div>
    </header>
    <main class="wrap">
      <slot />
    </main>
    <footer class="site-footer">
      <div class="wrap">
        <p>&copy; {new Date().getFullYear()} {site.name} · <a href="/rss.xml">RSS</a></p>
      </div>
    </footer>
  </body>
</html>
`

const wpPostList = `---
import type { CollectionEntry } from 'astro:content';

interface Props {
  posts: CollectionEntry<'posts'>[];
}

const { posts } = Astro.props;
const format = new Intl.DateTimeFormat('en', { dateStyle: 'long', timeZone: 'UTC' });
---
<ul class="post-list">
  {posts.map((post) => (
    <li>
      <a href={post.data.permalink}>{post.data.title}</a>
      <div class="meta"><time datetime={post.data.date.toISOString()}>{format.format(post.data.date)}</time></div>
      {post.data.excerpt && <p>{post.data.excerpt}</p>}
    </li>
  ))}
</ul>
`

const wpEntryComponent = `---
import type { CollectionEntry } from 'astro:content';
import { render } from 'astro:content';
import terms from '../data/terms.json';

interface Props {
  entry: CollectionEntry<'posts'> | CollectionEntry<'pages'>;
}

const { entry } = Astro.props;
const { Content } = await render(entry);
const isPost = entry.collection === 'posts';
const format = new Intl.DateTimeFormat('en', { dateStyle: 'long', timeZone: 'UTC' });
const linkFor = (list: { name: string; permalink: string }[], name: string) =>
  list.find((t) => t.name === name)?.permalink;
---
<article>
  <h1>{entry.data.title}</h1>
  {isPost && (
    <p class="meta">
      <time datetime={entry.data.date.toISOString()}>{format.format(entry.data.date)}</time>
    </p>
  )}
  {entry.data.featuredImage && (
    <figure>
      <img src={entry.data.featuredImage} alt={entry.data.featuredImageAlt || ''} />
    </figure>
  )}
  <Content />
  {isPost && entry.data.categories.length + entry.data.tags.length > 0 && (
    <p class="meta terms">
      {entry.data.categories.map((name) => {
        const href = linkFor(terms.categories, name);
        return href ? <a href={href}>{name}</a> : <span>{name} </span>;
      })}
      {entry.data.tags.map((name) => {
        const href = linkFor(terms.tags, name);
        return href ? <a href={href}>#{name}</a> : <span>#{name} </span>;
      })}
    </p>
  )}
</article>
`

const wpIndexPage = `---
import { getCollection } from 'astro:content';
import Base from '../layouts/Base.astro';
import Entry from '../components/Entry.astro';
import PostList from '../components/PostList.astro';
import { allPosts } from '../lib/content';

// WordPress either shows a static front page here or the latest posts.
const pages = await getCollection('pages');
const front = pages.find((page) => page.data.permalink === '/' && !page.data.postsIndex);
const posts = await allPosts();
---
<Base title={front?.data.title} description={front?.data.excerpt} permalink="/">
  {front ? <Entry entry={front} /> : <PostList posts={posts} />}
</Base>
`

const wpCatchAllPage = `---
// Rebuilds every WordPress URL: each post and page at its permalink, and each
// category and tag archive at its old path.
import { getCollection } from 'astro:content';
import Base from '../layouts/Base.astro';
import Entry from '../components/Entry.astro';
import PostList from '../components/PostList.astro';
import { allPosts, routeParam } from '../lib/content';
import terms from '../data/terms.json';

export async function getStaticPaths() {
  const posts = await allPosts();
  const pages = await getCollection('pages');
  const taken = new Set<string>();
  const routes = [];

  for (const entry of [...posts, ...pages]) {
    const path = routeParam(entry.data.permalink);
    if (!path || taken.has(path)) continue;
    taken.add(path);
    routes.push({ params: { path }, props: { kind: 'entry' as const, entry, posts } });
  }

  for (const term of [...terms.categories, ...terms.tags]) {
    const path = routeParam(term.permalink);
    if (!path || taken.has(path)) continue;
    const list = posts.filter((post) =>
      (term.taxonomy === 'category' ? post.data.categories : post.data.tags).includes(term.name),
    );
    if (list.length === 0) continue;
    taken.add(path);
    routes.push({ params: { path }, props: { kind: 'term' as const, term, posts: list } });
  }
  return routes;
}

const props = Astro.props;
const entry = props.kind === 'entry' ? props.entry : undefined;
const term = props.kind === 'term' ? props.term : undefined;
const listPosts = term || entry?.data.postsIndex;
const title = entry ? entry.data.title : (term?.taxonomy === 'tag' ? 'Tag: ' : '') + term?.name;
const permalink = entry ? entry.data.permalink : term?.permalink;
---
<Base title={title} description={entry?.data.excerpt} permalink={permalink} image={entry?.data.featuredImage}>
  {listPosts ? (
    <>
      <h1>{title}</h1>
      <PostList posts={props.posts} />
    </>
  ) : (
    entry && <Entry entry={entry} />
  )}
</Base>
`

const wpRSS = `import rss from '@astrojs/rss';
import site from '../data/site.json';
import { allPosts } from '../lib/content';

export async function GET(context) {
  const posts = (await allPosts()).slice(0, 50);
  return rss({
    title: site.name,
    description: site.description || site.name,
    site: context.site,
    items: posts.map((post) => ({
      title: post.data.title,
      pubDate: post.data.date,
      description: post.data.excerpt,
      link: post.data.permalink,
      categories: [...post.data.categories, ...post.data.tags],
    })),
  });
}
`

const wp404 = `---
import Base from '../layouts/Base.astro';
---
<Base title="Page not found">
  <h1>Page not found</h1>
  <p>That page doesn't exist. <a href="/">Go to the home page</a>.</p>
</Base>
`

// wpServeMJS serves dist/ with no dependencies and applies redirects.json. It
// is what `npm start` runs.
const wpServeMJS = `// Serves the built site in dist/ and applies the redirects in redirects.json.
// No dependencies: run "npm run build" once, then "node serve.mjs".
import { createServer } from 'node:http';
import { createReadStream, readFileSync } from 'node:fs';
import { stat } from 'node:fs/promises';
import { dirname, extname, join, normalize, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const dist = join(root, 'dist');
const port = Number(process.env.PORT) || 3000;
const host = process.env.HOST || '0.0.0.0';

let redirects = {};
try {
  redirects = JSON.parse(readFileSync(join(root, 'redirects.json'), 'utf8'));
} catch {
  redirects = {};
}

const types = {
  '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8', '.mjs': 'text/javascript; charset=utf-8',
  '.json': 'application/json', '.xml': 'application/xml; charset=utf-8',
  '.txt': 'text/plain; charset=utf-8', '.svg': 'image/svg+xml', '.png': 'image/png',
  '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.gif': 'image/gif', '.webp': 'image/webp',
  '.avif': 'image/avif', '.ico': 'image/x-icon', '.bmp': 'image/bmp', '.pdf': 'application/pdf',
  '.woff': 'font/woff', '.woff2': 'font/woff2', '.mp4': 'video/mp4', '.webm': 'video/webm',
  '.mp3': 'audio/mpeg',
};

// WordPress query-string URLs (?p=12, ?page_id=5, ?cat=3, ?feed=rss2) are
// matched one parameter at a time, so tracking parameters don't break them.
const queryKeys = ['p', 'page_id', 'cat', 'tag', 'feed', 'attachment_id'];

function findRedirect(url) {
  const path = url.pathname;
  if (url.search) {
    if (redirects[path + url.search]) return redirects[path + url.search];
    for (const key of queryKeys) {
      const value = url.searchParams.get(key);
      if (value !== null) {
        const hit = redirects[path + '?' + key + '=' + value];
        if (hit) return hit;
      }
    }
  }
  return redirects[path] || redirects[path.endsWith('/') ? path.slice(0, -1) : path + '/'];
}

async function isFile(p) {
  try {
    return (await stat(p)).isFile();
  } catch {
    return false;
  }
}

async function resolveFile(pathname) {
  const full = normalize(join(dist, pathname));
  if (full !== dist && !full.startsWith(dist + sep)) return null;
  if (await isFile(full)) return full;
  if (await isFile(join(full, 'index.html'))) return join(full, 'index.html');
  if (!extname(full) && (await isFile(full + '.html'))) return full + '.html';
  return null;
}

createServer(async (req, res) => {
  try {
    const url = new URL(req.url || '/', 'http://localhost');
    const target = findRedirect(url);
    if (target) {
      res.writeHead(301, { Location: target });
      res.end();
      return;
    }
    let pathname;
    try {
      pathname = decodeURIComponent(url.pathname);
    } catch {
      res.writeHead(400);
      res.end('Bad request');
      return;
    }
    let file = await resolveFile(pathname);
    let status = 200;
    if (!file) {
      status = 404;
      file = (await isFile(join(dist, '404.html'))) ? join(dist, '404.html') : null;
    }
    if (!file) {
      res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end('Not found');
      return;
    }
    const ext = extname(file).toLowerCase();
    res.writeHead(status, {
      'Content-Type': types[ext] || 'application/octet-stream',
      'Cache-Control': pathname.startsWith('/_astro/') ? 'public, max-age=31536000, immutable' : ext === '.html' ? 'no-cache' : 'public, max-age=3600',
    });
    if (req.method === 'HEAD') {
      res.end();
      return;
    }
    createReadStream(file).pipe(res);
  } catch (err) {
    console.error(err);
    res.writeHead(500);
    res.end('Server error');
  }
}).listen(port, host, () => {
  console.log('Serving dist/ on http://' + host + ':' + port);
});
`
