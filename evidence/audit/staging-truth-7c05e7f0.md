# Lane E — staging and candidate-identity truth

Observed: 2026-08-24T03:00:14Z through 2026-08-24T03:04:21Z (UTC).
Observer: unauthenticated GET-only probes from this worktree.
Host: `https://mercmerc.net` (A `192.241.134.31`, matching
`evidence/external/staging-alpha-readiness.json` host.address).
Candidate commit: `7c05e7f01fc29db497bee78220f608d1aa4f7746` (this worktree HEAD).
Constraint: no POST/PUT/PATCH/DELETE against staging; no credentials transmitted;
no redeploy/restart/reconfigure.

## Verdict

| Question | Answer |
|---|---|
| Staging reachable | **yes** |
| Deployed commit | **`0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10`** (from live `GET /version`) |
| Image digest | **UNDETERMINED live** (public HTTP does not report it). Last documented digest for this exact commit **and** `build_date` is `sha256:5c33d078c71a8e42a9a2c2cbaa5bf4722195423c65a1f13647684f8e9fa50253` in `evidence/external/head-rebuild-redeploy.json` |
| vs candidate `7c05e7f0` | **BEHIND (12 commits)** |
| Billing webhook path `/v1/stripe/webhook` | **HTTP 405** on GET, `Allow: POST` (route exists, POST-only) |
| Connect webhook path `/v1/stripe/connect-webhook` | **HTTP 405** on GET, `Allow: POST` (route exists, POST-only; distinct from billing) |
| Honest 100/100 at `7c05e7f0` bound to this host? | **No.** Stripe-matrix receipts point at this host; this host is still the 2026-08-17 `0ffbd52` image, not `7c05e7f0`. |

## 1. Live identity (health / version)

Repo surface (`git show HEAD:control/api.go`):

```
mux.HandleFunc("GET /healthz", s.handleHealthz)
mux.HandleFunc("GET /readyz", s.handleReadyz)
mux.HandleFunc("GET /version", s.handleVersion)
```

`handleVersion` returns `currentControlBuildInfo()` (`control/buildinfo.go`),
stamped at image build via `Dockerfile.control` ldflags
`-X main.controlCommit=${MERC_BUILD_COMMIT}`.

### `GET /readyz`

```
curl -sS --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 30 \
  -D - -o /tmp/laneE-body -w 'http=%{http_code} ip=%{remote_ip}\n' \
  -X GET https://mercmerc.net/readyz
```

```
HTTP/1.1 200 OK
Alt-Svc: h3=":443"; ma=2592000
Content-Length: 164
Content-Type: application/json
Date: Mon, 24 Aug 2026 03:00:14 GMT
Strict-Transport-Security: max-age=31536000
Via: 1.1 Caddy
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
X-Request-Id: 5be98cdd-c023-4c38-9f49-09030db4f5a7
(no Server header)

http=200 ip=192.241.134.31
{"live_value_movement":false,"payment_mode":"test","payment_recovery_active":true,"provider_enabled":true,"status":"ready","stripe_api_version":"2025-06-30.basil"}
```

Matches `ops/staging/compose.alpha.yml` (`MERC_ENV: staging`,
`MERC_PAYMENT_MODE: test`) and `const stripeAPIVersion = "2025-06-30.basil"`
in `control/stripe_api_contract.go`. `live_value_movement=false` is the
test-mode safety bit `/readyz` is supposed to publish.

Re-checked:

```
curl -sS --proto '=https' --tlsv1.2 https://mercmerc.net/readyz
{"live_value_movement":false,"payment_mode":"test","payment_recovery_active":true,"provider_enabled":true,"status":"ready","stripe_api_version":"2025-06-30.basil"}
```

### `GET /healthz`

```
curl -sS --proto '=https' --tlsv1.2 -X GET https://mercmerc.net/healthz
HTTP/1.1 200 OK
{"status":"ok"}
```

(`http=200 ip=192.241.134.31`, body 16 bytes.)

### `GET /version` — deployed commit

