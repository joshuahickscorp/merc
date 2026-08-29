package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Where the immutable release artifact puts the files the control plane must have.
//
// The production image could not start. Dockerfile.control copied four HTML files
// and the binary; it did not copy src/pricing/board.json. The final stage is
// distroless with WORKDIR /, so every relative candidate missed, the source-file
// fallback resolves to a build-time path that does not exist in the image, no
// MERC_PRICE_BOARD was set in production compose, and startup reached
// log.Fatalf("catalogue price authority unavailable").
//
// The fix is not another fallback path. A fallback is what let a missing release
// artifact look like a working service on a developer machine and a dead one in
// production: the more places a file can be found, the less a successful start
// tells you about what was actually loaded. So there is now ONE absolute path per
// artifact, the image puts the file there, production configuration names it, and
// the digest of what was loaded is reported on /version — so a running deployment
// can be asked which board it is pricing from rather than assumed.
const (
	// releaseRoot is the artifact root inside the image. Absolute, because the
	// working directory of a distroless container is not a fact anyone should
	// have to know.
	releaseRoot = "/etc/merc"
	// releasePriceBoardPath is the catalogue price authority.
	releasePriceBoardPath = releaseRoot + "/pricing/board.json"
	// releaseWebRoot is the buyer/supplier/pricing surface.
	releaseWebRoot = "/web"
)

const (
	priceBoardPathEnv   = "MERC_PRICE_BOARD"
	priceBoardDigestEnv = "MERC_PRICE_BOARD_SHA256"
)

// resolvedPriceBoardPath reports the path the board will be read from and why.
//
// The "why" is returned rather than logged because it is the difference between a
// deployment that is pricing from its release artifact and one that is pricing
// from whatever happened to be in the working directory.
type resolvedPriceBoard struct {
	Path string
	// Source is one of: env, release, repo. A production deployment must never
	// be on repo.
	Source string
	// ExpectedDigest is the operator's declared digest, empty when none was given.
	ExpectedDigest string
}

// resolvePriceBoard applies the release rules.
//
// An explicit MERC_PRICE_BOARD wins, because an operator pointing at a specific
// file is making a decision and the software should not second-guess it — but in
// production that decision has to come with the digest it expects, or "override
// the price authority" is a single environment variable away from silently
// repricing the catalogue.
//
// Otherwise the release path. Otherwise, and only outside production, the
// repository layout, so `go test` and `go run` in a checkout keep working.
func resolvePriceBoard(env string) (resolvedPriceBoard, error) {
	production := isProductionEnv(env) || isLiveStripeCredential(stripeKey())
	expected := strings.ToLower(strings.TrimSpace(os.Getenv(priceBoardDigestEnv)))

	if configured := strings.TrimSpace(os.Getenv(priceBoardPathEnv)); configured != "" {
		// Naming the release path is CONFIGURATION, not an override. Production
		// compose is supposed to name it explicitly — that is the whole point of
		// having one documented path — and requiring a digest to state the
		// obvious would mean the correct configuration could not boot. The first
		// cut got this wrong and the image-boot gate caught it immediately.
		//
		// Pointing anywhere ELSE is an override, and in production it has to say
		// which bytes it expects: repricing the entire catalogue must not be one
		// environment variable away.
		source := "env"
		if filepath.Clean(configured) == releasePriceBoardPath {
			source = "release"
		} else if production && expected == "" {
			return resolvedPriceBoard{}, fmt.Errorf(
				"%s=%s points away from the release artifact at %s; %s must name the "+
					"digest that override is expected to have",
				priceBoardPathEnv, configured, releasePriceBoardPath, priceBoardDigestEnv)
		}
		return resolvedPriceBoard{Path: configured, Source: source, ExpectedDigest: expected}, nil
	}

	if info, err := os.Stat(releasePriceBoardPath); err == nil && !info.IsDir() {
		return resolvedPriceBoard{
			Path: releasePriceBoardPath, Source: "release", ExpectedDigest: expected,
		}, nil
	}

	if production {
		return resolvedPriceBoard{}, fmt.Errorf(
			"no price board at %s and %s is unset. The release image is supposed to "+
				"ship one there; a production deployment must not fall back to a "+
				"repository-relative path, because then a successful start says nothing "+
				"about which prices are live",
			releasePriceBoardPath, priceBoardPathEnv)
	}

	// Development only. Kept explicitly separate from the two paths above so that
	// "it worked on my machine" and "it works in the image" cannot be the same
	// code path again.
	candidates := []string{"../../src/pricing/board.json"}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates,
			filepath.Join(filepath.Dir(file), "..", "pricing", "board.json"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return resolvedPriceBoard{
				Path: candidate, Source: "repo", ExpectedDigest: expected,
			}, nil
		}
	}
	return resolvedPriceBoard{}, fmt.Errorf(
		"no price board found: %s is unset, %s does not exist, and no repository "+
			"copy resolved from %s", priceBoardPathEnv, releasePriceBoardPath, candidates)
}

// verifyPriceBoardDigest refuses bytes the operator did not expect.
//
// Applied whenever a digest was declared, in every environment: an operator who
// says which bytes they expect has made a checkable statement, and checking it in
// development is how the statement gets tested before it matters.
func verifyPriceBoardDigest(raw []byte, expected string) (string, error) {
	sum := sha256.Sum256(raw)
	actual := hex.EncodeToString(sum[:])
	if expected != "" && actual != expected {
		return actual, fmt.Errorf(
			"price board digest is %s, %s declares %s; refusing to price a catalogue "+
				"from bytes the operator did not approve", actual, priceBoardDigestEnv, expected)
	}
	return actual, nil
}

// releaseWebAsset resolves a shipped web file, preferring the release root.
//
// Same reasoning as the board, milder consequence: a missing HTML file is a 404
// rather than a dead process, which is exactly why it went unnoticed that
// Dockerfile.control shipped three of the seven pages the router serves.
func releaseWebAsset(name string) string {
	for _, root := range []string{releaseWebRoot, "../../clients/web"} {
		candidate := filepath.Join(root, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return filepath.Join(releaseWebRoot, name)
}
