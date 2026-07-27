package main

import (
	"os"
	"strings"
	"testing"
)

func TestShippedCatalogueSatisfiesOnboardingPolicy(t *testing.T) {
	if err := validateModelOnboarding(runtimeAuthority); err != nil {
		t.Fatalf("shipped catalogue violates its own onboarding policy: %v", err)
	}
	// A policy that passes because there is nothing to check is not a policy.
	if len(runtimeAuthority.Models) == 0 {
		t.Fatal("no models in the authority document")
	}
	for _, m := range runtimeAuthority.Models {
		if m.License == "" || m.LicenseURL == "" {
			t.Fatalf("model %q reached the shipped catalogue without licence terms", m.ID)
		}
	}
}

func TestOnboardingPolicyRefusesUnsellableModels(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*runtimeAuthorityDocument)
		want   string
	}{
		{"no licence", func(d *runtimeAuthorityDocument) { d.Models[0].License = "" }, "declares no licence"},
		{"licence off the allowlist", func(d *runtimeAuthorityDocument) {
			d.Models[0].License = "CC-BY-NC-4.0"
		}, "not on the resale allowlist"},
		{"licence with no url", func(d *runtimeAuthorityDocument) { d.Models[0].LicenseURL = "" }, "no licence_url"},
		{"non-commercial", func(d *runtimeAuthorityDocument) { d.Models[0].CommercialUse = false }, "may not sell"},
		{"remote code", func(d *runtimeAuthorityDocument) { d.Models[0].RemoteCode = true }, "remote_code=true"},
		{"attribution without text", func(d *runtimeAuthorityDocument) {
			d.Models[0].AttributionRequired = true
			d.Models[0].AttributionText = ""
		}, "declares no attribution_text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := runtimeAuthority
			// Copy the slice so one case cannot leak into the next.
			doc.Models = append([]authorityModel(nil), runtimeAuthority.Models...)
			tc.break_(&doc)

			err := validateModelOnboarding(doc)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s rejected for the wrong reason: %v", tc.name, err)
			}
		})
	}
}

// The attribution obligation is met by the NOTICE users actually receive, not
// by a boolean in a config file claiming it was. NOTICE is above the module
// root so the runtime cannot embed it; this is where that half is enforced.
func TestCatalogueAttributionAppearsInNotice(t *testing.T) {
	notice, err := os.ReadFile("../NOTICE")
	if err != nil {
		t.Fatalf("reading NOTICE: %v", err)
	}
	checked := 0
	for _, m := range runtimeAuthority.Models {
		if !m.AttributionRequired {
			continue
		}
		checked++
		if !strings.Contains(string(notice), m.AttributionText) {
			t.Fatalf("model %q requires the attribution %q and NOTICE does not contain it; "+
				"merc is serving it to paying buyers without meeting the licence term",
				m.ID, m.AttributionText)
		}
	}
	if checked == 0 {
		t.Skip("no catalogue model currently requires attribution")
	}
}
