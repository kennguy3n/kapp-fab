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

## PR-2d Americas pack details (CA + 14 LATAM)

The Americas batch is unusually rate-sensitive — five of the
LATAM jurisdictions republish their tax tables more than once a
year (Argentina via ARCA/AFIP resolutions, Mexico via SAT Anexo 8
when UMA shifts, Peru/Colombia/Chile via annual UIT/UVT/UTM
revaluations indexed to the CPI). Maintainers should expect to
review these packs at minimum once per calendar year and again
mid-year for AR.

| Country | Pack file                          | CoA template            | Locale | Key sources                                                                                                                                                                                                                                                                                                  | Review cadence                |
| ------- | ---------------------------------- | ----------------------- | ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------|
| CA      | `internal/hr/taxpacks/ca.go`       | `ca_aspe_basic.json`    | en, fr-CA (QC) | CRA T4127 (federal payroll deductions formulas), Revenu Québec TP-1015.G (QC formulas), CRA T4032 (CPP / EI maximums published each November for the following year), Revenu Québec QPP rates, ESDC EI premium rate notice. | Annual (November / January)   |
| BR      | `internal/hr/taxpacks/br.go`       | `br_cpc_basic.json`     | pt-BR  | Receita Federal IRRF tables (Tabela Progressiva Mensal — current version Lei 14.848/2024), Portaria Interministerial MTE/MPS for INSS thresholds, Caixa Econômica Federal for FGTS (8% rate is statutory).                                                                                                  | Annual + ad-hoc on indexation |
| MX      | `internal/hr/taxpacks/mx.go`       | `mx_nif_basic.json`     | es     | SAT Anexo 8 Art. 96 ISR table, SAT Subsidio para el Empleo table, IMSS cuotas obrero-patronales, UMA value (INEGI annual revaluation each February).                                                                                                                                                       | Annual (February) + UMA shift |
| AR      | `internal/hr/taxpacks/ar.go`       | `ar_rtfacpce_basic.json`| es     | ARCA / AFIP RG escalas Ganancias 4ta categoría (multi-year history: RG 5008/21, RG 5363/23, RG 5417/23, RG 5453/24, RG 5531/2024 inflation top-up); MTEYSS for jubilación / INSSJP / obra social rates. **Multi-update cadence**: AR revises mínimo no imponible and special deduction whenever cumulative monthly CPI exceeds the legal trigger (presently every 6 months). | Quarterly / on each ARCA RG   |
| CO      | `internal/hr/taxpacks/co.go`       | `latam_ifrs_basic.json` | es     | DIAN Resolución UVT (annual, December for following year); Article 383 Estatuto Tributario (retención progresiva); Decreto Único Reglamentario for FSP thresholds; MinTrabajo for SMLMV.                                                                                                                  | Annual (January)              |
| CL      | `internal/hr/taxpacks/cl.go`       | `cl_ifrs_basic.json`    | es     | SII Impuesto Único de Segunda Categoría monthly table (UTM-indexed), Superintendencia de Pensiones for AFP fund-administrator rates, Superintendencia de Salud for Fonasa/Isapre 7% rule, AFC for Seguro de Cesantía rates.                                                                                | Monthly UTM, annual rate review |
| PE      | `internal/hr/taxpacks/pe.go`       | `latam_ifrs_basic.json` | es     | SUNAT Renta de Quinta Categoría tables (UIT-indexed annually), ONP for pension rate, EsSalud for employer health-insurance rate.                                                                                                                                                                          | Annual (January, UIT change)  |
| CR      | `internal/hr/taxpacks/cr.go`       | `latam_ifrs_basic.json` | es     | Ministerio de Hacienda / Dirección General de Tributación Direct Resolution on Impuesto al Salario thresholds, CCSS for SEM/IVM/Banco Popular employee rates.                                                                                                                                              | Annual (January)              |
| PA      | `internal/hr/taxpacks/pa.go`       | `latam_ifrs_basic.json` | es     | DGI Impuesto Sobre la Renta tables (Decreto Ejecutivo 170), CSS for cuota obrera (9.75%), Seguro Educativo statutory rate (1.25% employee).                                                                                                                                                                | Multi-year (rates very stable)|
| UY      | `internal/hr/taxpacks/uy.go`       | `latam_ifrs_basic.json` | es     | DGI IRPF Categoría II tables (BPC-indexed annually), BPS for Jubilación / FONASA / FRL rates, Decreto del Poder Ejecutivo for BPC adjustment each January.                                                                                                                                                  | Annual (January, BPC change)  |
| EC      | `internal/hr/taxpacks/ec.go`       | `latam_ifrs_basic.json` | es     | SRI Impuesto a la Renta de Personas Naturales tables (USD; thresholds revalued when CPI exceeds 5%), IESS for 9.45% employee rate.                                                                                                                                                                        | Annual (January) + CPI gate   |
| DO      | `internal/hr/taxpacks/do.go`       | `latam_ifrs_basic.json` | es     | DGII Impuesto Sobre la Renta tables (Decreto 273-11; thresholds revalued annually), SIPEN / SISALRIL for AFP & SFS rates, IDOPPRIL for SRL (employer-only).                                                                                                                                              | Annual (January)              |
| GT      | `internal/hr/taxpacks/gt.go`       | `latam_ifrs_basic.json` | es     | SAT Guatemala ISR Régimen Sobre Utilidades / Régimen Opcional Simplificado rates (Ley del ISR Decreto 10-2012), IGSS for 4.83% employee rate.                                                                                                                                                              | Multi-year (rates very stable)|
| PY      | `internal/hr/taxpacks/py.go`       | `latam_ifrs_basic.json` | es     | DNIT (formerly SET) IRP tables (jornales mínimos-indexed; jornal mínimo set by Ministerio de Trabajo each March/April), IPS for 9% employee rate.                                                                                                                                                          | Annual (April, jornal change) |
| TT      | `internal/hr/taxpacks/tt.go`       | `latam_ifrs_basic.json` | en     | Inland Revenue Division (IRD) Trinidad & Tobago PAYE rates (Income Tax Act Chap 75:01), NIBTT Class Earnings Schedule (16-class table), IRD Health Surcharge schedule.                                                                                                                                  | Annual (October budget read)  |

