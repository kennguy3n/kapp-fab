# E2E Email Tests (Playwright + Mailosaur)

Automated end-to-end tests for email workflows using [Playwright](https://playwright.dev/) and [Mailosaur](https://mailosaur.com/).

## Test Cases

| TC ID | Description | Spec File |
|-------|-------------|-----------|
| TC034 | Welcome email is received after account creation | `welcome-email.spec.ts` |
| TC035 | Welcome email contains a valid .ovpn download link | `welcome-email.spec.ts` |
| TC036 | Downloaded .ovpn file contains correct server config | `welcome-email.spec.ts` |
| TC037 | Welcome email body matches design specification | `welcome-email.spec.ts` |
| TC083 | Employee receives invitation email in their inbox | `employee-invitation.spec.ts` |
| TC084 | Invitation email contains the correct installation link | `employee-invitation.spec.ts` |
| TC085 | Invitation link is unique per employee | `employee-invitation.spec.ts` |
| TC086 | Expired invitation link shows an error page | `employee-invitation.spec.ts` |
| TC087 | Resending invite sends a new unique link | `employee-invitation.spec.ts` |

## Setup

### 1. Install dependencies

```bash
npm install
```

### 2. Create a Mailosaur account

1. Sign up at [mailosaur.com](https://mailosaur.com/)
2. Create a server (sandbox) — note the **Server ID**
3. Copy your **API key** from Account → API Access

### 3. Configure environment variables

```bash
cp tests/e2e/.env.example tests/e2e/.env
```

Edit `tests/e2e/.env` with your Mailosaur credentials and application URLs.

### 4. Run tests

```bash
# Run all email E2E tests
npx playwright test --config tests/e2e/playwright.config.ts

# Run only welcome email tests
npx playwright test --config tests/e2e/playwright.config.ts welcome-email

# Run only invitation tests
npx playwright test --config tests/e2e/playwright.config.ts employee-invitation

# Run with headed browser for debugging
npx playwright test --config tests/e2e/playwright.config.ts --headed

# Run with trace viewer
npx playwright test --config tests/e2e/playwright.config.ts --trace on
```

Or use the npm script:

```bash
npm run test:e2e:email
```

## Architecture

```
tests/e2e/
├── playwright.config.ts          # Playwright config for email E2E tests
├── .env.example                  # Environment variable template
├── README.md                     # This file
├── fixtures/
│   └── auth.fixture.ts           # Auth fixtures (admin login, API context)
├── helpers/
│   ├── mailosaur.ts              # Mailosaur SDK wrapper
│   ├── ovpn-parser.ts            # .ovpn file parser
│   └── download.ts               # HTTP download helper
└── specs/
    ├── welcome-email.spec.ts     # TC034–TC037
    └── employee-invitation.spec.ts # TC083–TC087
```

## How it works

1. **Mailosaur** provides disposable email addresses routed to their API
2. Tests use these addresses when creating accounts or sending invitations
3. After the action triggers an email, the test polls Mailosaur's API until the email arrives
4. The email body, links, and attachments are inspected programmatically
5. For `.ovpn` files, the link is followed, the file downloaded, and its contents parsed

## TC086 – Expired invitation link

This test requires one of:

- **Backend time-travel API**: `POST /v1/internal/test/expire-invitation` that force-expires an invitation token. This is the cleanest approach.
- **Direct DB manipulation**: `UPDATE invitations SET expires_at = NOW() - INTERVAL '1 day' WHERE token = '...'`
- **Fallback**: The test tampers with the invitation URL to simulate an invalid/expired token.

For production-grade testing, implement the time-travel endpoint behind a feature flag that is only enabled in test environments.
