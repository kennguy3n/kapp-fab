# Documentation Site

The `docs/` tree is published as a static documentation site using
**MkDocs Material**. This document explains the build, deployment,
and versioning strategy. The configuration lives at the repository
root: `mkdocs.yml`.

---

## 18.1 Local Build

```bash
# Install (once):
python -m pip install --user \
  mkdocs==1.6.* \
  mkdocs-material==9.5.* \
  mkdocs-mermaid2-plugin==1.1.* \
  mkdocs-redirects==1.2.* \
  mike==2.1.*

# Build:
mkdocs build --strict
# Output: site/  (gitignored)

# Live preview:
mkdocs serve -a 127.0.0.1:8001
```

The `--strict` flag treats broken internal links and other warnings
as errors, matching the CI step.

---

## 18.2 Configuration (`mkdocs.yml`)

See the file at the repo root. Key choices:

- **Theme**: `material` — accessible, dark-mode, code-copy buttons.
- **Markdown extensions** (PyMdown Extensions for tabs, admonitions,
  superfences with mermaid).
- **Search**: built-in (no external indexing service needed).
- **Navigation**: hand-curated to map to the operator's task flow,
  not file alphabetical order.
- **Mermaid**: enabled via `mkdocs-mermaid2-plugin` for diagrams
  embedded in markdown.

---

## 18.3 CI Build & Deploy

```yaml
# .github/workflows/docs.yml (planned)
name: docs

on:
  push:
    branches: [main]
    paths:
      - 'docs/**'
      - 'mkdocs.yml'
      - '.github/workflows/docs.yml'
  pull_request:
    paths:
      - 'docs/**'
      - 'mkdocs.yml'

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-python@v5
        with: { python-version: "3.12" }
      - run: |
          pip install mkdocs==1.6.* mkdocs-material==9.5.* \
                       mkdocs-mermaid2-plugin==1.1.* \
                       mkdocs-redirects==1.2.*
      - run: mkdocs build --strict
      - if: github.ref == 'refs/heads/main'
        uses: peaceiris/actions-gh-pages@v4
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
          publish_dir:  ./site
          cname:        docs.kapp.example.com   # optional custom domain
```

Pull requests run `mkdocs build --strict` as a check; failures block
merge. Pushes to `main` deploy to GitHub Pages
(`https://<org>.github.io/<repo>/`).

---

## 18.4 Versioned Docs (`mike`)

Multi-version publishing is handled by `mike`, which writes versioned
trees into the `gh-pages` branch and maintains a `versions.json`
index.

```bash
# First-time setup
mike deploy --push --update-aliases 0.1.0 latest
mike set-default --push latest

# On every release:
mike deploy --push --update-aliases <new-version> latest

# Manage versions:
mike list                  # list deployed versions
mike delete --push <ver>    # remove a version
```

The MkDocs Material `version` selector reads `versions.json`
automatically. The default theme config below opts in to the
selector and the `dev/main` floating tag.

---

## 18.5 Conventions

- **One H1 per file**, matching the filename slug.
- **Internal links**: always relative (`./FILE.md`, `../FILE.md`)
  — never absolute URLs to the published site.
- **Headings**: prefix with section numbers
  (`## 3.1 Severity Definitions`) so cross-references survive renames.
- **Code blocks**: include language hints
  (` ```bash `, ` ```sql `, ` ```yaml `) so syntax highlighting works.
- **Tables**: pipe-style, aligned columns. Don't auto-format —
  `mkdocs build --strict` will not complain about column width.
- **Mermaid diagrams**:
  ````
  ```mermaid
  graph TD
    A --> B
  ```
  ````
- **Front matter**: not used (MkDocs Material reads metadata from
  the page itself).

---

## 18.6 Search and Navigation

The default MkDocs Material search is sufficient for ~50 documents.
For broader scale (200+ documents, multi-language, faceted search),
swap in `mkdocs-material-search-suggest` and a backing
`lunr.js`-with-stemming worker. Not needed at v0.1.0 scale.

Top-level navigation prioritizes:

1. Getting started (developer → operator → admin paths).
2. Architecture (concepts before procedures).
3. Operations (runbooks, on-call, incident response).
4. Reference (API, KType, plugins, ADRs).
5. Compliance & security (auditor-facing).
