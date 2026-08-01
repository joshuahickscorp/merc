package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProjectFixture(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCompileProjectProducesDeterministicGraphProposal(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root, "compose.yaml", "services:\n  api:\n    image: local\n")
	writeProjectFixture(t, root, "src/pipeline.py", "embedding = client.embedding(batch_infer)\n")
	writeProjectFixture(t, root, "samples/input.jsonl", "{\"text\":\"a\"}\n{\"text\":\"b\"}\n")

	proposal, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	opts := projectCompileOptions{Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256}
	first, err := compileProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compileProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.IRSHA256 != second.IRSHA256 || first.ProjectSHA256 != second.ProjectSHA256 {
		t.Fatalf("compiler is nondeterministic: first=%s/%s second=%s/%s", first.IRSHA256, first.ProjectSHA256, second.IRSHA256, second.ProjectSHA256)
	}
	if first.Status != "PROPOSED_NOT_ADMISSIBLE" || first.Estimate.State != "UNCALIBRATED_REFUSE" {
		t.Fatalf("detector proposal became admission or estimate authority: %+v", first)
	}
	if !first.Probe.Executed || first.Probe.Kind != "NON_EXECUTING_FILE_SHAPE_V1" ||
		first.Probe.ApprovedIRSHA256 != proposal.IRSHA256 {
		t.Fatalf("bounded probe missing: %+v", first.Probe)
	}
	if len(first.Steps) < 3 || first.Steps[0].DependsOn != nil || len(first.Steps[1].DependsOn) != 1 {
		t.Fatalf("project graph dependencies missing: %+v", first.Steps)
	}
	if len(first.Artifacts) != 3 || len(first.ProjectSHA256) != 64 || len(first.IRSHA256) != 64 {
		t.Fatalf("artifact commitments missing: %+v", first)
	}
}

func TestCompileProjectRequiresProbeAuthorization(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root, "pipeline.py", "embedding")
	_, err := compileProject(projectCompileOptions{Root: root, ProbeRequested: true})
	if err == nil || !strings.Contains(err.Error(), "buyer-approved") {
		t.Fatalf("probe without authorization was not refused: %v", err)
	}
}

func TestCompileProjectRefusesChangedProjectAfterApproval(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root, "pipeline.py", "embedding")
	proposal, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFixture(t, root, "pipeline.py", "embedding\nbatch_infer")
	_, err = compileProject(projectCompileOptions{
		Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing changed project") {
		t.Fatalf("changed project used stale buyer approval: %v", err)
	}
}

func TestCompileProjectRefusesSensitiveAndAmbiguousInputs(t *testing.T) {
	t.Run("secret", func(t *testing.T) {
		root := t.TempDir()
		writeProjectFixture(t, root, ".env", "TOKEN=secret")
		if _, err := compileProject(projectCompileOptions{Root: root}); err == nil || !strings.Contains(err.Error(), "sensitive path") {
			t.Fatalf("sensitive input was not refused: %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		writeProjectFixture(t, root, "real.py", "embedding")
		if err := os.Symlink(filepath.Join(root, "real.py"), filepath.Join(root, "link.py")); err != nil {
			t.Fatal(err)
		}
		if _, err := compileProject(projectCompileOptions{Root: root}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink boundary was not refused: %v", err)
		}
	})
}

func TestCompileProjectRecordsUnsafeContainerRefusal(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root, "compose.yaml", "services:\n  task:\n    privileged: true\n")
	ir, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(ir.RefusalReasons, "\n")
	if !strings.Contains(joined, "unsafe container authority") {
		t.Fatalf("unsafe project lacks explicit refusal: %+v", ir.RefusalReasons)
	}
}

func TestProjectIRDigestCoversGraphAndExcludesSelf(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root, "pipeline.py", "embedding\nembedding")
	ir, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	want, err := projectIRDigest(ir)
	if err != nil {
		t.Fatal(err)
	}
	if want != ir.IRSHA256 {
		t.Fatalf("self-excluding digest mismatch: want %s got %s", want, ir.IRSHA256)
	}
	ir.Unknowns = append(ir.Unknowns, "new uncertainty")
	changed, err := projectIRDigest(ir)
	if err != nil {
		t.Fatal(err)
	}
	if changed == want {
		t.Fatal("IR digest did not cover graph uncertainty")
	}
}
