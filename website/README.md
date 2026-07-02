# fdb.dev website

The source for [fdb.dev](https://fdb.dev) — landing page, docs, changelog, performance
dashboard, and the Go vanity-import stubs. Built with [Hugo](https://gohugo.io) + the
[hextra](https://github.com/imfing/hextra) theme (vendored under `themes/hextra`), deployed
to GitHub Pages.

## Develop

```sh
cd website
hugo server          # http://localhost:1313
hugo --gc --minify   # production build into ./public
```

No npm step is required — hextra ships precompiled CSS (`themes/hextra/assets/css/compiled/main.css`).

## Structure

```
content/
  _index.md          landing page (layout: hextra-home)
  docs/              user guide (getting started, record layer, SQL, maturity)
  changelog.md       rendered from the repo CHANGELOG.md (see below)
static/install.sh    the frl CLI installer (curl -fsSL https://fdb.dev/install.sh | sh)
data/                site data (package list for vanity imports)
themes/hextra/       vendored theme (no go.mod; not part of the Go/Bazel build — see .bazelignore)
```

## Changelog (single source of truth)

The repo's top-level `CHANGELOG.md` is the source. The deploy workflow copies it into
`content/changelog.md` (prepending front matter) at build time, so it is never edited here
by hand.

## Vanity imports

The Go module path is `fdb.dev`. GitHub Pages is static, so we can't run a `?go-get=1`
handler — instead we generate a static stub page per package path carrying the
`<meta name="go-import">` / `<meta name="go-source">` tags (HTTP 200), driven by
`data/packages.yaml`.

## Deploy

GitHub Actions builds `website/` and publishes to Pages on push to `master`; `CNAME`
points the site at `fdb.dev`. See `.github/workflows/` and the DNS notes in the deploy PR.
