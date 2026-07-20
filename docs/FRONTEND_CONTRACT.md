# Frontend and API contract

The private-pilot UI is a thin client of the control API. PostgreSQL remains the
lifecycle and money authority; browser state is never authoritative.

| UI flow | API | Success state | Required failure states |
|---|---|---|---|
| Sign up / sign in | `POST /v1/signup`, `POST /v1/login` | revocable session; `Cache-Control: no-store` | validation, throttled, invalid credential |
| Identify account | `GET /v1/me` | buyer id, email, remaining credit | expired/revoked session |
| Discover capacity | `GET /v1/models`, `POST /v1/quote` | supported model and bounded quote | unsupported tuple, no eligible capacity, malformed input |
| Submit | `POST /v1/jobs` with `Idempotency-Key` | one job id, estimate, webhook secret once | 402 funding, 409 key/body conflict, capacity, validation |
| Track | `GET /v1/jobs/{id}`, `/events`, `/failures` | explicit lifecycle plus typed failures | 404 for absent or other-buyer ids |
| Cancel | `DELETE /v1/jobs/{id}` | repeatable `cancelled` response while owned | 409 once work or verification makes cancellation unsafe |
| Download | `GET /v1/jobs/{id}/results` | buyer-scoped result references | incomplete, missing artifact, integrity failure |
| Invoice / receipt | `/invoice`, `/receipt` | estimated and actual economics are labeled | incomplete settlement, provider outcome unknown |
| Supplier onboarding | `/v1/supplier/*` | connected status and revocable device credential | KYC/provider pending, revoked enrollment, unsupported Mac |

Every buyer request must treat `401` as re-authentication, `403` as lack of
authority, `404` as an opaque object miss, `409` as a state/idempotency conflict,
`422` as a validation failure, `429` as backoff, and `5xx` as retryable only when
the operation is naturally idempotent or the same idempotency key is reused.

The current checked-in website is intentionally minimal. A richer dashboard must
not invent optimistic success: queued, running, verifying, complete, failed, and
cancelled are distinct, and payment/provider `outcome_unknown` must be shown as
pending operator resolution rather than success or failure.
