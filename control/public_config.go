package main

import (
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strings"
)

type publicContactConfig struct {
	Configured    bool     `json:"configured"`
	SupportEmail  string   `json:"support_email,omitempty"`
	SecurityEmail string   `json:"security_email,omitempty"`
	StatusURL     string   `json:"status_url,omitempty"`
	TermsURL      string   `json:"terms_url,omitempty"`
	PrivacyURL    string   `json:"privacy_url,omitempty"`
	Missing       []string `json:"missing,omitempty"`
}

type publicBrowserConfig struct {
	SchemaVersion            int                 `json:"schema_version"`
	SettlementCurrency       string              `json:"settlement_currency"`
	StripePublishableKey     string              `json:"stripe_publishable_key,omitempty"`
	StripePaymentFormEnabled bool                `json:"stripe_payment_form_enabled"`
	Contacts                 publicContactConfig `json:"contacts"`
}

func validPublicEmail(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(strings.ToLower(value), "example.invalid") || strings.Contains(value, "[") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value && strings.Contains(value, "@")
}

func validPublicHTTPSURL(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(strings.ToLower(value), "example.invalid") || strings.Contains(value, "[") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func currentPublicContactConfig() publicContactConfig {
	values := map[string]string{
		"support_email":  strings.TrimSpace(getenvTrim("MERC_SUPPORT_EMAIL")),
		"security_email": strings.TrimSpace(getenvTrim("MERC_SECURITY_EMAIL")),
		"status_url":     strings.TrimSpace(getenvTrim("MERC_STATUS_URL")),
		"terms_url":      strings.TrimSpace(getenvTrim("MERC_TERMS_URL")),
		"privacy_url":    strings.TrimSpace(getenvTrim("MERC_PRIVACY_URL")),
	}
	missing := make([]string, 0, len(values))
	for name, value := range values {
		valid := validPublicHTTPSURL(value)
		if strings.HasSuffix(name, "_email") {
			valid = validPublicEmail(value)
		}
		if !valid {
			missing = append(missing, name)
			values[name] = ""
		}
	}
	sort.Strings(missing)
	return publicContactConfig{
		Configured: len(missing) == 0, Missing: missing,
		SupportEmail: values["support_email"], SecurityEmail: values["security_email"],
		StatusURL: values["status_url"], TermsURL: values["terms_url"], PrivacyURL: values["privacy_url"],
	}
}

func currentPublicBrowserConfig() publicBrowserConfig {
	publishable := strings.TrimSpace(getenvTrim("STRIPE_PUBLISHABLE_KEY"))
	if stripeCredentialClass(publishable) != "publishable_test" {
		publishable = ""
	}
	return publicBrowserConfig{
		SchemaVersion: 1, SettlementCurrency: SettlementCurrencyCode(),
		StripePublishableKey: publishable, StripePaymentFormEnabled: publishable != "",
		Contacts: currentPublicContactConfig(),
	}
}

func (s *Server) handlePublicConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, currentPublicBrowserConfig())
}
