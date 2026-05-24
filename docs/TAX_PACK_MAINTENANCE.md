# Tax pack maintenance

This document is the maintenance checklist for the country-
specific tax packs in `internal/hr/taxpacks/`, the matching Chart-
of-Accounts templates in `internal/tenant/coa_templates/`, and the
i18n catalogues in `internal/i18n/locales/` and
`apps/web/src/locales/`. It is meant to be opened every time a new
country is added or every time an existing pack's tax rates need
the annual refresh.

## What lives where

| Concern                   | Backend location                              | Frontend location                       |
| ------------------------- | --------------------------------------------- | --------------------------------------- |
| Tax pack (rates / rules)  | `internal/hr/taxpacks/<cc>.go`                | —                                       |
| CoA template (JSON)       | `internal/tenant/coa_templates/<cc>_basic.json` | mirror entry in `SetupWizardPage.tsx`  |
| Country → CoA mapping     | `DefaultCoATemplateForCountry()` in `internal/tenant/wizard.go` | `COUNTRY_COA_DEFAULTS` in `apps/web/src/pages/SetupWizardPage.tsx` |
| Country → locale mapping  | `DefaultLocaleForCountry()` in `internal/tenant/wizard.go`     | `COUNTRY_LOCALE_DEFAULTS` in `apps/web/src/lib/i18n/locales.ts`    |
| Locale catalogue          | `internal/i18n/locales/<tag>.json`            | `apps/web/src/locales/<tag>.json`       |

The frontend and backend each own their own copy of every map and
every catalogue on purpose: bundle size and request shape can
diverge over time (the frontend ships dropdown copy and Intl
formatter hints that the backend has no use for; the backend ships
server-side validator messages that the frontend never renders).
The drift between halves is caught by the CI checks listed at the
end of this document, not by sharing a single source file.

## Adding a new country

The minimum set of edits to add a new tax pack:

1. **Tax pack** — create `internal/hr/taxpacks/<cc>.go` implementing
   the `TaxPack` interface (`Country() string`, `Compute(...)` and
   any country-specific helpers). Register it in the `init()` block
   via `Register(&yourPack{})`. Cross-pack helpers that apply to
   multiple jurisdictions (e.g. `isGCCNational`) live in
   `taxpacks.go`, not in any single pack.
2. **Pack tests** — add a regression matrix to one of the regional
   test files (`apac_packs_test.go`, `europe_mena_packs_test.go`)
   covering at minimum: nominal salary, threshold crossings, year-
   to-date caps where they exist (ALV for CH, SSC for SA, Article
   13 for BH), and the empty-input edge case.
3. **CoA template** — author
   `internal/tenant/coa_templates/<cc>_basic.json` following the
   IFRS-1100/2100/3100/4100/5100 hierarchy already used by the
   other packs. Run `go test ./internal/tenant/...` to verify the
   template loads and that the parent-references are valid.
4. **Country → CoA mapping** — add the case to
   `DefaultCoATemplateForCountry()` (`internal/tenant/wizard.go`)
   AND the matching entry to `COUNTRY_COA_DEFAULTS` in
   `apps/web/src/pages/SetupWizardPage.tsx`. Both halves must agree.
5. **Country → locale mapping** — if the country has a non-English
   default locale, add the case to `DefaultLocaleForCountry()`
   (`internal/tenant/wizard.go`) AND to `COUNTRY_LOCALE_DEFAULTS`
   in `apps/web/src/lib/i18n/locales.ts`. Use the *renderable*
   tag (the one the locale resolver will downgrade to). For
   countries with multiple co-official languages, document the
   choice in a comment so a future maintainer doesn't second-guess
   it.
6. **Locale catalogues** — if a new locale is being added, create
   matching `<tag>.json` files in BOTH `internal/i18n/locales/`
   and `apps/web/src/locales/` with the full baseline keyset.
   `TestFrontendBackendCatalogueParity` in `internal/i18n/`
   enforces the two halves stay in lock-step.

## Annual tax rate review

Most packs have a year-end review cadence baked into the
deliverable: in the first week of every January, walk through the
checklist below to confirm every rate constant is still current.
The `tax-rate-review.yml` GitHub Actions job posts an issue the
first of every year with this checklist pre-filled, so the work
shows up in the team's tracker without anyone having to remember.

For each pack:

- **Income-tax bands / brackets** — check the country's revenue
  service site for any rate, threshold, or band change effective
  the new fiscal year.
