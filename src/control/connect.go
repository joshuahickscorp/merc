package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
)

func ensureConnectAccount(ctx context.Context, store *Store, supplierID uuid.UUID) (string, error) {
	if acct, err := store.SupplierStripeAcct(ctx, supplierID); err == nil && acct != "" {
		return acct, nil
	} else if err != nil && !errors.Is(err, errNotFound) {
		return "", err
	}
	out, err := stripeForm(ctx, "accounts", url.Values{
		"type":                               {"express"},
		"capabilities[transfers][requested]": {"true"},
		"metadata[supplier_id]":              {supplierID.String()},
	}, stripeConnectAccountIdempotencyKey(supplierID))
	if err != nil {
		return "", err
	}
	acct, _ := out["id"].(string)
	if !validStripeObjectID(acct, "acct_") {
		return "", fmt.Errorf("stripe account: provider returned an invalid connected-account id")
	}
	if err := store.SetSupplierStripeAcct(ctx, supplierID, acct); err != nil {
		// Named enrolment refusal (duplicate payout instrument) must surface
		// as-is so the API layer can map it without string matching.
		return "", err
	}
	return acct, nil
}

func stripeConnectAccountIdempotencyKey(supplierID uuid.UUID) string {
	return "cx-connect-account-" + supplierID.String()
}

func onboardingLink(ctx context.Context, acct string) (string, error) {
	acct = strings.TrimSpace(acct)
	if !validStripeObjectID(acct, "acct_") {
		return "", errors.New("stripe account_link: connected account id is invalid")
	}
	ret, refresh := strings.TrimSpace(os.Getenv("MERC_CONNECT_RETURN_URL")),
		strings.TrimSpace(os.Getenv("MERC_CONNECT_REFRESH_URL"))
	if err := validateConnectURLPair(ret, refresh, os.Getenv("SITE_HOST")); err != nil {
		return "", err
	}
	out, err := stripeForm(ctx, "account_links", url.Values{
		"account":     {acct},
		"refresh_url": {refresh},
		"return_url":  {ret},
		"type":        {"account_onboarding"},
	}, "")
	if err != nil {
		return "", err
	}
	link, err := parseStripeAccountLinkResponse(out)
	if err != nil {
		return "", err
	}
	return link, nil
}

func parseStripeAccountLinkResponse(out map[string]any) (string, error) {
	objectType, ok := out["object"].(string)
	if !ok || strings.TrimSpace(objectType) != "account_link" {
		return "", errors.New("stripe account_link: provider returned the wrong object type")
	}
	link, _ := out["url"].(string)
	link = strings.TrimSpace(link)
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Path == "" {
		return "", errors.New("stripe account_link: provider returned an invalid HTTPS URL")
	}
	if strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")) != "connect.stripe.com" ||
		(u.Port() != "" && u.Port() != "443") {
		return "", errors.New("stripe account_link: provider URL is not hosted by connect.stripe.com")
	}
	return link, nil
}

func validateConnectURLPair(returnURL, refreshURL, siteHost string) error {
	siteHost = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(siteHost), "."))
	if returnURL == "" || refreshURL == "" {
		return errors.New("MERC_CONNECT_RETURN_URL and MERC_CONNECT_REFRESH_URL are required")
	}
	if siteHost == "" {
		return errors.New("SITE_HOST is required to validate Stripe Connect return origins")
	}
	for name, raw := range map[string]string{
		"MERC_CONNECT_RETURN_URL": returnURL, "MERC_CONNECT_REFRESH_URL": refreshURL,
	} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("%s must be an absolute HTTPS URL without credentials or fragment", name)
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host != siteHost || (u.Port() != "" && u.Port() != "443") {
			return fmt.Errorf("%s must use the SITE_HOST HTTPS origin", name)
		}
	}
	return nil
}

func parseStripeConnectAccountStatus(out map[string]any, expectedAcct string) (bool, error) {
	expectedAcct = strings.TrimSpace(expectedAcct)
	if !validStripeObjectID(expectedAcct, "acct_") {
		return false, errors.New("stripe connected account: expected account id is invalid")
	}
	objectType, ok := out["object"].(string)
	if !ok || strings.TrimSpace(objectType) != "account" {
		return false, errors.New("stripe connected account: provider returned the wrong object type")
	}
	returnedAcct, _ := out["id"].(string)
	returnedAcct = strings.TrimSpace(returnedAcct)
	if !validStripeObjectID(returnedAcct, "acct_") || returnedAcct != expectedAcct {
		return false, errors.New("stripe connected account: provider returned the wrong account")
	}
	payoutsEnabled, ok := out["payouts_enabled"].(bool)
	if !ok {
		return false, errors.New("stripe connected account: provider omitted payouts_enabled")
	}
	return payoutsEnabled, nil
}

func (s *Server) handleWorkerConnectStatus(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	acct, err := s.store.SupplierStripeAcct(r.Context(), auth.SupplierID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading Stripe connected account")
		return
	}
	if stripeKey() == "" || acct == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": stripeKey() != "", "connected": false, "payouts_enabled": false,
			"credential_id": auth.CredentialID, "enrollment_device_bound": auth.EnrollmentDeviceBound,
			"device_fingerprint": auth.DeviceFingerprint, "credential_version": auth.CredentialVersion,
		})
		return
	}
	out, err := stripeGet(r.Context(), "accounts/"+url.PathEscape(acct))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "reading Stripe connected account status")
		return
	}
	pe, err := parseStripeConnectAccountStatus(out, acct)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "Stripe connected account status is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "connected": true, "payouts_enabled": pe,
		"credential_id": auth.CredentialID, "enrollment_device_bound": auth.EnrollmentDeviceBound,
		"device_fingerprint": auth.DeviceFingerprint, "credential_version": auth.CredentialVersion,
	})
}