```
curl -sS --proto '=https' --tlsv1.2 https://mercmerc.net/version
```

```
{"version":"v0.1.0-merc-staging","commit":"0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10","build_date":"2026-08-17T20:24:26Z","go_version":"go1.26.6","platform":"linux/amd64","modified":false,"price_board_sha256":"440909b34593487d8bd5d00c1b65244cc0a971755507b665014334505a34f48c","price_board_source":"release","capability_matrix_sha256":"0b569a272b2bef7f553cad0efbe7bac9fc42a18944b16ac60b590763f497d60c","authority_document_sha256":"7c7538f6763054fc383da664d48779a8caea6735f67ad4749dbf6a341691d7be"}
```

| Field | Live value | Notes |
|---|---|---|
| `commit` | `0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10` | **not** `7c05e7f0` |
| `build_date` | `2026-08-17T20:24:26Z` | identical to `evidence/external/head-rebuild-redeploy.json` `build_date` |
| `version` | `v0.1.0-merc-staging` | staging overlay, not prod `dev` default |
| `modified` | `false` | clean VCS stamp |
| `price_board_sha256` | `440909b3…f48c` | equals `git show HEAD:pricing/board.json \| shasum -a 256` **and** `git show 0ffbd52:pricing/board.json \| shasum -a 256` |
| `capability_matrix_sha256` | `0b569a27…d60c` | equals `pinnedCapabilityMatrixDigest` in `control/capability_manifest_test.go` at HEAD |

The matching `commit` **and** `build_date` with the 2026-08-17 rebuild receipt
is evidence this is still that build, not a later rebuild of the same SHA.

## 2. Deployed commit vs candidate `7c05e7f0`

```
git rev-parse HEAD
7c05e7f01fc29db497bee78220f608d1aa4f7746

git merge-base 0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10 \
               7c05e7f01fc29db497bee78220f608d1aa4f7746
0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10

git merge-base --is-ancestor 0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10 \
                             7c05e7f01fc29db497bee78220f608d1aa4f7746
# exit 0 → deployed IS an ancestor of candidate

git rev-list --count 0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10..7c05e7f01fc29db497bee78220f608d1aa4f7746
12

git rev-list --count 7c05e7f01fc29db497bee78220f608d1aa4f7746..0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10
0

git log -1 --format='%H%n%ci%n%s' 0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10
0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10
2026-08-17 16:19:41 -0400
alpha: the price board was serving a seed that pointed at the superseded r4 receipt

git log -1 --format='%H%n%ci%n%s' 7c05e7f01fc29db497bee78220f608d1aa4f7746
7c05e7f01fc29db497bee78220f608d1aa4f7746
2026-08-23 22:43:49 -0400
alpha: Accounts v1 support enabled; connected-account creation PASSES
```

**Relation: staging is BEHIND the candidate by 12 commits.**
The 12 commits present in git at `7c05e7f0` and absent from the running process:

```
git log --format='%H %ci %s' \
  0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10..7c05e7f01fc29db497bee78220f608d1aa4f7746
```