- **Social-security caps and rates** — confirm contribution
  ceilings, employer/employee splits, and any new wage-based caps
  (e.g. UK NIC upper earnings limit, CH ALV YTD cap, SA GOSI 9%
  base, AU SGC quarterly cap).
- **Standard deductions / personal allowances** — in
  jurisdictions that index allowances to inflation (US, UK, IN),
  confirm the new amount.
- **Filing thresholds** — confirm any change in the
  small-employer simplified-filing exemption (e.g. AU's
  $50k STSL threshold, NZ's KiwiSaver auto-enrol minimum).
- **GST/VAT rate** — confirm the standard rate AND the registered-
  threshold gross turnover ($60k NZ, $75k AU, SGD 1M SG, etc.).
- **Year-end YTD caps** — confirm any year-to-date cap that
  triggers a different rate above the cap (CH ALV solidarity, SA
  GOSI annual reset).

If any value changes:

1. Update the constant in the pack file (`internal/hr/taxpacks/<cc>.go`).
2. Update or add a regression case in the regional test file.
3. Reference the source (URL to the revenue service notice or
   gazetted regulation) in a comment next to the constant. Future
   maintainers should be able to verify the change without
   re-doing the research.
4. If the change is mid-year effective (less common), gate the
   new rate behind a date comparison rather than replacing the old
   constant outright.

## Updating an existing tax pack

Same flow as a year-end refresh, but in the comment block above
the constant being changed, include:

- The effective date of the change.
- The source URL (must be the revenue service or gazetted
  regulation, not a blog summary).
- A reference to the regression test case that pins the new
  behaviour.

Do NOT silently widen a tax band without a test — the regression
matrix is the only mechanism that catches a typo in the new band
boundary.

## Updating an i18n catalogue

Two kinds of catalogue change:

1. **Adding a new key** — add it to `en.json` in BOTH
   `internal/i18n/locales/` and `apps/web/src/locales/`. Then add
   the key to every other locale in both directories. The
   `TestEveryLocaleShipsBaselineKeys` (backend) and
   `TestFrontendBackendCatalogueParity` (cross-half) tests will
   fail until every locale has the new key.
2. **Updating a translation value** — replace the value in the
   target locale's catalogue file in both halves. Value drift
   between halves IS allowed (translator queues run independently
   on each side), but the keyset must stay identical.

For full RTL languages (ar), confirm the new copy makes sense as
inline-flow text — avoid embedded English punctuation that would
break the bidi algorithm.

## CI checks that catch drift

The following tests / workflows enforce the contracts in this
document. If you change any of the files mentioned above, run them
locally before opening the PR:

- `go test ./internal/hr/taxpacks/...` — pack registration,
  per-country regression matrices, GCC nationality helpers.
- `go test ./internal/tenant/...` — CoA template registration,
  country→CoA mapping (backend), locale validator.
- `go test ./internal/i18n/...` — backend bundle loader,
  Accept-Language middleware, every-locale-ships-baseline-keys
  check, frontend↔backend catalogue parity (skips when frontend
  not present in checkout, fails on real CI).
- `npm run build --workspace=apps/web` — frontend wizard
  references resolve, locale chunks emit cleanly.
- `.github/workflows/tax-rate-review.yml` — annual scheduled run
  on January 1st that opens an issue with the year-end review
  checklist pre-filled. Manual trigger via `workflow_dispatch` for
  off-cycle audits.

If you add a new country and any of these tests start failing
after your changes, the fix is almost always: a missing entry in
one of the maps in the table at the top of this document. Walk
through the table column-by-column and verify both halves agree.

## Triage convention for `📝 Info:` review notes

The repo's `Devin Review` integration posts notes flagged
`📝 Info:` for observations the bot itself labels as "intentional
/ not a bug / consistent with the codebase". The maintainer's job
is to reassess each note before deciding whether to act:

- If the observation matches existing intent (Swiss equity code 28
  outside the liability tree, AU IFRS-fallback for CoA, sticky
  user-chosen CoA template across country edits), reply on the
  thread documenting why no change is being made.
- If the observation surfaces a real gap the bot mis-labelled, fix
  it on a follow-up commit and reference the thread in the commit
  message.

Don't bulk-dismiss `📝 Info:` notes without reading them — the bot
sometimes flags real bugs as info when the code change looked
small.
