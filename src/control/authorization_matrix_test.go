package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type authorizationMatrix struct {
	RouteClasses []struct {
		ID     string   `json:"id"`
		Routes []string `json:"routes"`
	} `json:"route_classes"`
}

func concreteAuthorizationPath(pattern string) string {
	path := strings.SplitN(pattern, " ", 2)[1]
	path = strings.ReplaceAll(path, "{$}", "")
	path = strings.ReplaceAll(path, "{path...}", "test.css")
	path = strings.ReplaceAll(path, "{id}", "00000000-0000-4000-8000-000000000001")
	path = strings.ReplaceAll(path, "{name}", "dispatch")
	return path
}

func TestAuthorizationMatrixProtectedRoutesRejectAnonymousAndWrongCredentialNamespace(t *testing.T) {
	raw, err := os.ReadFile("../../ops/authorization-matrix.json")
	must(t, err)
	var matrix authorizationMatrix
	must(t, json.Unmarshal(raw, &matrix))

	checked := 0
	for _, class := range matrix.RouteClasses {
		if class.ID != "buyer_owned" && class.ID != "worker_owned" && class.ID != "operator" {
			continue
		}
		for _, pattern := range class.Routes {
			parts := strings.SplitN(pattern, " ", 2)
			for _, mode := range []string{"anonymous", "wrong_namespace"} {
				req := httptest.NewRequest(parts[0], concreteAuthorizationPath(pattern), nil)
				if mode == "wrong_namespace" {
					if class.ID == "worker_owned" {
						req.Header.Set("Authorization", "Bearer cx_wrong_namespace")
					} else {
						req.Header.Set("X-Worker-Token", "cxw_wrong_namespace")
					}
				}
				rec := httptest.NewRecorder()
				// A fresh server keeps this exhaustive auth test independent of the
				// outer per-IP abuse limiter; the assertion is about the route's
				// credential middleware, not aggregate request rate.
				NewServer(nil, nil, nil, nil).Routes().ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s %s: got %d, want 401", pattern, mode, rec.Code)
				}
			}
			checked++
		}
	}
	// 62 after the image lane: POST /v1/images/generations is buyer-owned and
	// must reject an anonymous caller like every other buyer route.
	// 61 after the [KEEP-RT] reversal: the five realtime routes (chat/completions,
	// the receipt read, two worker offer endpoints and the admin refund) returned
	// with the lane once CUDA hardware admission made it servable.
	// 63 after the perf lane: GET /admin/plan-accuracy reports realized-versus-
	// predicted planning error and is operator-only like every other admin read.
	// 65 after the funding lane: POST /v1/billing/topup is the buyer's only way
	// to put money in, and POST /admin/buyers/{id}/prepaid-refund is the operator
	// authority that takes it back out over card rails.
	// 66 once GET /v1/worker/viability entered the reviewed matrix. The route has
	// been registered through authWorker since before this session; the matrix
	// simply did not list it, which is why the coverage validator in `make ci` was
	// already failing. It is worker_owned like every other /v1/worker route: a
	// worker asking why it is or is not being offered work, about itself.
	// 67 after GET /v1/jobs was added to the review inventory. The route already
	// uses authBuyer and its handler scopes every result to the authenticated
	// buyer; this exhaustive check ensures its middleware cannot be bypassed.
	// 69 after the project-order reservation API: both create and buyer-scoped
	// read carry the same buyer credential boundary as the firm jobs they govern.
	// 70 after the measured RuntimeSelector regret report entered the operator
	// surface. It is read-only, but still carries the same admin boundary as the
	// rest of the operator evidence routes.
	// 71 after the scope-pinned RuntimeSelector promotion gate entered the same
	// operator evidence surface. It evaluates only; activation remains a
	// separate audited policy write.
	// 72 after the authenticated RuntimeSelector rollback caller entered the same
	// operator surface. It writes a new forward revision through the store; it
	// cannot edit or delete the evidence it rolls back.
	// 88 after the service-lease, fabric, and market-liquidity routes entered the
	// reviewed matrix. The matrix includes public routes too; this assertion
	// covers the protected subset exercised by the credential-bound middleware.
	// 89 after the reserved service-lease buyer data plane entered the same
	// ownership boundary; it is delivery under an existing lease, not a second
	// realtime billing authority.
	// 90 after the authenticated project compiler/probe route entered the buyer
	// surface. It returns proposal evidence only; it cannot quote or execute.
	// 91 after the buyer-scoped durable project compile receipt read entered the
	// same ownership boundary; it returns the stored IR evidence only.
	// 92 after the buyer-scoped deterministic render-unit read entered the same
	// boundary; it expands IR only and remains explicitly non-executable.
	// 93 after the composed operator network-liquidity receipt entered the
	// reviewed operator surface; it combines retained lane evidence only.
	// 94 after worker-scoped immutable topology evaluation replay entered the
	// fabric evidence surface; it cannot promote local placement.
	// 96 after buyer-scoped render assembly manifests and immutable receipt reads
	// entered the rendering evidence surface; neither route can execute or settle.
	// 98 after the worker-authenticated LoRA evaluation report and buyer-scoped
	// receipt read entered the outcome-evidence surface; neither route can train,
	// deploy, charge, pay, or settle.
	// 99 after the operator dispute-resolution route entered the admin surface.
	// An unresolvable dispute previously had no exit at all — no route, buyer or
	// admin, called SetDisputeStatus — so a supplier's payout stayed blocked
	// until someone ran SQL against production. The route resolves a terminal
	// dispute and records who did it and on what basis; it cannot open one, and
	// it cannot move money except by unblocking a payout the ledger already owed.
	// 101 after the two buyer-owned execution-envelope routes entered the matrix.
	// They were registered through authBuyer at api.go:151-152 and had been taking
	// buyer traffic while absent from the reviewed inventory, so nobody had
	// decided what each role may do with them. Listing them puts them under this
	// exhaustive anonymous/wrong-namespace check like every other buyer route;
	// neither can create or move money outside the envelope's own bounded cap.
	// 103 after the two operator selector routes joined the matrix: applying an
	// activation policy, and submitting a directed job onto a named non-routable
	// cell. Both are authAdmin, and both are now covered by this exhaustive
	// anonymous / wrong-namespace check like every other protected route. Neither
	// can move money, and neither can promote a cell without the promotion gate.
	// 104 after GET /v1/worker/ledger entered the worker_owned surface: the
	// per-credit payout trail a supplier sees beside earnings aggregates. Same
	// worker token boundary as /v1/worker/earnings; no cash movement.
	// 106 after the versioned UI composition reads (GET /v1/ui/v1/buy buyer-owned,
	// GET /v1/ui/v1/earn worker-owned). They copy existing handler bodies; they
	// cannot quote, submit, or pay. The operator-only Stripe failure observation
	// read is the 108th protected route.
	if checked != 108 {
		t.Fatalf("checked %d protected routes, want 108", checked)
	}
}
