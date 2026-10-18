#!/usr/bin/env bash
# Read the latest release tag and write it into docs/site/hugo.yaml's
# params.version so the docs show the current release. Run by the Cloudflare
# build before Hugo; safe to run locally, since it only edits the working tree.
#
# notify tags look like "version-0.3.3"; the "version-" prefix is stripped.
# With no reachable tag it leaves hugo.yaml alone.
set -euo pipefail

HUGO_YAML="docs/site/hugo.yaml"

# Cloudflare Workers Builds clones shallow and without tags; fetch just the
# tag refs so 'git describe' has something to look at.
git fetch --tags --depth=1 >/dev/null 2>&1 || true

# Highest version-* tag by version order. This does not need the tag to be an
# ancestor of HEAD, which release tags on a shallow or detached build may not be.
TAG=$(git tag --list 'version-*' --sort=-v:refname | head -n 1)
if [ -n "${TAG}" ]; then
    VERSION="${TAG#version-}"
else
    echo "sync-version: no version-* tags reachable, leaving hugo.yaml untouched" >&2
    exit 0
fi

# awk + temp file keeps this portable across GNU sed (Linux/CF) and BSD sed
# (macOS).
awk -v v="${VERSION}" '
    /^  version: "/ { print "  version: \"" v "\""; next }
    { print }
' "${HUGO_YAML}" > "${HUGO_YAML}.tmp"
mv "${HUGO_YAML}.tmp" "${HUGO_YAML}"

echo "sync-version: hugo.yaml params.version set to ${VERSION}"
