# Release signing, notarization, and external staging

Two of the shippability gauntlet's external items — signed distribution and
external strict-TLS staging — are blocked on credentials and infrastructure
rather than on code. This is the exact path for each, written against what is
actually on this machine as of 2026-08-09, not a generic recipe.

Everything marked **automated** already runs unattended. Everything marked
**yours** needs a human once; after that it is automated forever.

---

## 1. macOS signing and notarization

Driver: `scripts/notarize-macos-release.sh`. It signs, submits, staples,
verifies as Gatekeeper would, and writes a bound receipt to
`evidence/state/macos-signing-<version>.json`.

### Current state

```
$ security find-identity -v -p codesigning
     0 valid identities found
```

There is no Developer ID certificate on this machine, so nothing can be signed
today. The script fails closed and names the missing item rather than emitting
an unsigned artifact that looks signed — a build that silently skips signing is
worse than one that stops, because the first one ships.

### What you do once — the certificate (yours)

1. Enrol in the Apple Developer Program at <https://developer.apple.com> (99
   USD/yr). Individual enrolment is fine; the team id becomes part of your
   signing identity either way.
2. Create the certificate:
   - Xcode → Settings → Accounts → your Apple ID → Manage Certificates → **+** →
     **Developer ID Application**; or
   - Keychain Access → Certificate Assistant → Request a Certificate From a
     Certificate Authority (save to disk), then upload the CSR at
     <https://developer.apple.com/account/resources/certificates> choosing
     **Developer ID Application**, download the `.cer`, and double-click it.
3. Confirm:
   ```bash
   security find-identity -v -p codesigning
   ```
   A line reading `Developer ID Application: YOUR NAME (TEAMID)` must appear.

**Developer ID Application** is the right type. *Apple Development* and *Apple
Distribution* are for the App Store and TestFlight; neither notarizes for
direct download, which is how the merc CLI and agent are distributed.

### What you do once — the notary API key (yours)

1. <https://appstoreconnect.apple.com> → Users and Access → Integrations → App
   Store Connect API → **Generate API Key**, role **Developer**.
2. Download `AuthKey_XXXXXX.p8`. **Apple allows this download exactly once.**
   Store it outside the repository.
3. Note the **Key ID** and the **Issuer ID** from that page.
4. Store it in the keychain once so no secret has to live in your shell history:
   ```bash
   xcrun notarytool store-credentials merc-notary \
     --key /secure/AuthKey_XXXXXX.p8 --key-id XXXXXX --issuer <issuer-uuid>
   export MERC_NOTARY_PROFILE=merc-notary
   ```

### What runs unattended after that (automated)

```bash
scripts/build-cli-release.sh v1.2.3
scripts/notarize-macos-release.sh v1.2.3
```

The script then, with no further input:

- signs every executable with `--options runtime --timestamp` (hardened runtime
  and a secure timestamp are both notarization prerequisites; without them Apple
  rejects *after* upload, wasting a round trip per build)
- zips with `ditto` and submits with `notarytool submit --wait`, so the exit
  code is Apple's verdict rather than "upload succeeded"
- on rejection, prints `notarytool log` — the actual reason, not a status code
- staples the ticket to each binary, so the artifact validates **offline**;
  without stapling a machine with no network still shows the
  unidentified-developer dialog even though notarization succeeded
- runs `spctl --assess --type execute`, which is the check that catches a
  signed-but-not-hardened build — otherwise that only surfaces on a stranger's
  machine
- writes a receipt recording artifact SHA-256s, the submission id, and a
  `does_not_prove` field stating plainly that signing proves publisher identity
  and nothing about correctness or safety

`MERC_SKIP_NOTARIZE=1` signs and verifies locally without submitting, and
records `notarized: false` with `binding_status: UNBOUND` — useful before the
API key exists, and honest about what it did not do.

### Answering the obvious question

You do **not** notarize each release by hand. The one-time cost is the
certificate and the API key. After that a release is two commands, and it is
reasonable to run them from CI.

---

## 2. External strict-TLS staging on Cloudflare

### Current state — read this first

Credentials exist, in the **older** secrets file:

```
.merc-secrets.env   CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_API_TOKEN,
                    CLOUDFLARE_EMAIL, CLOUDFLARE_GLOBAL_API_KEY
```

