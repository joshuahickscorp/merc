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
	}
	out, err := stripeForm(ctx, "accounts", url.Values{
		"type":                               {"express"},
		"capabilities[transfers][requested]": {"true"},
		"metadata[supplier_id]":              {supplierID.String()},
	}, "")
	if err != nil {
		return "", err
	}
	acct, _ := out["id"].(string)
	if acct == "" {
		return "", fmt.Errorf("stripe account: no id in response")
	}
	if err := store.SetSupplierStripeAcct(ctx, supplierID, acct); err != nil {
		return "", err
	}
	return acct, nil
}

func onboardingLink(ctx context.Context, acct string) (string, error) {
	ret, refresh := strings.TrimSpace(os.Getenv("CX_CONNECT_RETURN_URL")),
		strings.TrimSpace(os.Getenv("CX_CONNECT_REFRESH_URL"))
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
	link, _ := out["url"].(string)
	if link == "" {
		return "", fmt.Errorf("stripe account_link: no url in response")
	}
	return link, nil
}

func validateConnectURLPair(returnURL, refreshURL, siteHost string) error {
	siteHost = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(siteHost), "."))
	if returnURL == "" || refreshURL == "" {
		return errors.New("CX_CONNECT_RETURN_URL and CX_CONNECT_REFRESH_URL are required")
	}
	if siteHost == "" {
		return errors.New("SITE_HOST is required to validate Stripe Connect return origins")
	}
	for name, raw := range map[string]string{
		"CX_CONNECT_RETURN_URL": returnURL, "CX_CONNECT_REFRESH_URL": refreshURL,
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

func validateLiveConnectURLConfig(cxEnv, stripeSecret, returnURL, refreshURL, siteHost string) error {
	liveStripe := strings.HasPrefix(stripeSecret, "sk_live_")
	production := strings.EqualFold(cxEnv, "production") || strings.EqualFold(cxEnv, "prod")
	if !production && !liveStripe {
		return nil
	}
	if err := validateConnectURLPair(returnURL, refreshURL, siteHost); err != nil {
		return fmt.Errorf("live Stripe Connect configuration invalid: %w; refusing to start", err)
	}
	return nil
}

func (s *Server) handleWorkerConnectStatus(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	acct, _ := s.store.SupplierStripeAcct(r.Context(), auth.SupplierID)
	if stripeKey() == "" || acct == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": stripeKey() != "", "connected": false, "payouts_enabled": false,
			"credential_id": auth.CredentialID, "enrollment_device_bound": auth.EnrollmentDeviceBound,
			"device_fingerprint": auth.DeviceFingerprint, "credential_version": auth.CredentialVersion,
		})
		return
	}
	out, err := stripeGet(r.Context(), "accounts/"+acct)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	pe, _ := out["payouts_enabled"].(bool)
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "connected": true, "payouts_enabled": pe,
		"credential_id": auth.CredentialID, "enrollment_device_bound": auth.EnrollmentDeviceBound,
		"device_fingerprint": auth.DeviceFingerprint, "credential_version": auth.CredentialVersion,
	})
}
