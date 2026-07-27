package main

import (
	"fmt"
	"strings"
)

// Onboarding policy for catalogue models.
//
// The authority file already pins each model to a repo, a revision and a
// per-artifact sha256, so merc knows exactly which bytes it serves. What it did
// not record was whether merc is allowed to sell inference from those bytes,
// and whether serving them runs anybody else's code on a supplier's machine.
// Both were handled once, in prose, in NOTICE -- which means the next model
// added gets no review at all.
//
// These are the gates that prose cannot enforce. They run at process start and
// panic, because a model that fails one must not reach a catalogue that
// suppliers serve and buyers are charged for.

// Licences merc may resell inference under.
//
// This is an allowlist, not a denylist: an unrecognised licence is refused
// rather than assumed permissive. Getting that backwards means the first
// non-commercial model anyone adds ships silently.
var resaleAllowedLicenses = map[string]string{
	"Apache-2.0":          "permissive, commercial use unrestricted",
	"MIT":                 "permissive, commercial use unrestricted",
	"BSD-3-Clause":        "permissive, commercial use unrestricted",
	"Llama-3.2-Community": "commercial use permitted below 700M MAU, with attribution and an acceptable use policy",
}

func validateModelOnboarding(authority runtimeAuthorityDocument) error {
	for _, m := range authority.Models {
		if strings.TrimSpace(m.License) == "" {
			return fmt.Errorf("model %q declares no licence; merc cannot sell inference "+
				"from weights whose terms it has not recorded", m.ID)
		}
		terms, allowed := resaleAllowedLicenses[m.License]
		if !allowed {
			return fmt.Errorf("model %q declares licence %q, which is not on the resale "+
				"allowlist; add it there only after someone has read the licence and "+
				"confirmed merc may charge for inference under it", m.ID, m.License)
		}
		if strings.TrimSpace(m.LicenseURL) == "" {
			return fmt.Errorf("model %q declares licence %q with no licence_url; the terms "+
				"must be locatable by whoever audits this next", m.ID, m.License)
		}

		// merc charges buyers for the output. A model that forbids commercial
		// use cannot be part of that, whatever its other terms allow.
		if !m.CommercialUse {
			return fmt.Errorf("model %q is marked commercial_use=false but the catalogue "+
				"is priced; merc would be charging for inference it may not sell (%s)",
				m.ID, terms)
		}

		// Suppliers are third parties running merc's runtime on their own
		// hardware. trust_remote_code executes Python fetched from the model
		// repo on that hardware: a repo compromise, or a malicious model,
		// becomes code execution on every machine that serves it. There is no
		// catalogue entry worth that, so this is refused outright rather than
		// made configurable.
		if m.RemoteCode {
			return fmt.Errorf("model %q sets remote_code=true; that runs repo-supplied "+
				"code on supplier hardware and is refused unconditionally", m.ID)
		}

		// Whether the attribution is actually PRESENT is checked against the
		// shipped NOTICE by TestCatalogueAttributionAppearsInNotice. It cannot
		// be checked here: NOTICE lives above the module root and go:embed
		// cannot reach it, and reading it from disk at runtime would depend on
		// the working directory of a container. So the runtime gate is that the
		// text is declared, and the CI gate is that NOTICE contains it.
		if m.AttributionRequired && strings.TrimSpace(m.AttributionText) == "" {
			return fmt.Errorf("model %q requires attribution (%s) but declares no "+
				"attribution_text", m.ID, terms)
		}
	}
	return nil
}