Note that `.merc-credentials.env`, written more recently, does **not** carry
them. `scripts/merc-credentials.sh` rewrites its output file from scratch on
every run, so a value it does not re-emit is a value it drops — that is how
these ended up split across two files. Consolidate deliberately rather than by
accident.

Helper scripts already exist: `scripts/merc-cloudflare-key.sh`,
`scripts/cloudflare-purge.sh`, `scripts/cloudflare-teardown.sh`.

**The domains do not resolve.**

```
$ whois mermerc.net
No match for domain "MERMERC.NET"

$ dig NS mermerc.net @1.1.1.1   → empty
$ dig NS mermerc.app @1.1.1.1   → empty
$ dig SOA mermerc.app @1.1.1.1  → empty
```

`mermerc.net` is not registered. `mermerc.app` has no NS, no SOA and no A
record, so even if it is registered it is delegated to nothing and serves
nothing. Nothing can be deployed to either name in this state. Confirm the exact
spelling you own before anything below.

### Step 1 — register and delegate (yours)

Register the domain, then point it at Cloudflare: add the site in the Cloudflare
dashboard, take the two assigned nameservers, and set them at your registrar.
Delegation is live when this returns Cloudflare nameservers:

```bash
dig NS yourdomain.tld +short
```

`.app` is on the HSTS preload list, so **HTTPS is mandatory** — there is no
http:// fallback for testing. That is a good default for a staging host that
must prove strict TLS anyway.

### Step 2 — TLS mode (automated once delegated)

Set SSL/TLS mode to **Full (strict)**. Anything less is not strict TLS and does
not satisfy the gauntlet item:

- *Flexible* leaves the Cloudflare→origin hop unencrypted
- *Full* encrypts it but does not validate the origin certificate
- **Full (strict)** encrypts and validates — this is the one

For the origin, issue a Cloudflare Origin CA certificate and install it there,
then enable **Authenticated Origin Pulls** so the origin only accepts
connections from Cloudflare.

### Step 3 — the landing page (automated)

The repo already has `web/` with real pages (`buyer.html`, `admin.html`,
`prices.html`). For a deliberately blank landing screen, deploy a single-file
Worker or a Pages project serving one black page. Once credentials are in the
environment:

```bash
npm i -g wrangler        # not currently installed on this machine
wrangler login           # or export CLOUDFLARE_API_TOKEN
wrangler deploy
```

The API token needs `Zone:DNS:Edit` and `Workers Scripts:Edit` for the zone.
The Global API Key also works but authorizes everything on the account — prefer
the scoped token.

### Step 4 — what staging must prove for the gauntlet

A page that loads is not the item. The gauntlet asks for external strict-TLS
staging, which means:

- a real certificate chain a stranger's browser accepts, verifiable from off
  this machine
- the control plane reachable over that TLS, not just a static page
- an offsite restore drill against it
- paging that reaches a human who acknowledges

The first three are automatable once the domain exists. The fourth is
irreducibly a person — Bible §19 is explicit that no LLM may invent external
approval, and an acknowledgement nobody made is exactly that.

---

## 3. SBOM and licences — status

**SBOM: generator exists, automated.** `scripts/generate-sbom.py` →
`evidence/state/sbom.json` (unbound historical snapshot at `8e6b1024`;
CycloneDX 1.5, 639 components at that commit), generated from
`go list -m -json all` and `cargo metadata` rather than a scanner that would
itself need installing and trusting.

Licence picture from it: overwhelmingly permissive — 215 `MIT OR Apache-2.0`,
82 MIT, 20 `Apache-2.0 OR MIT`, 20 Unicode-3.0, 9 Apache-2.0. One copyleft
appearance, `r-efi` as `MIT OR Apache-2.0 OR LGPL-2.1-or-later`, which is a
choice — take MIT and the LGPL branch never applies. **No AGPL, no SSPL, no
GPL.** 236 Go components carry no declared licence because Go module metadata
has no licence field; they are recorded as undeclared rather than guessed
permissive, since a reviewer would act on the guess.

