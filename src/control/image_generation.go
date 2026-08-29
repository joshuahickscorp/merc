package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// Governance for image generation.
//
// Text and image generation are not the same product risk, and merc cannot
// govern them with the same rules.
//
// The licence difference is structural. Apache-2.0 and MIT constrain what merc
// may do with the weights and stop there. The licences the open image models
// actually ship under -- CreativeML OpenRAIL-M and its successors -- permit
// commercial use but attach USE RESTRICTIONS that the licensor requires the
// licensee to pass on to every downstream user. merc is not the end user here;
// it resells generation to buyers. So accepting an OpenRAIL model obliges merc
// to impose those restrictions on its own buyers, and a lane that takes the
// weights without enforcing the restrictions is in breach however careful its
// other gates are.
//
// The content difference is that a text refusal costs a buyer a completion,
// while an image lane that generates the wrong thing produces an artifact that
// exists, is stored in merc's object storage, and is served from merc's domain.
//
// This file is the policy. It is deliberately separate from any runtime: it
// must hold whichever engine ends up behind it.

const (
	maxImagesPerRequest = 4
	maxImagePromptRunes = 4000
)

// Sizes merc will generate. An allowlist rather than a range: an arbitrary
// dimension is a cheap way to make a supplier allocate far more memory than the
// quoted price covers, and every size has to have a price.
var allowedImageSizes = map[string]bool{
	"256x256": true, "512x512": true, "768x768": true, "1024x1024": true,
	"1024x1792": true, "1792x1024": true,
}

var (
	errImagePolicyRefusal = errors.New("image request refused by merc's generation policy")
	errImageRequestShape  = errors.New("invalid image request")
)

// imagePolicyRefusal names WHICH rule refused, so a buyer can tell a policy
// refusal from a capacity failure and merc can report refusals by category
// without retaining the prompt.
type imagePolicyRefusal struct {
	Rule   string
	Reason string
}

func (e imagePolicyRefusal) Error() string {
	return fmt.Sprintf("%v: %s (%s)", errImagePolicyRefusal, e.Reason, e.Rule)
}

func (e imagePolicyRefusal) Unwrap() error { return errImagePolicyRefusal }

// The categories below are the ones an image marketplace must refuse regardless
// of what any model would produce, because the harm is in the artifact
// existing, not in the model's willingness to make it.
//
// These patterns are a FLOOR, not the whole control. They catch unambiguous
// requests; they will not catch a determined user, and nothing here should be
// read as claiming otherwise. The runtime-side safety checker and the
// post-generation review path are the rest of it, and this lane does not claim
// to be governed until those exist too.
var imageRefusalRules = []struct {
	Rule    string
	Pattern *regexp.Regexp
	Reason  string
}{
	{
		Rule: "csam",
		// Sexual content involving minors. No exception, no override, no
		// operator flag: this is refused before any other consideration.
		Pattern: regexp.MustCompile(`(?i)\b(child|children|kid|kids|minor|minors|toddler|infant|preteen|underage|teen|teenage|loli|shota)\b[^.]{0,60}\b(nude|naked|nsfw|sexual|sexy|erotic|porn|explicit|lingerie|undressed)\b|` +
			`\b(nude|naked|nsfw|sexual|sexy|erotic|porn|explicit|lingerie|undressed)\b[^.]{0,60}\b(child|children|kid|kids|minor|minors|toddler|infant|preteen|underage|teen|teenage|loli|shota)\b`),
		Reason: "sexual content involving minors",
	},
	{
		Rule: "non_consensual_intimate",
		Pattern: regexp.MustCompile(`(?i)\b(nude|naked|undress|undressed|topless|deepfake\w*)\b[^.]{0,60}\b(without (her|his|their) consent|non-?consensual|revenge|unaware|hidden camera|upskirt)\b|` +
			`\b(non-?consensual|revenge porn|upskirt|creepshot)\b`),
		Reason: "non-consensual intimate imagery",
	},
	{
		Rule: "real_person_likeness",
		// Photorealistic depictions of an identifiable real person. Refused
		// because merc cannot obtain that person's consent and the artifact
		// would be served from merc's own domain.
		Pattern: regexp.MustCompile(`(?i)\b(photorealistic|photo-?real|realistic photo|deepfake|face ?swap|lookalike)\b[^.]{0,80}\b(of|depicting)\b[^.]{0,40}\b(president|senator|prime minister|celebrity|actor|actress|politician)\b`),
		Reason:  "photorealistic depiction of an identifiable real person",
	},
	{
		Rule:    "forged_document",
		Pattern: regexp.MustCompile(`(?i)\b(fake|forged|counterfeit|replica)\b[^.]{0,40}\b(passport|driver'?s? licen[cs]e|id card|identity card|banknote|currency|certificate|diploma|visa)\b`),
		Reason:  "forged identity or financial document",
	},
}