```
7c05e7f01fc29db497bee78220f608d1aa4f7746 2026-08-23 22:43:49 -0400 alpha: Accounts v1 support enabled; connected-account creation PASSES
e010ffc3243419cd2d6e6f49af2d9e9f4f9b602c 2026-08-23 22:39:24 -0400 alpha: fix a NameError I shipped, and record that the platform-profile gate cleared
c9229b4b877669de9ee31c2587f985a2072b2c39 2026-08-23 22:33:11 -0400 alpha: correct the record — the Accounts v2 port did NOT land in 9d064ea9
a402acb7e70e0143eba2267c8217c9d00abf7580 2026-08-23 22:28:57 -0400 alpha: the receipt names the wall that exists, and redundancy has someone to be redundant with
9d064ea9578e7ea5ad9a62ac765b6bb0761423c4 2026-08-23 17:52:44 -0400 alpha: port connected-account creation to Accounts v2
ca6a6d3aa13d05e429c66c271645e9ee11ea95d7 2026-08-23 17:23:26 -0400 alpha: let live credentials sit beside the test ones without disabling the test matrix
6d917be1ff3a823107412153bc9a4bb3ed5a63c1 2026-08-17 17:27:23 -0400 alpha: refresh the Stripe matrix receipt from a real run against the Connect wall
67239524c07602126b3f69e27d710918b4122c60 2026-08-17 17:25:39 -0400 alpha: the named reviewer moves to public launch; the rehearsal stands on what it executed
23bbbb141d881dbda4f40669752bafa940f1b45b 2026-08-17 17:11:51 -0400 alpha: stamp the refreshed quote-refusal-chain receipt
323752954ae9237674f1d8dec240b96e9daf8fc4 2026-08-17 17:11:21 -0400 docs: record why the staging loop still refuses, and what it is not
77b36ea990fe5db690b1030fe1c245c739d36dc2 2026-08-17 16:49:35 -0400 alpha: redundancy may only replace a honeypot when redundancy can actually be independent
b0ba3352c6293b96983755189fdbced588f90259 2026-08-17 16:38:38 -0400 alpha: deploy both fixes, price board is up, and the next real gate is named
```

Counted with `git rev-list --count` = **12**.

`git diff --stat 0ffbd52..7c05e7f0` is 35 files, +2702/−424. Deploy/Caddy/compose
webhook routing files are **unchanged** except `ops/staging/alpha-participants.json`
(+6/−1). `control/api.go` webhook registrations are identical at both commits:

```
git grep -n 'stripe/webhook\|connect-webhook' \
  0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10 HEAD -- control/api.go
```

```
0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10:control/api.go:176:mux.HandleFunc("POST /v1/stripe/webhook", s.handleStripeWebhook)                          // unauthed; verified by signature
0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10:control/api.go:177:mux.HandleFunc("POST /v1/stripe/connect-webhook", s.handleConnectWebhook)                 // Connect account.updated; verified by signature
HEAD:control/api.go:176:mux.HandleFunc("POST /v1/stripe/webhook", s.handleStripeWebhook)                          // unauthed; verified by signature
HEAD:control/api.go:177:mux.HandleFunc("POST /v1/stripe/connect-webhook", s.handleConnectWebhook)                 // Connect account.updated; verified by signature
```

What staging is missing is therefore not the two webhook paths; it is the
twelve subsequent commits (Accounts v1 enablement, Stripe-matrix receipt
refresh, canary policy / verification / economic-authority edits, readiness
script changes). A readiness 100/100 claimed at `7c05e7f0` is not a claim
about the process answering at `mercmerc.net` today.

## 3. Image digest

Live `/version` has no digest field. Public HTTP cannot `docker inspect`.
This lane did not SSH to the droplet.

Last documented identity for this same `commit` + `build_date`, from
`evidence/external/head-rebuild-redeploy.json`:

```
host: mercmerc.net
candidate_commit: 0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10
candidate_image_id: sha256:5c33d078c71a8e42a9a2c2cbaa5bf4722195423c65a1f13647684f8e9fa50253
candidate_loaded_id: sha256:5c33d078c71a8e42a9a2c2cbaa5bf4722195423c65a1f13647684f8e9fa50253
build_date: 2026-08-17T20:24:26Z
finished_at: 2026-08-17T20:28:06Z
compose: docker compose -f docker-compose.prod.yml -f docker-compose.smallhost.yml -f docker-compose.canary.yml -f ops/staging/compose.alpha.yml
```

**Live-observed digest: UNDETERMINED.**
**Documented digest for this build: `sha256:5c33d078c71a8e42a9a2c2cbaa5bf4722195423c65a1f13647684f8e9fa50253`.**
The identical `build_date` makes a silent rebuild of `0ffbd52` unlikely, but
that is corroboration, not a live digest measurement.

## 4. Stripe webhook identities the repo expects