### Argentina inflation-adjustment workflow

AR is the most maintenance-intensive pack in the batch. Whenever
ARCA / AFIP publishes a resolución general updating the mínimo no
imponible, special deduction, or any bracket boundary, the
maintainer should:

1. Locate the published `escala` table in the Boletín Oficial.
2. Update the named constants at the top of `internal/hr/taxpacks/ar.go`
   (one constant per row, with the resolution number in the
   `// Source:` comment).
3. Update `EffectiveYear()` if the change crosses a fiscal year
   boundary.
4. Add a regression case to `americas_packs_test.go` pinning the
   new bracket boundary against the gazetted table.
5. Re-run `go test ./internal/hr/taxpacks/` and confirm the
   bracket-contiguity invariant (`TestAmericasBracketTablesAreContiguous`)
   still holds. AR is a strict-contiguity table (no published-
   rounding tolerance) so any drift here is a real transcription
   error.

### Mexico SAT-rounded table tolerance

MX ISR (Art. 96) is the exception to the strict bracket-contiguity
invariant. SAT publishes the `cuota_fija` column rounded to 2
decimal places, so the mathematically-derived contiguity check
fails by a few centavos. `TestAmericasBracketTablesAreContiguous`
accepts up to MXN 0.10 of drift per row for MX — anything larger
is treated as a transcription error. The published SAT table
remains the source of truth; do not "fix" the cuota_fija values
to mathematical precision when refreshing the rates.

### Canada CPP / EI annual update workflow

CRA publishes the next-year CPP and EI maximums in November of
the preceding year (T4032 / EI Premium Reduction Program notice).
The maintainer should:

1. Update `caCPPYMPE`, `caCPP2AdditionalCeiling`, `caCPPBasicExemption`,
   `caEIMaximumInsurableEarnings`, and the rate constants in
   `internal/hr/taxpacks/ca.go`.
2. Update QC's QPP and QPIP equivalents from the matching Revenu
   Québec notice.
3. For each of the 13 provinces, check the provincial budget
   address (March-May for most, late-Spring for QC) for indexation
   of provincial bracket thresholds and Basic Personal Amount.
4. Add a regression case to `americas_packs_test.go` for any
   province whose brackets shifted — at minimum ON, QC, BC, AB
   (the four largest by employee population) should always have a
   pinned case.

## Community contribution model

For contributors adding a brand-new country pack (rather than
maintaining an existing one), the end-to-end walkthrough lives in
[`docs/CONTRIBUTING_TAX_PACKS.md`](CONTRIBUTING_TAX_PACKS.md). The
short version:

1. Run the scaffold CLI: `go run ./cmd/new-tax-pack -cc XX -name "Country Name"`.
   This generates a compileable skeleton across all the files this
   document touches (pack, CoA, wizard maps, optional locale).
2. Fill in the statutory rates, brackets, and caps in the generated
   pack file with citations to the revenue authority's published
   source.
3. Author the regression test matrix for the regional test file.
4. Open a PR; the `tax-pack-pr` workflow runs scoped tests + lint
   on every push.

The annual review workflow (`.github/workflows/tax-rate-review.yml`)
now covers every registered pack. The January-1st auto-opened issue
walks the full per-country checklist; mid-year rate changes get a
manual `workflow_dispatch` trigger.

For organisations interested in long-term ownership of a country
pack, the partner-program section in `CONTRIBUTING_TAX_PACKS.md`
documents the named-reviewer + scoped commit-access model.
