package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Object isolation between buyers is currently a property of how the code is
// written -- every object key is built server-side from the job id, and no
// endpoint takes a caller-supplied key -- rather than something anything
// checks. That holds until someone adds an endpoint that accepts a ref, which
// is an easy and quiet thing to do. These are the gates for it.

// A buyer must not be able to reach another buyer's job through any of the
// buyer-facing job routes, and must never be handed a presigned URL for it.
func TestBuyerCannotReachAnotherBuyersJobObjects(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)
	storage := openObjectStorageForTest(t)
	server := NewServer(store, storage, nil, nil)

	owner, intruder := uuid.New(), uuid.New()
	for _, b := range []uuid.UUID{owner, intruder} {
		if _, err := store.pool.Exec(ctx,
			`INSERT INTO buyers (id,email) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			b, "tenant-"+b.String()+"@example.invalid"); err != nil {
			t.Fatalf("seed buyer: %v", err)
		}
	}

	jobID := uuid.New()
	inputKey := "jobs/" + jobID.String() + "/input.jsonl"
	outputKey := "jobs/" + jobID.String() + "/output.jsonl"
	for _, k := range []string{inputKey, outputKey} {
		mustf(t, storage.PutObject(ctx, k, []byte("owner-only-"+k), "application/octet-stream"), "seed object: %v")
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,tier,input_ref,output_ref,created_at)
		VALUES ($1,$2,'complete','embed','batch',$3,$4, now())`,
		jobID, owner, inputKey, outputKey); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	// The owner reads their own job: this must succeed, or a later "intruder
	// blocked" result would prove nothing about isolation and everything about
	// the route being broken for everyone.
	if _, err := store.GetJob(ctx, jobID, owner); err != nil {
		t.Fatalf("owner cannot read their own job: %v", err)
	}
	if _, err := store.GetJob(ctx, jobID, intruder); err == nil {
		t.Fatal("GetJob returned another buyer's job")
	}
	if jobs, err := store.ListJobsForBuyer(ctx, owner, 100); err != nil {
		t.Fatalf("owner cannot list jobs: %v", err)
	} else if len(jobs) != 1 || jobs[0].ID != jobID {
		t.Fatalf("owner job list = %+v, want only %s", jobs, jobID)
	}
	if jobs, err := store.ListJobsForBuyer(ctx, intruder, 100); err != nil {
		t.Fatalf("intruder job list failed: %v", err)
	} else if len(jobs) != 0 {
		t.Fatalf("intruder job list leaked owner jobs: %+v", jobs)
	}
	if err := store.CancelJob(ctx, jobID, intruder); err == nil {
		t.Fatal("CancelJob accepted another buyer's job")
	}

	// And at the HTTP layer, where the presigned URL would actually be handed
	// out: no response to the intruder may contain one.
	for _, path := range []string{
		"/v1/jobs/" + jobID.String(),
		"/v1/jobs/" + jobID.String() + "/results",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxBuyer,
			&AuthResult{BuyerID: intruder}))
		rec := httptest.NewRecorder()
		server.Routes().ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Fatalf("%s returned 200 to a non-owner", path)
		}
		body := rec.Body.String()
		for _, leak := range []string{jobID.String() + "/input", jobID.String() + "/output",
			"X-Amz-Signature", "x-amz-signature"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s leaked %q to a non-owner: %s", path, leak, body)
			}
		}
	}
}

// No buyer-facing request body may carry an object key. If one ever can, a
// caller names the object instead of the server deriving it, and every
// isolation guarantee above becomes advisory.
func TestJobSubmitRejectsCallerSuppliedObjectRefs(t *testing.T) {
	// A body that is valid apart from the ref, so a rejection can only be
	// about the ref. The first version of this test used "model":"m", which
	// fails on ModelRef's type before the decoder ever reaches the ref field:
	// it passed against a jobSubmit that did accept input_ref.
	const valid = `{"model":{"kind":"catalogue","ref":"m"},"job_type":{"type":"embed"},"input":["x"]}`
	var control jobSubmit
	mustf(t, decodeStrictJSONObject([]byte(valid), &control), "baseline body must decode, or a rejection below proves nothing: %v")

	for _, field := range []string{
		`"input_ref":"jobs/00000000-0000-0000-0000-000000000001/output.jsonl"`,
		`"output_ref":"jobs/00000000-0000-0000-0000-000000000001/output.jsonl"`,
		`"input_ref":"../../etc/passwd"`,
		`"result_key":"jobs/00000000-0000-0000-0000-000000000001/tasks/x/attempt-1/result.json"`,
		`"object_key":"anything"`,
	} {
		body := strings.TrimSuffix(valid, "}") + "," + field + "}"
		// decodeStrictJSONObject is what the submit handler actually uses; a
		// test that ran its own decoder would prove nothing about production.
		var submit jobSubmit
		err := decodeStrictJSONObject([]byte(body), &submit)
		if err == nil {
			t.Fatalf("job submit accepted a caller-supplied object ref: %s", body)
		}
		if !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s was rejected for the wrong reason (%v); the rejection must be "+
				"about the unknown ref field, not an unrelated decode failure", field, err)
		}
	}
}