From `evidence/external/stripe-sandbox-matrix.json` `fixtures`
(and the later `connect_remainder.pre_connect` `GET /v1/webhook_endpoints` row):

| Role | Stripe endpoint id | URL the matrix bound refusals to |
|---|---|---|
| Billing | `we_1U5Cz2CwPLrR4vaYKXZzRjmn` | `https://mercmerc.net/v1/stripe/webhook` |
| Connect | `we_1U5Cz3CwPLrR4vaYVjElBvu8` | `https://mercmerc.net/v1/stripe/connect-webhook` |

Same file also names two **non-live** endpoint ids:

- `we_1U7npECwPLrR4vaYGg1kuBHw` — recreate probe; `deleted: true`;
  `probe_url` was `https://mercmerc.net/v1/stripe/connect-webhook-probe/20260824T024126Z-l9cn`
- `we_1U7npHCwPLrR4vaYfgzEMO1Q` — notes: `deleted unusable connect!=true recreate`

`harness.endpoint_ids_distinct` is `true`. Connect flag on the live connect
endpoint was `null` (Connect not enabled at matrix time). This lane did not
call Stripe's API (would require a secret).

### Live path distinctness (GET only)

Go registers both as POST-only. GET of an existing method-mismatched route is
405 with `Allow: POST`. GET of an unregistered path is 404.

```
curl -sS -D - -o /tmp/laneE-wh --proto '=https' --tlsv1.2 \
  --connect-timeout 10 --max-time 15 \
  -X GET https://mercmerc.net/v1/stripe/webhook
```

```
HTTP/2 405
allow: POST
content-type: text/plain; charset=utf-8
via: 1.1 Caddy
x-request-id: 9e615fca-7e41-4dd8-a4aa-4224857b6bb9
content-length: 19

Method Not Allowed
```

```
curl -sS -D - -o /tmp/laneE-cwh --proto '=https' --tlsv1.2 \
  --connect-timeout 10 --max-time 15 \
  -X GET https://mercmerc.net/v1/stripe/connect-webhook
```

```
HTTP/2 405
allow: POST
content-type: text/plain; charset=utf-8
via: 1.1 Caddy
x-request-id: 5e09fb1a-f132-4b05-8374-6e29788299de
content-length: 19

Method Not Allowed
```

Negative controls:

| GET | HTTP | Body |
|---|---|---|
| `/v1/stripe/webhook/` (trailing slash) | 404 | `404 page not found` |
| `/v1/stripe/webhook-probe-lane-e` | 404 | `404 page not found` |
| `/v1/stripe/connect-webhook-probe/20260824T024126Z-l9cn` | 404 | `404 page not found` |

The two webhook paths still exist as **distinct POST-only routes**. They do not
collapse into one handler alias, and the leftover recreate-probe URL is not
served. Signature-refusal HTTP 400 was **not** re-probed (POST prohibited).
Last recorded POST refusals remain in the matrix:
`live https://mercmerc.net/v1/stripe/webhook HTTP 400 invalid stripe signature;
connect HTTP 400 invalid stripe signature` (`refusals.signature`,
`run_id` `20260816T235949Z-l9nc`, connect remainder `20260824T024126Z-l9cn`).

## 5. Other public GET answers (exposure)

These are GET-only. Auth-gated APIs were probed without credentials.

