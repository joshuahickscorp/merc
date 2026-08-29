package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAdminMoneyAuthorityBodiesAreStrictAndBounded(t *testing.T) {
	server := &Server{}
	tests := []struct {
		name       string
		body       string
		subsidize  bool
		wantStatus int
		wantError  string
	}{
		{
			name: "fund unknown field",
			body: `{"fund_ref":"fund-1","external_treasury_ref":"treasury-1",` +
				`"authorized_cents":100,"reason":"test","unexpected":true}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "unknown field",
		},
		{
			name: "fund trailing object",
			body: `{"fund_ref":"fund-1","external_treasury_ref":"treasury-1",` +
				`"authorized_cents":100,"reason":"test"} {"second":true}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "multiple JSON values",
		},
		{
			name: "subsidy unknown field",
			body: `{"fund_ref":"fund-1","authorization_ref":"auth-1",` +
				`"reason":"test","unexpected":true}`,
			subsidize:  true,
			wantStatus: http.StatusBadRequest,
			wantError:  "unknown field",
		},
		{
			name: "subsidy trailing scalar",
			body: `{"fund_ref":"fund-1","authorization_ref":"auth-1",` +
				`"reason":"test"} true`,
			subsidize:  true,
			wantStatus: http.StatusBadRequest,
			wantError:  "multiple JSON values",
		},
		{
			name: "fund duplicate cents",
			body: `{"fund_ref":"fund-1","external_treasury_ref":"treasury-1",` +
				`"authorized_cents":100,"authorized_cents":101,"reason":"test"}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "duplicate key",
		},
		{
			name: "subsidy escaped duplicate reference",
			body: `{"fund_ref":"fund-1","authorization_ref":"auth-1",` +
				`"\u0061uthorization_ref":"auth-2","reason":"test"}`,
			subsidize:  true,
			wantStatus: http.StatusBadRequest,
			wantError:  "duplicate key",
		},
		{
			name:       "fund oversized",
			body:       `{"fund_ref":"` + strings.Repeat("x", moneyAuthorityBodyLimit) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "exceeds",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			if tc.subsidize {
				req.SetPathValue("id", uuid.NewString())
			}
			rr := httptest.NewRecorder()
			if tc.subsidize {
				server.handleAdminSubsidizePayout(rr, req)
			} else {
				server.handleAdminCreateSubsidyFund(rr, req)
			}
			if rr.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", rr.Code, rr.Body.String(), tc.wantStatus)
			}
			if !strings.Contains(rr.Body.String(), tc.wantError) {
				t.Fatalf("body=%s, want error containing %q", rr.Body.String(), tc.wantError)
			}
		})
	}
}
