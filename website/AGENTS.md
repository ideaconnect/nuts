# Project Guidelines

## Overview

Marketing / documentation website for NUTS — a Caddy Server module that bridges
NATS.io JetStream messages to Server-Sent Events (SSE). This directory lives
inside the NUTS repo (it used to be the separate `nuts-website` repo); the module
sources are its siblings one level up. Repo-wide agent rules are in the
[root AGENTS.md](../AGENTS.md).

## Tech Stack

- **Static site generator:** Jekyll
- **CSS framework:** Tailwind CSS (via PostCSS)
- **Language:** English
- **Hosting:** GitHub Pages, served at `https://idct.tech/nuts`. The org's
  `idct.tech` Pages custom domain makes this project repo resolve at the `/nuts`
  subpath, so `_config.yml` sets `baseurl: /nuts`. Deployed on every `v*` tag by
  [.github/workflows/website.yml](../.github/workflows/website.yml).

## Build and Test

Preferred: the containerised targets from the repo root (Docker only, no local
Ruby/Node):

```bash
make website-serve     # livereload dev server at http://localhost:4000/nuts/
make website-build     # production build → website/_site (mirrors CI)
make website-clean     # remove generated output
```

Native equivalents from this directory (require local Ruby + Node):

```bash
make install-deps      # bundle install + npm install
make serve             # jekyll serve --livereload
make build             # production build → _site/
```

Output goes to `_site/`. Never commit `_site/` or `assets/css/dist/`.

## Architecture

```
_layouts/        # Page templates (default, page, docs)
_includes/       # Reusable HTML partials (header, footer, hero)
_data/            # Structured data (YAML/JSON)
_posts/           # Blog posts (if any)
assets/
  css/            # Tailwind entry point, processed via PostCSS
  js/             # Minimal JS (SSE demos, mobile nav)
  images/         # Optimized images
_config.yml       # Jekyll configuration
tailwind.config.js
postcss.config.js
```

## Code Style

- **HTML/Liquid:** Use Tailwind utility classes directly; avoid custom CSS unless Tailwind can't express it
- **Layouts:** Keep DRY — shared chrome in `_layouts/default.html`, section-specific in child layouts
- **JavaScript:** Vanilla JS only — no frameworks. Keep scripts minimal and inline where possible
- **Content:** Markdown files with YAML front matter. Reference NUTS repo docs rather than duplicating technical details

## Conventions

- All code examples showing NUTS usage must match the API from the [NUTS README](https://github.com/ideaconnect/nuts)
- Use `EventSource` (not `fetch`) in all SSE client examples
- Keep the Tailwind config minimal — extend the default theme only when the design requires it
- Images must have `alt` attributes and use responsive sizing (`srcset` or Tailwind responsive classes)
- Links to NUTS repo use `https://github.com/ideaconnect/nuts` — never hardcode commit SHAs in links

## Key NUTS Concepts (for content accuracy)

- NUTS is a **Caddy module**, not a standalone server
- It bridges **NATS JetStream** → **SSE** (Server-Sent Events)
- Supports message replay via `last-id` query param or `Last-Event-ID` header
- Inspired by [Mercure.rocks](https://mercure.rocks/) but uses NATS as the backend
- Installed via `xcaddy build --with github.com/ideaconnect/nuts`
