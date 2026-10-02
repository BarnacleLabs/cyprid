# Security policy

cyprid installs and runs other people's software on your machine, so we take
security reports seriously. Thank you for helping keep its users safe.

## Reporting a vulnerability

**Please don't open a public issue for a security problem.** Report it privately
instead: go to this repository's **Security** tab and click **Report a
vulnerability**. Only the maintainers can see the report.

Please include:

- the commit you tested (`git log -1` in the cyprid folder), or the release you downloaded
- what an attacker could do, and what they need first (another account on the same
  machine, network access, getting the user to open a web page, and so on)
- steps to reproduce it, ideally the smallest example that shows the problem
- your macOS version and Mac model, if it matters

## What to expect

We're a small team, so these are aims rather than guarantees:

- we'll acknowledge your report within **5 working days**
- we'll tell you whether we can reproduce it and how serious we think it is within
  **14 days**
- for serious problems, we aim to release a fix within **30 days**, and we'll keep
  you updated if it takes longer

We'll credit you in the fix's release notes unless you'd rather we didn't. Please
give us a reasonable time to release a fix before disclosing the problem publicly.

## Supported versions

Fixes go into the latest version on the `main` branch and the next release. Older
versions don't get separate fixes: please update.

## Scope

Especially relevant, because they're where cyprid runs code or guards access:

- **the web page and admin API** (`/admin`), which can install models, start
  processes and edit recipes, and so can run commands on the user's machine
- **the checks that keep the admin API local**: the loopback, Host-header and
  same-origin checks, and their protection against CSRF and DNS rebinding
- **network mode**: the API key check, and anything that lets another machine reach
  more than the question API
- **installing and downloading**: recipe handling, the pinned-version and checksum
  checks, the embedded workers in `shims/`, and deleting weights (which must never
  remove files outside the Hugging Face cache)
- **the question API** (`/v1/systemone`): request handling, passing requests to
  models, and handling of API keys (including `TYPESAFE_API_KEY`)

Out of scope (please report these to the project concerned instead):

- vulnerabilities in the models, their Python packages or their servers (Kev,
  decider, Laya, Von, OpenJev, jevmlx, GLiNER, Cygnet, Clef and others), in `uv`, Go or
  macOS
- the behaviour of a recipe the user wrote or chose to install themselves: recipes
  decide which commands run, so a malicious recipe is equivalent to running
  malicious software
- attacks that need an account that is already the same user as the one running
  cyprid, since that account can already run anything cyprid can

## How the admin API is protected

For context when assessing a report: the admin API and web page accept requests only
from the same machine, addressed to localhost, from cyprid's own page (defeating
CSRF and DNS rebinding), and carrying the per-user access token from
`~/.cyprid/admin-token` (mode 0600), sent by the CLI as a header or by a browser as
an HttpOnly, SameSite=Strict cookie obtained through a one-time sign-in link. Other
accounts on a shared machine therefore can't use it. The question API (`/v1/…`)
is deliberately open to local programs without a token, like Ollama's; in network
mode, other machines need the separate API key.
