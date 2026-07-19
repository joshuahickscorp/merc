package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed runtime-authority.json
var runtimeAuthorityJSON []byte

type runtimeAuthorityDocument struct {
	SchemaVersion int    `json:"schema_version"`
	MatrixVersion string `json:"matrix_version"`
	Runtime       struct {
		ID              string   `json:"id"`
		Engine          string   `json:"engine"`
		Device          string   `json:"device"`
		HardwareClasses []string `json:"hardware_classes"`
	} `json:"runtime"`
	Models []struct {
		ID          string  `json:"id"`
		WireKind    string  `json:"wire_kind"`
		Job         string  `json:"job_type"`
		MinMemoryGB float64 `json:"min_memory_gb"`
	} `json:"models"`
	Cells []struct {
		ID           string  `json:"id"`
		Job          string  `json:"job"`
		Model        string  `json:"model"`
		Runner       string  `json:"runner"`
		MinMemoryGB  float64 `json:"min_memory_gb"`
		Verification string  `json:"verification"`
	} `json:"cells"`
}

type generatedRuntimeCapability struct {
	ID              string
	Runtime         string
	Engine          string
	Device          string
	HardwareClasses []string
	Job             string
	Model           string
	ModelKind       string
	Runner          string
	MinMemoryGB     float64
	Verification    string
}

var (
	runtimeAuthority                       = loadRuntimeAuthority()
	generatedRuntimeMatrixVersion          = runtimeAuthority.MatrixVersion
	generatedRuntimeMatrixSHA256           = runtimeAuthoritySHA256()
	generatedAdvertisedRuntimeCapabilities = projectRuntimeCapabilities(runtimeAuthority)
)

func loadRuntimeAuthority() runtimeAuthorityDocument {
	var authority runtimeAuthorityDocument
	if err := json.Unmarshal(runtimeAuthorityJSON, &authority); err != nil {
		panic(fmt.Sprintf("decode embedded runtime authority: %v", err))
	}
	if authority.SchemaVersion != 1 || authority.MatrixVersion == "" ||
		authority.Runtime.ID == "" || authority.Runtime.Engine == "" ||
		authority.Runtime.Device == "" || len(authority.Runtime.HardwareClasses) == 0 ||
		len(authority.Models) != 2 || len(authority.Cells) != 2 {
		panic("embedded runtime authority is incomplete")
	}
	return authority
}

func runtimeAuthoritySHA256() string {
	sum := sha256.Sum256(runtimeAuthorityJSON)
	return hex.EncodeToString(sum[:])
}

func projectRuntimeCapabilities(authority runtimeAuthorityDocument) []generatedRuntimeCapability {
	models := make(map[string]struct {
		kind string
		job  string
		min  float64
	}, len(authority.Models))
	for _, model := range authority.Models {
		if model.ID == "" || model.WireKind == "" || model.Job == "" || model.MinMemoryGB <= 0 {
			panic("embedded runtime authority contains an invalid model")
		}
		if _, exists := models[model.ID]; exists {
			panic("embedded runtime authority contains a duplicate model")
		}
		models[model.ID] = struct {
			kind string
			job  string
			min  float64
		}{model.WireKind, model.Job, model.MinMemoryGB}
	}
	capabilities := make([]generatedRuntimeCapability, 0, len(authority.Cells))
	seen := make(map[string]bool, len(authority.Cells))
	for _, cell := range authority.Cells {
		model, ok := models[cell.Model]
		if seen[cell.ID] || !ok || cell.ID == "" || cell.Job != model.job ||
			cell.Runner != cell.Job || cell.MinMemoryGB < model.min || cell.Verification == "" {
			panic("embedded runtime authority contains an invalid cell")
		}
		seen[cell.ID] = true
		capabilities = append(capabilities, generatedRuntimeCapability{
			ID: cell.ID, Runtime: authority.Runtime.ID, Engine: authority.Runtime.Engine,
			Device: authority.Runtime.Device, HardwareClasses: authority.Runtime.HardwareClasses,
			Job: cell.Job, Model: cell.Model, ModelKind: model.kind, Runner: cell.Runner,
			MinMemoryGB: cell.MinMemoryGB, Verification: cell.Verification,
		})
	}
	return capabilities
}

func syncRuntimeCatalog(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
WITH desired AS (
    SELECT * FROM jsonb_to_recordset(($1::jsonb)->'models') AS model(
        id text, family text, quant text, kind text, dim int, job_type text,
        price_per_1k numeric, min_memory_gb real, hf_repo text
    )
), upserted AS (
    INSERT INTO models (id, family, quant, kind, dim, job_type, price_per_1k, min_memory_gb, hf_repo)
    SELECT id, family, quant, kind, dim, job_type, price_per_1k, min_memory_gb, hf_repo
      FROM desired
    ON CONFLICT (id) DO UPDATE SET
        family=EXCLUDED.family, quant=EXCLUDED.quant, kind=EXCLUDED.kind,
        dim=EXCLUDED.dim, job_type=EXCLUDED.job_type,
        price_per_1k=EXCLUDED.price_per_1k, min_memory_gb=EXCLUDED.min_memory_gb,
        hf_repo=EXCLUDED.hf_repo
    RETURNING id
)
DELETE FROM models WHERE id NOT IN (SELECT id FROM desired)`, runtimeAuthorityJSON)
	if err != nil {
		return fmt.Errorf("synchronize runtime model catalog: %w", err)
	}
	return nil
}
