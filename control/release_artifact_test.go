package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The image must ship everything the router serves.
//
// Dockerfile.control copied the binary and three HTML pages. The router registers
// seven web routes and reads the catalogue price authority from a file, and none
// of those files were in the image — so the production container reached
// log.Fatalf("catalogue price authority unavailable") and could not start at all,
// while the live host stayed up only because it had been assembled by hand.
//
// This reads the Dockerfile rather than building it, because building requires a
// daemon that CI may not have, and the failure being guarded against is an
// omission in a COPY list — which is exactly what a text check catches. The
// image-boot proof is a separate gate (scripts/test-release-image-boots.sh).
func TestReleaseImageShipsEveryFileTheRouterServes(t *testing.T) {
	dockerfile, err := os.ReadFile("../Dockerfile.control")
	if err != nil {
		t.Fatal(err)
	}
	image := string(dockerfile)

	// The price board, at the one absolute path the resolver looks for.
	if !strings.Contains(image, "COPY pricing/board.json "+releasePriceBoardPath) {
		t.Errorf("the image does not ship the price board to %s. Without it the "+
			"container cannot start: BuildCataloguePriceSchedule fails and main "+
			"log.Fatalf's before it serves anything.", releasePriceBoardPath)
	}

	// Every page the router registers, whether by whole-directory copy or by name.
	api, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	shipsWholeTree := strings.Contains(image, "COPY web/ /web/")
	for _, page := range []string{
		"index.html", "buyer.html", "admin.html", "prices.html", "supplier.html",
	} {
		if _, err := os.Stat(filepath.Join("..", "web", page)); err != nil {
			continue // not a page this repository has
		}
		if shipsWholeTree || strings.Contains(image, "web/"+page) {
			continue
		}
		t.Errorf("web/%s exists and the image does not ship it", page)
	}
	// The two routes that serve from directories rather than named files.
	for route, dir := range map[string]string{
		"GET /assets/site/{path...}":    "web/assets/site",
		"GET /.well-known/security.txt": "web/.well-known",
	} {
		if !strings.Contains(string(api), route) {
			continue
		}
		if _, err := os.Stat(filepath.Join("..", dir)); err != nil {
			t.Errorf("the router serves %q but %s does not exist", route, dir)
			continue
		}
		if !shipsWholeTree && !strings.Contains(image, dir) {
			t.Errorf("the router serves %q from %s and the image does not ship that "+
				"directory", route, dir)
		}
	}
}

// Production configuration must NAME the price board rather than discover it.
func TestProductionComposeNamesThePriceBoard(t *testing.T) {
	compose, err := os.ReadFile("../docker-compose.prod.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), priceBoardPathEnv+":") {
		t.Fatalf("docker-compose.prod.yml does not set %s. Discovery is what let a "+
			"missing release artifact look like a working service on a developer "+
			"machine and a dead one in production.", priceBoardPathEnv)
	}
	if !strings.Contains(string(compose), releasePriceBoardPath) {
		t.Errorf("docker-compose.prod.yml does not name %s", releasePriceBoardPath)
	}
}

// The resolver's rules, including the ones that only bite in production.
func TestPriceBoardResolutionRefusesWhatProductionMustRefuse(t *testing.T) {
	// A repo checkout resolves, so development keeps working.
	resolved, err := resolvePriceBoard("development")
	if err != nil {
		t.Fatalf("a repository checkout did not resolve a board: %v", err)
	}
	if resolved.Source != "repo" && resolved.Source != "release" {
		t.Errorf("unexpected source %q in development", resolved.Source)
	}

	// Production refuses to fall back to the repository. This is the whole rule:
	// a production start that succeeds must have loaded the release artifact, or
	// a successful start says nothing about which prices are live.
	t.Setenv("MERC_ENV", "production")
	if _, err := os.Stat(releasePriceBoardPath); err == nil {
		t.Skip("this host has a release board installed; the fallback rule cannot " +
			"be exercised here")
	}
	if _, err := resolvePriceBoard("production"); err == nil {
		t.Fatal("production fell back to a repository-relative price board")
	} else if !strings.Contains(err.Error(), releasePriceBoardPath) {
		t.Errorf("the refusal does not name the path the image is supposed to ship "+
			"to: %v", err)
	}

	// Naming the release path IS the documented production configuration and must
	// boot without ceremony. Requiring a digest to state the obvious would mean
	// the correct configuration could not start, which is how the first cut of
	// this rule failed its own image-boot gate.
	t.Setenv(priceBoardPathEnv, releasePriceBoardPath)
	if named, err := resolvePriceBoard("production"); err != nil {
		t.Fatalf("naming the release path was refused: %v", err)
	} else if named.Source != "release" {
		t.Errorf("naming the release path resolved as source %q", named.Source)
	}

	// Pointing ELSEWHERE in production must declare the digest it expects.
	// Repricing the entire catalogue must not be one environment variable away.
	t.Setenv(priceBoardPathEnv, "/tmp/somebody-elses-board.json")
	if _, err := resolvePriceBoard("production"); err == nil {
		t.Fatal("a production override was accepted with no expected digest")
	} else if !strings.Contains(err.Error(), priceBoardDigestEnv) {
		t.Errorf("the refusal does not name %s: %v", priceBoardDigestEnv, err)
	}

	// With a digest it is allowed, because an operator naming specific bytes has
	// made a checkable statement.
	t.Setenv(priceBoardDigestEnv, strings.Repeat("a", 64))
	got, err := resolvePriceBoard("production")
	if err != nil {
		t.Fatalf("a digest-declared override was refused: %v", err)
	}
	if got.Source != "env" || got.ExpectedDigest != strings.Repeat("a", 64) {
		t.Fatalf("resolved = %+v", got)
	}
}

// A declared digest is enforced in every environment, so the statement gets
// tested before it matters.
func TestPriceBoardDigestMismatchIsRefused(t *testing.T) {
	raw := []byte(`{"schema_version":1}`)
	actual, err := verifyPriceBoardDigest(raw, "")
	if err != nil {
		t.Fatalf("no declared digest should never refuse: %v", err)
	}
	if len(actual) != 64 {
		t.Fatalf("digest %q is not a sha256", actual)
	}
	if _, err := verifyPriceBoardDigest(raw, actual); err != nil {
		t.Errorf("a matching digest was refused: %v", err)
	}
	if _, err := verifyPriceBoardDigest(raw, strings.Repeat("b", 64)); err == nil {
		t.Error("a mismatched digest was accepted; the catalogue would have been " +
			"priced from bytes the operator did not approve")
	}
}

// /version must be able to say which board it is serving.
func TestVersionReportsThePriceBoardItLoaded(t *testing.T) {
	info := currentControlBuildInfo()
	if info.CapabilityMatrixSHA256 != generatedRuntimeMatrixSHA256 {
		t.Error("/version does not report the capability matrix agents bind")
	}
	if info.PriceBoardSource == "" {
		t.Fatal("/version reports no price board source at all")
	}
	if info.PriceBoardSource != "unavailable" && len(info.PriceBoardSHA256) != 64 {
		t.Errorf("price board source is %q but the digest is %q",
			info.PriceBoardSource, info.PriceBoardSHA256)
	}
	t.Logf("price board: source=%s sha256=%s", info.PriceBoardSource, info.PriceBoardSHA256)
}
