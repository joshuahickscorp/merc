package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func alphaHonestyRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "ops", "scripts", "alpha", "lib.sh")); err != nil {
		t.Fatalf("repo root %s missing ops/scripts/alpha/lib.sh: %v", root, err)
	}
	return root
}

func alphaLibOutput(t *testing.T, root string, extraEnv []string, snippet string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", "set -euo pipefail; . \""+root+"/ops/scripts/alpha/lib.sh\"; "+snippet)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func writeAlphaHonestyJSON(t *testing.T, path string, body any) {
	t.Helper()
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAlphaBootReceiptWrongCommitIsNotGreen(t *testing.T) {
	root := alphaHonestyRepoRoot(t)
	dir := t.TempDir()
	receipt := filepath.Join(dir, "boot.json")
	writeAlphaHonestyJSON(t, receipt, map[string]any{
		"schema_version": 1,
		"kind":           "alpha_boot_green",
		"status":         "PASS",
		"binding_status": "BOUND",
		"lane":           "VENDOR_WALL_UPPER_BOUND",
		"commit":         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	env := []string{
		"MERC_ALPHA_BOOT_RECEIPT=" + receipt,
		"MERC_CANDIDATE_COMMIT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	got, err := alphaLibOutput(t, root, env, "alpha_boot_status")
	if err != nil {
		t.Fatalf("alpha_boot_status: %v\n%s", err, got)
	}
	if got == "PASS" {
		t.Fatal("BOUND PASS receipt at the wrong commit must not be green")
	}
}

func TestAlphaBootReceiptMatchingCommitIsGreen(t *testing.T) {
	root := alphaHonestyRepoRoot(t)
	dir := t.TempDir()
	receipt := filepath.Join(dir, "boot.json")
	const commit = "cccccccccccccccccccccccccccccccccccccccc"
	writeAlphaHonestyJSON(t, receipt, map[string]any{
		"schema_version": 1,
		"kind":           "alpha_boot_green",
		"status":         "PASS",
		"binding_status": "BOUND",
		"lane":           "VENDOR_WALL_UPPER_BOUND",
		"commit":         commit,
	})
	env := []string{
		"MERC_ALPHA_BOOT_RECEIPT=" + receipt,
		"MERC_CANDIDATE_COMMIT=" + commit,
	}
	got, err := alphaLibOutput(t, root, env, "alpha_boot_status")
	if err != nil {
		t.Fatalf("alpha_boot_status: %v\n%s", err, got)
	}
	if got != "PASS" {
		t.Fatalf("matching commit: boot status %q, want PASS", got)
	}
}

func TestLiveAlphaBootReceiptIsHonestToHEAD(t *testing.T) {
	root := alphaHonestyRepoRoot(t)
	headOut, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	head := strings.TrimSpace(string(headOut))
	raw, err := os.ReadFile(filepath.Join(root, "evidence", "state", "alpha-boot-green.json"))
	if err != nil {
		t.Fatalf("read boot receipt: %v", err)
	}
	var receipt struct {
		Status        string `json:"status"`
		BindingStatus string `json:"binding_status"`
		Commit        string `json:"commit"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("parse boot receipt: %v", err)
	}
	status, err := alphaLibOutput(t, root, nil, "alpha_boot_status")
	if err != nil {
		t.Fatalf("alpha_boot_status: %v\n%s", err, status)
	}
	if receipt.Commit == head && receipt.Status == "PASS" && receipt.BindingStatus == "BOUND" {
		if status != "PASS" {
			t.Fatalf("receipt is BOUND PASS at HEAD %s but boot status is %q", head, status)
		}
		return
	}
	if status == "PASS" {
		t.Fatalf("boot is green while receipt commit %s != HEAD %s (or receipt is not BOUND PASS)", receipt.Commit, head)
	}
}

func TestP1IndependentApprovalFollowsGoNoGoLedger(t *testing.T) {
	root := alphaHonestyRepoRoot(t)
	dir := t.TempDir()

	openPath := filepath.Join(dir, "open.json")
	writeAlphaHonestyJSON(t, openPath, map[string]any{
		"schema_version": 2,
		"decisions": map[string]any{
			"live_money_or_public_launch": "NO_GO_PROHIBITED",
		},
		"open_p1": []map[string]any{{
			"id":    "P1-INDEPENDENT-APPROVAL",
			"owner": "repository_owner",
		}},
	})
	openEnv := []string{"MERC_ALPHA_GO_NO_GO=" + openPath}
	got, err := alphaLibOutput(t, root, openEnv, "alpha_ledger_gate_state P1-INDEPENDENT-APPROVAL")
	if err != nil {
		t.Fatalf("open ledger state: %v\n%s", err, got)
	}
	if got != "open" {
		t.Fatalf("open ledger: state %q, want open", got)
	}
	label, err := alphaLibOutput(t, root, openEnv, "alpha_state_label P1-INDEPENDENT-APPROVAL")
	if err != nil {
		t.Fatalf("open ledger label: %v\n%s", err, label)
	}
	if label == "dropped" {
		t.Fatal("open_p1 entry must not be labelled dropped")
	}

	droppedPath := filepath.Join(dir, "dropped.json")
	writeAlphaHonestyJSON(t, droppedPath, map[string]any{
		"schema_version": 2,
		"open_p1":        []any{},
		"dropped_p1":     []any{"P1-INDEPENDENT-APPROVAL"},
	})
	droppedEnv := []string{"MERC_ALPHA_GO_NO_GO=" + droppedPath}
	got, err = alphaLibOutput(t, root, droppedEnv, "alpha_ledger_gate_state P1-INDEPENDENT-APPROVAL")
	if err != nil {
		t.Fatalf("dropped ledger state: %v\n%s", err, got)
	}
	if got != "dropped" {
		t.Fatalf("dropped ledger: state %q, want dropped", got)
	}
	status, err := alphaLibOutput(t, root, droppedEnv, "alpha_receipt_status P1-INDEPENDENT-APPROVAL")
	if err != nil {
		t.Fatalf("dropped receipt status: %v\n%s", err, status)
	}
	if status != "dropped" {
		t.Fatalf("dropped ledger: receipt status %q, want dropped", status)
	}

	raw, err := os.ReadFile(filepath.Join(root, "ops", "go-no-go.json"))
	if err != nil {
		t.Fatalf("read go-no-go: %v", err)
	}
	var live struct {
		OpenP1 []struct {
			ID string `json:"id"`
		} `json:"open_p1"`
		DroppedP1 []any `json:"dropped_p1"`
	}
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatalf("parse go-no-go: %v", err)
	}
	inOpen := false
	for _, item := range live.OpenP1 {
		if item.ID == "P1-INDEPENDENT-APPROVAL" {
			inOpen = true
			break
		}
	}
	liveState, err := alphaLibOutput(t, root, nil, "alpha_ledger_gate_state P1-INDEPENDENT-APPROVAL")
	if err != nil {
		t.Fatalf("live ledger state: %v\n%s", err, liveState)
	}
	want := "absent"
	if inOpen {
		want = "open"
	} else {
		for _, item := range live.DroppedP1 {
			switch v := item.(type) {
			case string:
				if v == "P1-INDEPENDENT-APPROVAL" {
					want = "dropped"
				}
			case map[string]any:
				if id, _ := v["id"].(string); id == "P1-INDEPENDENT-APPROVAL" {
					want = "dropped"
				}
			}
		}
	}
	if liveState != want {
		t.Fatalf("live lib.sh state %q disagrees with ops/go-no-go.json (%s)", liveState, want)
	}
}

func TestCitedCanaryComposeOverlayIsTracked(t *testing.T) {
	root := alphaHonestyRepoRoot(t)
	overlay := filepath.Join(root, "docker-compose.canary.yml")
	raw, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatalf("launch review names docker-compose.canary.yml but it is not a file: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "MERC_ENV: staging") {
		t.Fatal("docker-compose.canary.yml must override MERC_ENV: staging")
	}
	if strings.Contains(body, "sk_live_") || strings.Contains(body, "rk_live_") || strings.Contains(body, "pk_live_") {
		t.Fatal("canary overlay must not contain live Stripe prefixes")
	}
	review, err := os.ReadFile(filepath.Join(root, "docs", "archive", "staging", "ALPHA_LAUNCH_READINESS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "docker-compose.canary.yml") {
		t.Fatal("ALPHA_LAUNCH_READINESS.md no longer names docker-compose.canary.yml")
	}
	runbook, err := os.ReadFile(filepath.Join(root, "ops", "scripts", "alpha", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runbook), "docker-compose.canary.yml") {
		t.Fatal("deploy runbook must name the tracked canary overlay")
	}
}

func TestGoNoGoLiveMoneyRemainsProhibited(t *testing.T) {
	root := alphaHonestyRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "ops", "go-no-go.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decision struct {
		Decisions map[string]string `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Decisions["live_money_or_public_launch"] != "NO_GO_PROHIBITED" {
		t.Fatalf("live_money_or_public_launch=%q, want NO_GO_PROHIBITED",
			decision.Decisions["live_money_or_public_launch"])
	}
}