// imageGenerationRequest is merc's canonical shape. It is deliberately NOT the
// OpenAI request struct: the surface accepts OpenAI's field names, but what the
// policy and the contract see is this.
type imageGenerationRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	N      int    `json:"n"`
	Size   string `json:"size"`
	// ResponseFormat is accepted for compatibility. merc always stores the
	// artifact and returns a presigned URL: a base64 image inline would bypass
	// object storage and therefore bypass retention and erasure.
	ResponseFormat string `json:"response_format"`
	User           string `json:"user"`
}

// validateImageRequest normalises and bounds the request. Defaults match
// OpenAI's so an existing client does not have to change to get a valid one.
func validateImageRequest(req *imageGenerationRequest) error {
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return fmt.Errorf("%w: prompt is required", errImageRequestShape)
	}
	if n := len([]rune(req.Prompt)); n > maxImagePromptRunes {
		return fmt.Errorf("%w: prompt is %d characters, the limit is %d",
			errImageRequestShape, n, maxImagePromptRunes)
	}
	if strings.TrimSpace(req.Model) == "" {
		return fmt.Errorf("%w: model is required", errImageRequestShape)
	}
	if req.N == 0 {
		req.N = 1
	}
	if req.N < 1 || req.N > maxImagesPerRequest {
		return fmt.Errorf("%w: n must be between 1 and %d, got %d",
			errImageRequestShape, maxImagesPerRequest, req.N)
	}
	if req.Size == "" {
		req.Size = "1024x1024"
	}
	if !allowedImageSizes[req.Size] {
		return fmt.Errorf("%w: size %q is not offered; merc generates %s",
			errImageRequestShape, req.Size, strings.Join(sortedImageSizes(), ", "))
	}
	switch req.ResponseFormat {
	case "", "url":
		req.ResponseFormat = "url"
	case "b64_json":
		// Refused rather than silently downgraded. An inline image never enters
		// object storage, so it would have no retention, no erasure path and no
		// artifact to attach to a dispute -- the buyer would receive something
		// merc cannot later account for.
		return fmt.Errorf("%w: response_format b64_json is not offered; merc stores every "+
			"generated image so it has a retention, erasure and dispute-evidence path, "+
			"and returns a presigned url", errImageRequestShape)
	default:
		return fmt.Errorf("%w: response_format %q is not recognised",
			errImageRequestShape, req.ResponseFormat)
	}
	return nil
}

