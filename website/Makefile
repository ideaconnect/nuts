.PHONY: install-deps build serve

# Native (non-Docker) toolchain setup. Prefer the repo-root `make website-*`
# targets, which run this in a container so no local Ruby/Node is required.
install-deps:
	bundle install
	npm install

# Production build → _site/. JEKYLL_ENV=production is set on both steps so the
# Tailwind/PostCSS pipeline minifies (cssnano) and Jekyll builds for production,
# regardless of the caller's environment. Used by CI and the Docker build stage.
build:
	JEKYLL_ENV=production npm run css:build
	JEKYLL_ENV=production bundle exec jekyll build

# Local livereload dev server (native). The Docker equivalent is
# `make website-serve` from the repo root.
serve:
	npm run css:build
	bundle exec jekyll serve --livereload
