package main

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestOperationalControlsAreDurableAndActorAudited(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	actor := testAdminActor(uuid.New())
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,'control-admin')`,
		actor.PrincipalID, "control-test-"+actor.PrincipalID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE operational_controls
			SET paused=false,reason='test cleanup',updated_by=NULL,updated_at=now(),version=version+1`)
		_, _ = pool.Exec(ctx, `DELETE FROM admin_actions WHERE actor_principal_id=$1`, actor.PrincipalID)
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id=$1`, actor.PrincipalID)
	})

	for _, name := range []string{controlIntake, controlDispatch, controlPayments, controlWebhooks} {
		correlation := "control-" + name + "-" + uuid.NewString()
		got, err := store.AdminSetOperationalControl(
			ctx, actor, name, true, "exercise "+name+" pause", correlation)
		if err != nil {
			t.Fatalf("pause %s: %v", name, err)
		}
		if !got.Paused || got.Name != name || got.UpdatedBy == nil || *got.UpdatedBy != actor.PrincipalID {
			t.Fatalf("pause %s returned incomplete state: %+v", name, got)
		}
		paused, err := store.OperationalControlPaused(ctx, name)
		if err != nil || !paused {
			t.Fatalf("durable pause %s = %v, %v", name, paused, err)
		}

		var targetKind, reason, digest string
		var targetID uuid.UUID
		if err := pool.QueryRow(ctx, `
			SELECT target_kind,target_id,reason,request_sha256
			FROM admin_actions WHERE correlation_ref=$1`, correlation,
		).Scan(&targetKind, &targetID, &reason, &digest); err != nil {
			t.Fatalf("audit %s: %v", name, err)
		}
		if targetKind != adminTargetControl || targetID != operationalControlID(name) ||
			reason == "" || len(digest) != 64 {
			t.Fatalf("audit %s is incomplete", name)
		}

		if _, err := store.AdminSetOperationalControl(
			ctx, actor, name, false, "exercise complete", correlation+"-resume"); err != nil {
			t.Fatalf("resume %s: %v", name, err)
		}
	}
}

func TestOperationalControlMutationFailsClosed(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	actor := testAdminActor(uuid.New())
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,'control-admin')`,
		actor.PrincipalID, "control-test-"+actor.PrincipalID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id=$1`, actor.PrincipalID) })

	if _, err := store.AdminSetOperationalControl(ctx, actor, controlIntake, true, "", ""); !errors.Is(err, errAdminMutationInvalid) {
		t.Fatalf("empty reason accepted: %v", err)
	}
	if _, err := store.AdminSetOperationalControl(ctx, actor, "unknown", true, "test", ""); !errors.Is(err, errAdminMutationInvalid) {
		t.Fatalf("unknown control accepted: %v", err)
	}
}
