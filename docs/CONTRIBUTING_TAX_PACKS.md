# Contributing tax packs

This guide is the end-to-end walkthrough for adding (or maintaining)
a country tax pack in kapp-fab. It is meant to be read once before
your first pack and then skimmed for the per-country checklist on
every subsequent pack.

The repo already ships 44+ packs covering most major payroll
jurisdictions; the per-country shape is well-established. The goal of
the contribution model documented here is to let an accountant, a
domain expert, or an in-country partner ship a pack without having to
read the rest of the codebase first.

## Before you start

The minimum you need to know about a country to ship a pack:

- The **statutory deductions** the employer must withhold from gross
  pay. The set is jurisdiction-specific but typically includes:
  - Income tax (PAYE / withholding / pay-as-you-earn).
  - Employee social-security contributions (pension, health,
    unemployment, disability).
  - Any payroll-only levies (health-insurance contribution,
    solidarity surcharge, training levy, etc.).
- The **rates / brackets** for each deduction, sourced from the
  country's revenue authority (e.g. HMRC for the UK, the IRS for the
  US, the BMF for Germany).
- Whether any deduction has an **annual cap** (e.g. social-security
  contributions are usually capped at a wage ceiling).
- Whether the rates depend on **employee attributes** (residency
  status, age, marital status, number of children).
- The **effective date** of the rates you're using — the pack
  declares an `EffectiveYear()` so the payroll engine can warn
  operators when their slip's pay-period year diverges from it.

You DO NOT need to be a Go expert. The scaffold tool below generates
all the boilerplate; the contributor's work is to translate the
statutory bracket table into a bracket walk inside one function.

## Quick start: the scaffold tool

The fastest way to add a new pack is to run the scaffold CLI:

```bash
go run ./cmd/new-tax-pack -cc XX -name "Country Name"
```

For a country with a non-English default locale that the repo does
not already ship, pass `-locale` AND `-locale-name`:

```bash
go run ./cmd/new-tax-pack -cc XK -name "Kosovo" -locale sq -locale-name "Shqip"
```

The scaffold does six things:

1. Creates `internal/hr/taxpacks/<cc>.go` — a registration-only pack
   that implements the `TaxPack` interface but emits zero deductions
   until you fill in the rates.
2. Creates `internal/tenant/coa_templates/<cc>_basic.json` — an IFRS
   chart-of-accounts template with the standard 1xxx / 2xxx / 3xxx /
   4xxx / 5xxx / 6xxx / 7xxx hierarchy.
3. Patches `internal/tenant/wizard.go` — adds the `//go:embed`
   directive, the `chartOfAccountsTemplates` map entry, and the
   `DefaultCoATemplateForCountry` switch case.
4. Patches `apps/web/src/pages/SetupWizardPage.tsx` — adds the
   `COA_TEMPLATES` and `COUNTRY_COA_DEFAULTS` entries so the wizard's
   dropdown picks up the new chart.
5. If `-locale` was passed:
   - Patches `apps/web/src/lib/i18n/locales.ts` — adds the
     `SupportedLocales` entry and the `COUNTRY_LOCALE_DEFAULTS`
     mapping.
   - Patches `internal/tenant/wizard.go` — adds the
     `DefaultLocaleForCountry` switch case.
   - Creates two locale catalogue files (backend + frontend) seeded
     from `en.json` so the keyset-parity tests pass on day one.
6. Prints the next steps for you to do by hand.

Re-running the scaffold over an existing pack is a no-op for the
patches (every insertion has a `SkipIfPresent` guard) but will refuse
to clobber the pack file unless you pass `-force`. The expected
workflow for a year-end refresh is NOT to re-run the scaffold; it is
to open a follow-up PR against the existing pack.

## Per-country checklist (after the scaffold)

The scaffold output is a compileable skeleton — the contributor's
job is to flesh it out before opening the PR.

### 1. Fill in the statutory bracket table

