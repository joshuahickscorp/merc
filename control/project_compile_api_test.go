package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

func renderProjectTarArchive(t *testing.T) []byte {
	t.Helper()
	declaration := projectDeclarationFixture()
	files := map[string]string{"pipeline.py": "json_schema rendering\n"}
	for path, pin := range map[string]*ProjectIRArtifactPin{
		"scene.blend":         &declaration.Steps[0].Rendering.Scene,
		"engine.bin":          &declaration.Steps[0].Rendering.Engine,
		"textures/albedo.png": &declaration.Steps[0].Rendering.Assets[0],
		"plugins/denoise.bin": &declaration.Steps[0].Rendering.Plugins[0],
		"fonts/inter.ttf":     &declaration.Steps[0].Rendering.Fonts[0],
	} {
		contents := "project render asset: " + path
		digest := sha256.Sum256([]byte(contents))
		pin.SHA256 = hex.EncodeToString(digest[:])
		files[path] = contents
	}
	declarationRaw, err := json.Marshal(declaration)
	if err != nil {
		t.Fatal(err)
	}
	files[projectDeclarationName] = string(declarationRaw)
	return projectTarArchive(t, files)
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
	receiptID, err := uuid.Parse(proposalRec.Header().Get("X-Merc-Project-Compile-Receipt"))
	if err != nil {
		t.Fatalf("proposal omitted durable compile receipt id: %q", proposalRec.Header().Get("X-Merc-Project-Compile-Receipt"))
	}
	getReceipt := httptest.NewRequest(http.MethodGet, "/v1/projects/compile/"+receiptID.String(), nil)
	getReceipt.Header.Set("Authorization", "Bearer "+buyerKey)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReceipt)
	var storedReceipt ProjectCompileReceipt
	if getRec.Code != http.StatusOK || json.Unmarshal(getRec.Body.Bytes(), &storedReceipt) != nil ||
		storedReceipt.ID != receiptID.String() || storedReceipt.Status != "PROPOSED_NOT_ADMISSIBLE" ||
		storedReceipt.IR.IRSHA256 == "" || storedReceipt.IR.ProjectSHA256 == "" {
		t.Fatalf("durable compile receipt read failed: status=%d body=%s receipt=%+v", getRec.Code, getRec.Body.String(), storedReceipt)
	}
	if _, err := pool.Exec(ctx, `UPDATE project_compile_receipts SET status='PROBED_NOT_ADMISSIBLE' WHERE id=$1`, receiptID); err == nil {
		t.Fatal("database allowed an immutable project compile receipt to mutate")
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

func TestProjectCompileRenderUnitRouteExpandsDurableIROnly(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@render-unit.invalid"); err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "render-unit", true)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(store, nil, nil, nil).Routes()
	post := httptest.NewRequest(http.MethodPost, "/v1/projects/compile", bytes.NewReader(renderProjectTarArchive(t)))
	post.Header.Set("Authorization", "Bearer "+buyerKey)
	post.Header.Set("Content-Type", "application/x-tar")
	posted := httptest.NewRecorder()
	handler.ServeHTTP(posted, post)
	if posted.Code != http.StatusOK {
		t.Fatalf("render compile status=%d body=%s", posted.Code, posted.Body.String())
	}
	receiptID, err := uuid.Parse(posted.Header().Get("X-Merc-Project-Compile-Receipt"))
	if err != nil {
		t.Fatalf("render compile omitted receipt id: %v", err)
	}
	get := httptest.NewRequest(http.MethodGet,
		"/v1/projects/compile/"+receiptID.String()+"/render/render/units/0", nil)
	get.Header.Set("Authorization", "Bearer "+buyerKey)
	got := httptest.NewRecorder()
	handler.ServeHTTP(got, get)
	var receipt ProjectRenderWorkUnitReceipt
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &receipt) != nil ||
		receipt.Version != renderWorkUnitReceiptVersion || receipt.CompileReceiptID != receiptID.String() ||
		receipt.Status != "DECOMPOSITION_ONLY_NOT_EXECUTABLE" || receipt.StepID != "render" ||
		receipt.WorkUnit.Ordinal != 0 || receipt.WorkUnit.Frame != 1 || receipt.WorkUnit.Camera != "hero" ||
		receipt.WorkUnit.PixelWidth != 256 || receipt.WorkUnit.PixelHeight != 256 ||
		!strings.Contains(receipt.ExecutionRefusal, "unresolved") {
		t.Fatalf("render unit route overclaimed or lost deterministic identity: status=%d body=%s receipt=%+v", got.Code, got.Body.String(), receipt)
	}
	bad := httptest.NewRequest(http.MethodGet,
		"/v1/projects/compile/"+receiptID.String()+"/render/render/units/960", nil)
	bad.Header.Set("Authorization", "Bearer "+buyerKey)
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest || !strings.Contains(badRec.Body.String(), "outside") {
		t.Fatalf("out-of-range render unit status=%d body=%s", badRec.Code, badRec.Body.String())
	}
}
