package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func projectTarArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestProjectCompileProductionRouteBindsProposalAndProbe(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@project-compile.invalid"); err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "project-compile", true)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(store, nil, nil, nil).Routes()
	archive := projectTarArchive(t, map[string]string{
		"Dockerfile":        "FROM alpine:3.20\n",
		"src/pipeline.py":   "embedding = client.embedding(batch_infer)\n",
		"schema/config.txt": "json_schema\n",
	})
	post := func(body []byte, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/projects/compile", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+buyerKey)
		req.Header.Set("Content-Type", "application/x-tar")
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	proposalRec := post(archive, nil)
	if proposalRec.Code != http.StatusOK || proposalRec.Header().Get("X-Merc-Bounded-Probe") != "not_requested" {
		t.Fatalf("proposal status=%d headers=%v body=%s", proposalRec.Code, proposalRec.Header(), proposalRec.Body.String())
	}
	var proposal ProjectWorkloadIR
	if err := json.Unmarshal(proposalRec.Body.Bytes(), &proposal); err != nil {
		t.Fatal(err)
	}
	if proposal.Status != "PROPOSED_NOT_ADMISSIBLE" || proposal.IRSHA256 == "" || proposal.ProjectSHA256 == "" ||
		len(proposal.Detections) == 0 || proposal.Probe.Executed || proposal.Economics.PricingDecisionSHA256 != "" ||
		proposal.Estimate.State != "UNCALIBRATED_REFUSE" {
		t.Fatalf("proposal overclaimed or lost detector authority: %+v", proposal)
	}

	probeRec := post(archive, map[string]string{
		"X-Merc-Bounded-Probe":      "true",
		"X-Merc-Approved-IR-SHA256": proposal.IRSHA256,
	})
	if probeRec.Code != http.StatusOK || probeRec.Header().Get("X-Merc-Bounded-Probe") != "executed" {
		t.Fatalf("probe status=%d headers=%v body=%s", probeRec.Code, probeRec.Header(), probeRec.Body.String())
	}
	var probed ProjectWorkloadIR
	if err := json.Unmarshal(probeRec.Body.Bytes(), &probed); err != nil {
		t.Fatal(err)
	}
	if !probed.Probe.Executed || probed.Probe.ApprovedIRSHA256 != proposal.IRSHA256 ||
		probed.ProjectSHA256 != proposal.ProjectSHA256 || probed.IRSHA256 == proposal.IRSHA256 {
		t.Fatalf("probe did not bind the approved proposal and fresh probe digest: %+v proposal=%+v", probed.Probe, proposal)
	}

	changed := projectTarArchive(t, map[string]string{
		"Dockerfile":      "FROM alpine:3.20\n",
		"src/pipeline.py": "embedding = client.embedding(batch_infer)\nchanged\n",
	})
	changedRec := post(changed, map[string]string{
		"X-Merc-Bounded-Probe":      "1",
		"X-Merc-Approved-IR-SHA256": proposal.IRSHA256,
	})
	if changedRec.Code != http.StatusBadRequest || !strings.Contains(changedRec.Body.String(), "refusing changed project") {
		t.Fatalf("changed project status=%d body=%s", changedRec.Code, changedRec.Body.String())
	}

	var linkArchive bytes.Buffer
	tw := tar.NewWriter(&linkArchive)
	if err := tw.WriteHeader(&tar.Header{Name: "link.py", Typeflag: tar.TypeSymlink, Linkname: "outside.py"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	linkRec := post(linkArchive.Bytes(), nil)
	if linkRec.Code != http.StatusBadRequest || !strings.Contains(linkRec.Body.String(), "not allowed") {
		t.Fatalf("symlink archive status=%d body=%s", linkRec.Code, linkRec.Body.String())
	}
}