Open `internal/hr/taxpacks/<cc>.go`. The scaffold's `ComputeWith-
holding` body returns no deductions. Replace it with the bracket walk
for the country's income tax. Reference any existing pack with a
similar tax system as a template:

- **Progressive bracket walk** — see `de.go` (Germany's
  Lohnsteuer), `fr.go` (France's PAS), `gb.go` (UK PAYE), or
  `us.go` (US federal withholding). The pattern is:

  ```go
  var brackets = []bracket{
      {Floor: dec(0),       Top: dec(11604),  Base: dec(0),     Rate: dec(0)},
      {Floor: dec(11604),   Top: dec(66760),  Base: dec(0),     Rate: dec(0.14)},
      // ... up to the open-ended top bracket
      {Floor: dec(277826),  Top: dec(0),      Base: dec(91686), Rate: dec(0.45)},
  }
  ```

  The `Base` value at each bracket is the cumulative tax at that
  bracket's floor, which lets the bracket-walk function compute
  the marginal tax for the slice without recomputing every lower
  bracket. See `TestBracketTablesAreContiguous` in `taxpacks_test.go`
  for the contiguity invariant.

- **Flat rate** — see `hu.go` (Hungary SZJA 15%), `ro.go` (Romania
  Impozit 10%), or `cz.go` (Czechia 15% + 23% over the cap). The
  pattern is much simpler:

  ```go
  tax := gross.Mul(dec(0.10))  // 10% flat rate
  ```

- **Step / table-based withholding** — see `no.go` (Norway
  Trekktabell). When the country publishes a step table rather than
  a bracket schedule, model it as a step function with discrete
  per-slice rates. The Trekktabell is the canonical example.

### 2. Add the period prorating

Every pack annualises the slip's gross before applying the bracket
walk, then prorates the tax back to the slip's period. The
convention is `periodFraction = days / 365.25` so a 31-day slip in
February gets the right fraction of an annual bracket. See the
existing packs for the exact pattern — the helper is local to each
pack to allow per-country calendar quirks (e.g. countries with
13-month statutory pay or fortnightly pay-as-you-earn schedules).

### 3. Add the YTD cap enforcement (when applicable)

Many social-security contributions are capped at an annual wage
ceiling (e.g. UK NIC upper earnings limit, German Rentenversicherung
Beitragsbemessungsgrenze, Swiss ALV CHF 148,200 ceiling). The pack
reads the year-to-date gross from `EmployeeInfo.YTDGross` and
decides whether the slip's contribution would push cumulative gross
over the ceiling — if so, only the slice below the ceiling counts.

See the cap logic in `pl.go` (PL ZUS), `se.go` (SE pensionsavgift),
`fi.go` (FI SAVA), `gr.go` (GR EFKA), `ch.go` (CH ALV), or `sa.go`
(SA GOSI) for the canonical patterns.

### 4. Author the Chart-of-Accounts template

Open `internal/tenant/coa_templates/<cc>_basic.json`. The scaffold
emits a flat IFRS hierarchy with one TODO line at code 2140 for the
country's payroll-liability account.

Replace the TODO line with one row per statutory deduction the pack
emits. For example, a pack that emits PAYE + NIC + Student Loan
deductions adds three rows:

```json
{"code": "2140", "name": "PAYE Payable", "type": "liability", "parent_code": "21"},
{"code": "2141", "name": "NIC Employee Payable", "type": "liability", "parent_code": "21"},
{"code": "2142", "name": "Student Loan Payable", "type": "liability", "parent_code": "21"},
```

The payroll posting hook in `internal/hr/payroll_engine.go` maps the
pack's `Deduction.Code` (e.g. `GB_PAYE`, `GB_NIC`, `GB_STUDENT_LOAN`)
to the chart's liability code, so the deduction line debits the
salary expense and credits the right statutory-liability account on
day one.

### 5. Write a regression test matrix

Open the regional test file (e.g. `europe_west_packs_test.go`,
`apac_packs_test.go`, `africa_eastasia_packs_test.go`) and add a
test case for the new pack covering at least:

- **Nominal salary** — one slip at the country's median pay (e.g.
  EUR 45,000 / year for Germany) with the expected deductions
  hand-derived from the statutory tables. Use a tight tolerance
  (±2 EUR or so for the income-tax line) so a rate-table typo of
  ≥0.5 percentage points fails the test.
- **Threshold crossings** — one slip just below and one slip just
  above any bracket boundary or cap ceiling.
- **YTD cap behaviour** — for packs with annual ceilings, one
  slip that would push cumulative gross over the cap, verifying
  only the slice below the cap is charged.
- **Zero / negative gross** — one slip with `gross = 0` and one
  with `gross < 0` (sometimes generated by claw-back adjustments).
  Both should return no deductions and no panics.
- **Zero-day period** — one slip where `PayPeriod.End` precedes
  `PayPeriod.Start`, exercising the `Days() == 0` guard.

The patterns vary per country; the existing 44 packs each have a
hand-tuned test matrix, so pick the one with the closest tax system
as a starting point.

### 6. Cite your sources

Every rate constant in the pack file MUST have a comment citing the
revenue authority's published source. Acceptable citations:

- A URL to the revenue authority's official rate page (e.g.
  https://www.gov.uk/government/publications/rates-and-allowances-for-income-tax,
  https://www.bmf.gv.at/themen/steuern/lohnsteuer.html).
- A reference to a gazette / regulation (e.g. "Federal Decree-Law
  No. 47 of 2022", "Royal Decree 142/2024").
- A reference to a tax-authority circular (e.g. "IRS Pub 15-T",
  "ATO Schedule 1 NAT 1004").

Citations that are **NOT** acceptable:

- A blog post or tax-news summary (rates change, blogs go stale).
- A consultancy's marketing page.
- An English translation of a non-English revenue-authority page
  without a link to the original (translation errors are common
  for tax terminology).

Per-comment citations let the next maintainer verify the rate
without redoing the research. The annual review workflow (see below)
opens a tracking issue every January 1st that points back to the
citations in each pack.

### 7. Update the locale catalogues (when applicable)

If you used `-locale` with a new tag, the scaffold seeded the two
locale catalogue files with the contents of `en.json` so the
keyset-parity tests pass. Translate the values to the country's
language before opening the PR. For RTL languages (Arabic, Hebrew),
also set `direction: "rtl"` in the `SupportedLocales` entry the
scaffold added.

### 8. Run the test matrix locally

```bash
go test ./internal/hr/taxpacks/...
go test ./internal/tenant/...
go test ./internal/i18n/...
go test ./cmd/new-tax-pack/...
npm run build --workspace=apps/web
golangci-lint run ./internal/hr/taxpacks/... ./internal/tenant/... ./cmd/new-tax-pack/...
```

All of the above must pass before opening the PR. CI re-runs the
same checks; failing locally is faster than waiting for CI.

### 9. Update the maintenance docs

Add a row to the regional table in `docs/TAX_PACK_MAINTENANCE.md`
covering the new pack with:

- Pack file path.
- CoA template path.
- Locale tag.
- Key statutory sources (the URLs you cited in your pack comments).
- Review cadence (annual / quarterly / on-demand). Most countries
  are annual (January); a few jurisdictions republish rates more
  frequently — Argentina (ARCA / AFIP resolutions) and Mexico (SAT
  Anexo 8) are quarterly.

### 10. Open the PR

Branch naming convention: `devin/<timestamp>-<cc>-pack` or
`<contributor>/<cc>-pack`.

The PR description should include:

- A one-line summary of the deductions the pack emits.
- The fiscal year the rates are calibrated for.
- Links to the citation URLs (so a reviewer can verify without
  re-deriving the brackets from the rate page).
- A note on any deliberate simplifications (e.g. "this pack uses
  the commune mean rate for Denmark; per-commune resolution from
  the Skattekort is tracked as a follow-up").

CI runs the full test matrix on every PR. The tax-pack-pr workflow
specifically targets PRs touching `internal/hr/taxpacks/` or
`internal/tenant/coa_templates/` and runs only the tests relevant to
the change so you get fast feedback.

## Maintenance: the annual review

Every January 1st, `.github/workflows/tax-rate-review.yml` opens a
GitHub issue with a per-country checklist pre-filled. The issue
template lists every registered pack with a checkbox; a maintainer
walks the list, verifies the rate constants against the revenue
authority's published rates for the new fiscal year, and ticks each
country once verified.

Mid-year rate changes happen too — the workflow supports
`workflow_dispatch` for off-cycle audits. If a country publishes a
rate change mid-year, open a PR with the new rates AND a date-gated
constant (the existing pattern is to keep both the old and new rate
in the pack, gated by a date comparison, so historical slips re-run
with the correct rate).

## Partner program

For organisations that want to own the long-term maintenance of a
country pack (an in-country accounting firm, a payroll-software
partner, a domain expert in a specific tax system), the partner
program provides:

- **Named-reviewer access** — the partner's lead maintainer is
  added as a code owner for the country's pack files (the
  `CODEOWNERS` file is updated per partner agreement). Any PR
  touching the country's pack requires the partner's approval.
- **Scoped commit access** — the partner has direct commit access
  scoped to `internal/hr/taxpacks/<cc>.go`,
  `internal/tenant/coa_templates/<cc>_basic.json`, and the locale
  catalogues for that country. Cross-cutting refactors of the
  `TaxPack` interface still go through core maintainers.
- **Annual review responsibility** — the partner owns the year-end
  review for their country and is responsible for closing the
  partner's row on the January tracking issue.
- **Credit** — the partner's name (firm or individual) is listed
  in the pack's docstring header and on the `docs/TAX_PACK_-
  MAINTENANCE.md` table.

To propose a partner agreement, open an issue with the label
`partner-program` and the country you would like to maintain. Core
maintainers will follow up with the agreement template.

## Asking for help

If you get stuck on a country-specific quirk:

- Open a draft PR with the work in progress and add a `# Question`
  section to the description; core maintainers review weekly.
- Open a GitHub Discussion in the `tax-packs` category with the
  citation URL and a description of the ambiguity.
- For partner-program participants, use the partner Slack channel
  in the kapp-fab partner space.

The existing 44 packs cover most common tax-system shapes; if your
country has a wrinkle that isn't covered by any of them, please
flag it in the PR so the next contributor knows to look there.