Model weights are deliberately **excluded** from the SBOM. They are third-party
artifacts with their own obligations, and two are BLOCKED — folding them in
would let a green SBOM read as a licence clearance it does not grant.

**Licences: three concrete items**, from `make license-register`:

1. **No owner-approved root project licence.** `agent/Cargo.toml` declares MIT
   but no licence text is tracked, and the Python package has no licence
   metadata. This is a decision only you can make; the mechanical part after it
   is minutes.
2. **Llama 3.2 1B Instruct Q4** — Meta Llama 3.2 Community Licence + AUP.
   Needs the agreement copy vendored, a "Built with Llama" attribution, and
   Notice attribution on distributed copies.
3. **all-MiniLM-L6-v2** — Apache-2.0. Needs notice preservation.

Both models are BLOCKED because the pin/hash enforcement is not
final-candidate-bound and no acceptance receipt or policy approval exists. The
register states explicitly that BLOCKED rows must not be edited to silence the
check — and the substance is a real legal question, because merc **prices and
serves** these models rather than merely linking them.

---

## 4. The live droplet is 727 commits behind on money code — read this first

Checked 2026-08-09 against the running host, not from documentation:

```
$ curl https://mercmerc.net/version
{"version":"dev","commit":"41db85b55ddd50bbc5551f2dda4aa2388a8f2b2d",
 "build_date":"unknown","modified":true,...}

$ git rev-list --count 41db85b5..HEAD
727
```

`evidence/deploy/live-cutover.json` is an unbound historical cutover record;
it records `stripe_mode: "live"` and carries its own warning: *"Stripe is LIVE.
Charges and payouts move real money."*

`"modified": true` means the running image was built from a dirty tree, so its
own provenance claim is already untrue — `MERC_BUILD_COMMIT` records the commit
argument regardless of whether the worktree was clean.

### What that host is missing, specifically

These are money-path commits, not cosmetics:

| Commit | What it fixes |
|---|---|
| `a7bf17a6` | **P0.** Realtime funding under-held open prepaid exposure — counted `estimated_usd` rather than the reserved residual, and had **no service-lease term at all**. Reserved is ~2× estimated (`ExtraTaskReserve = primaryTasks`), so it under-held about half of every batch reservation and the whole of every lease reservation. Whichever obligation settles second hits `errInsufficientPrepaid` *after* the supplier has worked: supplier credit posted against cash that cannot be collected. |
| `dd59aa62` | **P1.** Prepaid admission never took the buyer funding advisory lock that realtime and free-credit share, and realtime's balance read is not `FOR UPDATE`. Concurrent prepaid + realtime admits for one buyer could both pass and together exceed the balance. |
| `97ed15dd`, `7f51e9dc`, `11764383` | Currency authority, exact nanos vs rounded micros, and finishing in the currency the money was accepted in. |

Both defects were reproduced with failing-before tests in this session, so this
is a live exposure rather than a theoretical one.

### Why this is not fixed here

There is no SSH key to that host on this machine. Probed directly — `root`,
`merc`, `deploy`, `ubuntu` and `admin` all return
`Permission denied (publickey,password)` — and `docs/RUNBOOKS.md` says the same
in its own words: *"I have no SSH key to that host, so every step below is yours
to run."*

### The two honest options, in order of preference

1. **Disable live payments until the host is current.** `MERC_PAYMENT_MODE` is
   already documented as `sealed` "until the candidate-bound LIVE activation
   package exists". Sealing it removes the money exposure immediately and costs
   nothing but the canary's payment path.
2. **Deploy a current build.** The full production environment table and the
   image build/load steps are already prepared in `docs/RUNBOOKS.md` — nothing
   about them needs rediscovering. Two cautions from that runbook that are easy
   to get wrong and expensive to get wrong:
   - `MERC_TOKEN_KEY` must be copied **byte-for-byte**. `control/crypto.go`
     derives the AES key as `sha256(value)`; regenerating it makes every sealed
     OAuth token and webhook secret already in Postgres permanently
     undecryptable.
   - `MERC_VERIFICATION_SAMPLE_SECRET` must be set, or `control/verification.go`
     silently substitutes a **published** default sampling secret — which makes
     verification sampling predictable to anyone who reads the source.

Commit before building, so `/version` stops reporting `modified: true`.