| GET | HTTP | What it means |
|---|---|---|
| `/metrics` | **404**, 0 bytes | Caddyfile `@metrics path /metrics` / `respond @metrics 404`. Prometheus is not on the public vhost. |
| `/debug/pprof/` | **404** `404 page not found` | No pprof surface. |
| `/admin/workers` | **401** `{"error":"missing or malformed Authorization bearer token","code":"unauthorized","action":"authenticate"}` | Admin API refuses anonymous. |
| `/v1/me` | **401** same body | Buyer API refuses anonymous. |
| `/admin` (HTML shell) | **200** 7010 bytes | Public control-room page (`GET /admin` → `handleAdminRoom`); not the admin API. |
| `/`, `/buyer`, `/supplier`, `/prices` | **200** HTML | Public rooms, registered unauthenticated in `control/api.go`. |
| `/pricing/board.json` | **200** JSON catalogue | Public price board; `board_sha256` `440909b3…f48c`. |
| `/.well-known/security.txt` | **503** `{"error":"staffed security contact is not configured","code":"misconfigured","action":"contact_support"}` | `handleSecurityTxt` 503s in staging/production when `MERC_SECURITY_EMAIL` is unset. |
| `/v1/public/config` | **200** `{"schema_version":1,"settlement_currency":"usd","stripe_payment_form_enabled":false,"contacts":{"configured":false,"missing":["privacy_url","security_email","status_url","support_email","terms_url"]}}` | No `stripe_publishable_key` emitted. No secret-shaped values. Staffed contacts empty. |

Storage vhost (`Caddyfile` `{$STORAGE_HOST:storage.mercmerc.net}` reverse_proxies
MinIO):

```
curl -sS --proto '=https' --tlsv1.2 -X GET https://storage.mercmerc.net/minio/health/live
HTTP/2 200  content-length: 0

curl -sS --proto '=https' --tlsv1.2 -X GET https://storage.mercmerc.net/
HTTP/2 403  application/xml  AccessDenied  Resource=/

curl -sS --proto '=https' --tlsv1.2 -X GET https://storage.mercmerc.net/cx-jobs
HTTP/2 403  AccessDenied  BucketName=cx-jobs
```

Anonymous list/get is denied. The S3 API is on the public TLS name because
git's Caddyfile puts it there; that is config, not a surprise leak. Compose
binds host MinIO at `127.0.0.1:9000` only.

TCP from this observer to `192.241.134.31` (python `socket.connect`, 3s timeout):

```
:80 open
:443 open
:22 open
:2019 timed out     # Caddy admin is `admin 127.0.0.1:2019` in Caddyfile
:8080 timed out     # control LISTEN_ADDR :8080, not published
:9000 timed out     # minio published 127.0.0.1:9000
:9090 timed out     # prometheus published 127.0.0.1:9090
:5432 timed out     # postgres unpublished
```

Port 22 is the droplet's management listener, not an application compose
publish. It is open; this lane did not authenticate to it.

HTTP→HTTPS:

```
curl -sS -D - --proto '=http' -X GET http://mercmerc.net/readyz
HTTP/1.1 308 Permanent Redirect
Location: https://mercmerc.net/readyz
Server: Caddy
```

TLS:

```
echo | openssl s_client -connect mercmerc.net:443 -servername mercmerc.net \
  | openssl x509 -noout -subject -issuer -dates -ext subjectAltName
subject=CN=mercmerc.net
issuer=C=US, O=Let's Encrypt, CN=YE1
notBefore=Aug 16 21:12:38 2026 GMT
notAfter=Nov 14 21:12:37 2026 GMT
DNS:mercmerc.net

echo | openssl s_client -connect storage.mercmerc.net:443 \
  -servername storage.mercmerc.net \
  | openssl x509 -noout -subject -issuer -dates -ext subjectAltName
subject=CN=storage.mercmerc.net
issuer=C=US, O=Let's Encrypt, CN=YE2
notBefore=Aug 16 21:12:39 2026 GMT
notAfter=Nov 14 21:12:38 2026 GMT
DNS:storage.mercmerc.net
```

DNS: both names resolve to `192.241.134.31`.

HTTPS responses omit `Server` (Caddyfile `-Server` on the TLS site block).
The port-80 308 still sends `Server: Caddy` (Caddy's implicit HTTP listener,
not the SITE_HOST block).

CSP / HSTS / COOP / X-Frame-Options / Referrer-Policy / Permissions-Policy on
the control vhost match the Caddyfile at HEAD (and at `0ffbd52`; those files
are identical).

## 6. Drift list (git deploy config vs what the host answers)

