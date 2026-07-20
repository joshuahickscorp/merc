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
	if checked != 56 {
		t.Fatalf("checked %d protected routes, want 56", checked)
	}
}
