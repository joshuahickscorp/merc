package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bindWorkerToGovernedProfile stamps a worker row with the full governed
// identity a dispatchable worker must carry.
//
// Test fixtures that INSERT worker_authorized_capabilities directly bypass
// UpsertWorker, which is the only production path that resolves and validates a
// profile. A trigger now refuses dispatch capability for a worker with no
// governed identity, and it correctly caught four such fixtures — they were
// constructing a shape production cannot produce. This helper makes them obey
// the same rule rather than weakening the rule to fit them.
func bindWorkerToGovernedProfile(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	workerID uuid.UUID) {
	t.Helper()
	id, revision, digest, err := governedProfileIdentity("candle")
	if err != nil {
		// Historical/direct money-path fixtures are deliberately constructed
		// before their TEST_ONLY current benchmark authority is installed. With
		// every checked-in execution credential honestly demoted, there is no
		// buyer-routable profile for governedProfileIdentity to select. Still bind
		// the worker to the exact declared profile identity so these direct rows
		// have a shape production once emitted; they remain non-routable until the
		// test installs its explicit synthetic authority and capability.
		profile, ok := runtimeProfileByID("candle_metal")
		if !ok || profile.Engine != "candle" {
			t.Fatalf("resolve declared historical test profile after routable lookup failed (%v)", err)
		}
		id, revision = profile.RuntimeID, profile.Revision
		digest, err = profile.CapabilityDigest(runtimeAuthorityModels)
		mustf(t, err, "digest declared historical test profile: %v")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE workers SET engine='candle', runtime_profile_id=$2,
		                    runtime_profile_revision=$3, runtime_profile_digest=$4
		  WHERE id=$1`, workerID, id, revision, digest); err != nil {
		t.Fatalf("bind worker %s to governed profile: %v", workerID, err)
	}
}