`git diff --stat 0ffbd52..7c05e7f0 -- deploy/ Caddyfile docker-compose.prod.yml docker-compose.smallhost.yml docker-compose.canary.yml ops/staging/Caddyfile.alpha ops/staging/compose.alpha.yml`
was **empty**. `git diff --stat 0ffbd52..7c05e7f0 -- ops/staging/` showed only
`ops/staging/alpha-participants.json` (+6/−1). Live answers were compared to
that shared Caddy/compose shape.

1. **Process identity vs candidate (material).** Live `/version.commit` is
   `0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10`, not
   `7c05e7f01fc29db497bee78220f608d1aa4f7746`. Staging is **BEHIND 12 commits**.
   The Stripe matrix binds refusals to this host; this host is not the
   candidate the readiness program is about to score.
2. **Image digest not on the public surface.** `/version` reports commit /
   build_date / board hashes, not the compose image id. Digest is
   UNDETERMINED from GET probes. Last documented matching build:
   `sha256:5c33d078c71a8e42a9a2c2cbaa5bf4722195423c65a1f13647684f8e9fa50253`.
3. **Staffed contacts unset.** `GET /.well-known/security.txt` → 503
   `staffed security contact is not configured`. `GET /v1/public/config`
   lists all five contact fields missing and `stripe_payment_form_enabled: false`.
   Compose leaves `MERC_SECURITY_EMAIL` / `MERC_SUPPORT_EMAIL` / URL fields
   optional and empty-by-default; the running process is behaving as that
   empty config. Not a Caddyfile mismatch; it is a staging env gap versus a
   staffed production host.
4. **Port-80 `Server: Caddy`.** Caddyfile `-Server` strips the header on the
   TLS vhost (confirmed: no `Server` on HTTPS). The automatic HTTP 308 still
   advertises Caddy. Minor, and not a git-vs-live Caddyfile byte drift.
5. **No Caddyfile / compose public-behavior drift detected.** `/metrics` 404,
   `/readyz` 200 via Caddy, HSTS 31536000, control not on :8080, MinIO not on
   public :9000, prometheus not on public :9090, Caddy admin :2019 not
   reachable, payment_mode=test, live_value_movement=false, stripe API
   version `2025-06-30.basil`, both webhook paths POST-only and distinct.
6. **Webhook route source is unchanged** between deployed and candidate
   (`control/api.go` lines 176–177 identical). Path presence at HEAD is
   therefore not evidence the *candidate binary* is running.

**Should-not-expose findings (live):** none that contradict git. `/metrics`
is closed. pprof is closed. admin APIs 401. MinIO anonymous 403. No
publishable live key and no secret-shaped body on `/v1/public/config`.
The public `/admin` HTML shell is the documented unauthenticated room, not
an API leak.

## 7. Method limits

- No POST, so live signature-refusal 400 and cross-authority 400 were not
  re-measured. Distinctness is GET 405 vs GET 404.
- No Stripe API, so `we_…` ids were not re-listed against Stripe; they are
  repo-expected identities from `fixtures`.
- No droplet docker inspect, so image digest is UNDETERMINED live.
- Sparse checkout: deploy/control/Caddy files were read with `git show` /
  `git grep`; they exist in git at HEAD.

## 8. `python3 scripts/validate-readiness.py`

Run after this file was written, from the worktree root. This worktree is a
sparse checkout (`scripts/validate-readiness.py` and `evidence/` are on disk;
`ops/` is not). The script reads `ops/readiness.json` and `ops/go-no-go.json`
from the filesystem, not from git. Real output:

```
python3 scripts/validate-readiness.py
readiness: FAIL: cannot load readiness ledgers: [Errno 2] No such file or directory: '/Users/scammermike/.claude-grok/worktrees/E-staging-truth-20260823-225857/ops/readiness.json'
exit=1
```

Those ledgers exist in git (`git cat-file -e HEAD:ops/readiness.json` succeeds)
but are not materialized here. This lane did not run `git sparse-checkout add`
and did not write any path other than this file. The FAIL is a sparse-checkout
blocker for the readiness score, not a finding about staging. Staging identity
above does not depend on that score.
