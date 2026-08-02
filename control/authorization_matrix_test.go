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
	raw, err := os.ReadFile("../ops/authorization-matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix authorizationMatrix
	if err := json.Unmarshal(raw, &matrix); err != nil {
		t.Fatal(err)
	}

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
	if checked != 70 {
		t.Fatalf("checked %d protected routes, want 70", checked)
	}
}