func sortedImageSizes() []string {
	out := make([]string, 0, len(allowedImageSizes))
	for s := range allowedImageSizes {
		out = append(out, s)
	}
	// Deterministic order so the error message is stable across processes.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// applyImageGenerationPolicy refuses prompts merc will not generate.
//
// Returns an imagePolicyRefusal naming the rule. The prompt itself is never
// returned or logged by this function: a refusal record that quotes the prompt
// stores the exact content the refusal existed to avoid storing.
func applyImageGenerationPolicy(prompt string) error {
	// Two normalisations, because one is not enough and they defeat opposite
	// evasions. Replacing separators with a space recovers "nude_child" but
	// turns "n-u-d-e" into "n u d e", which matches nothing. Deleting them
	// recovers "n-u-d-e" but welds "nude_child" into one token that no word
	// boundary matches. A rule matching EITHER form is refused.
	forms := []string{
		normaliseForPolicy(prompt, " "),
		normaliseForPolicy(prompt, ""),
	}
	for _, rule := range imageRefusalRules {
		for _, form := range forms {
			if rule.Pattern.MatchString(form) {
				return imagePolicyRefusal{Rule: rule.Rule, Reason: rule.Reason}
			}
		}
	}
	return nil
}

// normaliseForPolicy collapses the cheapest evasions: separator characters
// inserted between letters, and repeated whitespace. It does not attempt
// leetspeak or homoglyph normalisation, and the rules above do not claim to
// survive a determined attempt.
var policySeparators = regexp.MustCompile(`[\x00-\x1f_\-.*+~^|/\\]+`)

func normaliseForPolicy(prompt, separatorReplacement string) string {
	s := strings.ToLower(prompt)
	s = policySeparators.ReplaceAllString(s, separatorReplacement)
	return strings.Join(strings.Fields(s), " ")
}

// imageLicenceObligations describes what a model's licence requires merc to
// impose on ITS buyers. Text licences generally do not do this; the open image
// licences do, which is why it lives here rather than in the shared onboarding
// policy.
type imageLicenceObligations struct {
	// PassThroughUseRestrictions is true when the licence requires merc to bind
	// downstream users to the same restrictions. If merc cannot show it does
	// that, it may not serve the model.
	PassThroughUseRestrictions bool
	// AttributionText must appear in the shipped NOTICE.
	AttributionText string
}

var imageLicenceTerms = map[string]imageLicenceObligations{
	"Apache-2.0": {PassThroughUseRestrictions: false},
	"MIT":        {PassThroughUseRestrictions: false},
	"CreativeML-OpenRAIL-M": {
		PassThroughUseRestrictions: true,
		AttributionText:            "CreativeML Open RAIL-M",
	},
	"OpenRAIL++-M": {
		PassThroughUseRestrictions: true,
		AttributionText:            "CreativeML Open RAIL++-M",
	},
}

// validateImageModelLicence checks merc can actually resell generation from a
// model, and that where the licence demands restrictions be passed downstream,
// merc has a policy that does so.
//
// hasEnforcedUsePolicy is the answer to "does merc bind its buyers to these
// restrictions?" -- it is satisfied by applyImageGenerationPolicy existing and
// running on every request, which is asserted by the tests rather than by a
// flag someone can set.
func validateImageModelLicence(modelID, licence string, hasEnforcedUsePolicy bool) error {
	terms, known := imageLicenceTerms[licence]
	if !known {
		return fmt.Errorf("image model %q declares licence %q, which is not on the image "+
			"resale allowlist; open image licences attach use restrictions that text "+
			"licences do not, so each one has to be read before it is added", modelID, licence)
	}
	if terms.PassThroughUseRestrictions && !hasEnforcedUsePolicy {
		return fmt.Errorf("image model %q is licensed under %q, which requires merc to bind "+
			"downstream users to the same use restrictions; merc resells generation, so it "+
			"may not serve this model without a use policy of its own", modelID, licence)
	}
	return nil
}

// handleImageGenerations is merc's OpenAI-compatible image surface.
//
// It runs shape validation and then policy BEFORE anything reaches a contract,
// a scheduler or a supplier. A refused request must cost nobody: no contract
// row, no capacity held, no supplier woken, and no charge. Refusing after
// authorization would mean merc either eats the cost of its own policy or bills
// a buyer for work it declined to do.
func (s *Server) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRealtimeRequestBytes+1))
	if err != nil || len(raw) > maxRealtimeRequestBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge,
			"request body exceeds the image limit", "invalid_request_error", "request_too_large")
		return
	}
	var req imageGenerationRequest
	if err := decodeStrictJSONObject(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(),
			"invalid_request_error", "invalid_request")
		return
	}
	if err := validateImageRequest(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(),
			"invalid_request_error", "invalid_request")
		return
	}
	if err := applyImageGenerationPolicy(req.Prompt); err != nil {
		var refusal imagePolicyRefusal
		_ = errors.As(err, &refusal)
		metrics.imageRequestsRefused.Add(1)
		// The rule is named; the prompt is not repeated back. Nothing about a
		// refused request is written to storage or to the log.
		log.Printf("images: refused by policy rule=%s", refusal.Rule)
		writeOpenAIError(w, http.StatusBadRequest,
			"merc does not generate this: "+refusal.Reason,
			"invalid_request_error", "content_policy_violation")
		return
	}

	// Policy passed. Everything past this point needs a real image runtime
	// behind it -- a contract, a supplier, generation, storage, verification and
	// settlement -- and merc has none yet. Returning 503 is the honest answer:
	// the request was acceptable and merc cannot currently serve it. Answering
	// anything else would mean inventing a result or charging for nothing.
	writeOpenAIError(w, http.StatusServiceUnavailable,
		"image generation is not yet available: merc has no image runtime in service",
		"server_error", "no_capacity")
}
