# syntax=docker/dockerfile:1
#
# Local build/dev image for the NUTS website (Jekyll + Tailwind via PostCSS).
# Driven by the repo-root Makefile targets `website-build` and `website-serve`;
# it is not a deployment artefact (GitHub Pages builds the site itself — see
# .github/workflows/website.yml). Keeping the toolchain in a container means
# contributors need only Docker, not local Ruby + Node.

# --- base: Ruby + Node with all site dependencies installed -------------------
FROM ruby:3.3-slim AS base
# Install gems into an image path that a host bind-mount over /site cannot hide,
# so `website-serve` can mount live source without shadowing the toolchain.
ENV BUNDLE_PATH=/usr/local/bundle
WORKDIR /site
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential curl ca-certificates \
 && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*
# Dependency manifests first for layer caching.
COPY Gemfile Gemfile.lock ./
RUN bundle install
COPY package.json package-lock.json ./
RUN npm ci

# --- dev: livereload server; source is bind-mounted at run time ---------------
FROM base AS dev
EXPOSE 4000 35729
# Rebuild CSS once, then watch Tailwind and serve Jekyll together. Polling keeps
# file-change detection reliable across bind mounts (WSL2, macOS, Docker Desktop).
CMD ["sh", "-c", "npm run css:build && (npm run css:watch & bundle exec jekyll serve --host 0.0.0.0 --livereload --force_polling)"]

# --- build: bake the current source into a production _site -------------------
FROM base AS build
ENV JEKYLL_ENV=production
COPY . .
RUN npm run css:build \
 && bundle exec jekyll build

# --- artifact: export-only stage for `docker build --output` ------------------
# `make website-build` targets this stage so the generated site lands on the
# host at website/_site with nothing else copied out.
FROM scratch AS artifact
COPY --from=build /site/_site /
