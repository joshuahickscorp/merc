from __future__ import annotations

import json
import re
import sqlite3
import uuid
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import Any

from blender_vision.core.errors import ProjectError
from blender_vision.core.models import FidelityLevel, JobStatus
from blender_vision.core.util import atomic_write_json, utc_now

PROJECT_DIRECTORIES = (
    "references/originals",
    "references/masks",
    "measurements",
    "cameras",
    "features",
    "geometry",
    "scene",
    "renders",
    "comparisons",
    "training",
    "exports",
    "receipts",
    "jobs/manifests",
    "jobs/logs",
    "jobs/transfers",
    "observations",
    "artifacts/sha256",
)


SCHEMA = """
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS artifacts (
    digest TEXT PRIMARY KEY,
    size INTEGER NOT NULL,
    media_type TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    source_name TEXT,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reference_items (
    id TEXT PRIMARY KEY,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    original_name TEXT NOT NULL,
    media_type TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    metadata_json TEXT NOT NULL,
    quality_json TEXT NOT NULL,
    rights_state TEXT NOT NULL,
    viewpoint_label TEXT,
    duplicate_of TEXT REFERENCES reference_items(id),
    evidence_role TEXT NOT NULL DEFAULT 'acceptance_reference',
    acceptance_eligible INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reference_derivations (
    id TEXT PRIMARY KEY,
    derived_reference_id TEXT NOT NULL UNIQUE REFERENCES reference_items(id),
    source_reference_id TEXT NOT NULL REFERENCES reference_items(id),
    governed_source_id TEXT REFERENCES evidence_sources(id),
    operation TEXT NOT NULL,
    receipt_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS target_resolutions (
    id TEXT PRIMARY KEY,
    request_text TEXT NOT NULL,
    target_json TEXT NOT NULL,
    alternatives_json TEXT NOT NULL,
    ambiguity_json TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS target_resolution_events (
    id TEXT PRIMARY KEY,
    target_id TEXT NOT NULL UNIQUE REFERENCES target_resolutions(id),
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    supersedes_receipt_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence_sources (
    id TEXT PRIMARY KEY,
    target_id TEXT NOT NULL REFERENCES target_resolutions(id),
    reference_id TEXT REFERENCES reference_items(id),
    source_json TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rights_ledger (
    source_id TEXT PRIMARY KEY REFERENCES evidence_sources(id),
    rights_json TEXT NOT NULL,
    reviewed_by TEXT,
    reviewed_at TEXT,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence_source_governance_reviews (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES evidence_sources(id),
    reviewer TEXT NOT NULL,
    reviewer_type TEXT NOT NULL,
    source_json TEXT NOT NULL,
    rights_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    supersedes_receipt_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence_source_acquisitions (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES evidence_sources(id),
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    governance_receipt_digest TEXT NOT NULL REFERENCES artifacts(digest),
    source_json TEXT NOT NULL,
    reference_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    supersedes_receipt_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reference_adoption_proposals (
    id TEXT PRIMARY KEY,
    target_id TEXT NOT NULL REFERENCES target_resolutions(id),
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    status TEXT NOT NULL,
    proposal_json TEXT NOT NULL,
    proposal_digest TEXT NOT NULL REFERENCES artifacts(digest),
    decision_json TEXT,
    decision_digest TEXT REFERENCES artifacts(digest),
    source_id TEXT REFERENCES evidence_sources(id),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(target_id, reference_id)
);
CREATE TABLE IF NOT EXISTS search_providers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    config_json TEXT NOT NULL,
    reviewer TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS search_provider_reviews (
    id TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL UNIQUE REFERENCES search_providers(id),
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS search_discovery_runs (
    id TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL REFERENCES search_providers(id),
    target_id TEXT NOT NULL REFERENCES target_resolutions(id),
    status TEXT NOT NULL,
    plan_json TEXT NOT NULL,
    results_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence_pursuit_runs (
    id TEXT PRIMARY KEY,
    cache_key TEXT NOT NULL UNIQUE,
    target_id TEXT NOT NULL REFERENCES target_resolutions(id),
    provider_id TEXT REFERENCES search_providers(id),
    status TEXT NOT NULL,
    focus_terms_json TEXT NOT NULL,
    coverage_json TEXT NOT NULL,
    discovery_run_id TEXT REFERENCES search_discovery_runs(id),
    capture_request_ids_json TEXT NOT NULL,
    report_digest TEXT REFERENCES artifacts(digest),
    lease_token TEXT,
    lease_expires_at TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence_conflict_runs (
    id TEXT PRIMARY KEY,
    target_id TEXT NOT NULL REFERENCES target_resolutions(id),
    status TEXT NOT NULL,
    report_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence_conflict_reviews (
    id TEXT PRIMARY KEY,
    target_id TEXT NOT NULL REFERENCES target_resolutions(id),
    source_id TEXT NOT NULL REFERENCES evidence_sources(id),
    category TEXT NOT NULL,
    finding_sha256 TEXT NOT NULL,
    decision TEXT NOT NULL,
    configuration_model_json TEXT NOT NULL,
    reviewer TEXT NOT NULL,
    reason TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence_duplicate_runs (
    id TEXT PRIMARY KEY,
    target_id TEXT NOT NULL REFERENCES target_resolutions(id),
    status TEXT NOT NULL,
    report_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reference_mask_proposals (
    id TEXT PRIMARY KEY,
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    mask_artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    proposal_digest TEXT NOT NULL REFERENCES artifacts(digest),
    status TEXT NOT NULL,
    record_json TEXT NOT NULL,
    decision_digest TEXT REFERENCES artifacts(digest),
    approved_mask_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reference_masks (
    id TEXT PRIMARY KEY,
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    source_artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    method TEXT NOT NULL,
    reviewer TEXT NOT NULL,
    reason TEXT NOT NULL,
    creator TEXT NOT NULL DEFAULT 'unknown',
    backend TEXT NOT NULL DEFAULT 'human_manual',
    revision INTEGER NOT NULL DEFAULT 1,
    approval_state TEXT NOT NULL DEFAULT 'approved',
    confidence TEXT NOT NULL DEFAULT 'high',
    intended_use TEXT NOT NULL DEFAULT 'silhouette_evaluation',
    visible_components_json TEXT NOT NULL DEFAULT '[]',
    excluded_components_json TEXT NOT NULL DEFAULT '[]',
    roi_json TEXT NOT NULL DEFAULT '{}',
    proposal_id TEXT REFERENCES reference_mask_proposals(id),
    decision_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS scene_assets (
    id TEXT PRIMARY KEY,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    original_name TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    inventory_json TEXT,
    state TEXT NOT NULL DEFAULT 'DRAFT',
    is_authoritative INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS scene_transitions (
    id TEXT PRIMARY KEY,
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    reviewer TEXT NOT NULL,
    reason TEXT NOT NULL,
    evaluation_id TEXT,
    receipt_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS candidate_evaluations (
    id TEXT PRIMARY KEY,
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    baseline_scene_id TEXT REFERENCES scene_assets(id),
    status TEXT NOT NULL,
    gates_json TEXT NOT NULL,
    metrics_json TEXT NOT NULL,
    regressions_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    operation TEXT NOT NULL,
    status TEXT NOT NULL,
    cache_key TEXT,
    config_json TEXT NOT NULL,
    result_json TEXT,
    error_json TEXT,
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_cache_key ON jobs(cache_key, status);
CREATE TABLE IF NOT EXISTS job_provenance (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id),
    input_hashes_json TEXT NOT NULL,
    backend_json TEXT NOT NULL,
    execution_json TEXT NOT NULL,
    output_hashes_json TEXT NOT NULL,
    metrics_json TEXT NOT NULL,
    logs_json TEXT NOT NULL,
    failure_class TEXT,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS job_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL REFERENCES jobs(id),
    event TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cache_entries (
    cache_key TEXT PRIMARY KEY,
    operation TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS camera_solutions (
    id TEXT PRIMARY KEY,
    backend TEXT NOT NULL,
    backend_version TEXT,
    solution_json TEXT NOT NULL,
    diagnostics_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    approved INTEGER NOT NULL DEFAULT 0,
    decision_id TEXT,
    decision_digest TEXT REFERENCES artifacts(digest)
);
CREATE TABLE IF NOT EXISTS camera_decisions (
    id TEXT PRIMARY KEY,
    solution_id TEXT NOT NULL REFERENCES camera_solutions(id),
    state TEXT NOT NULL,
    decision_json TEXT NOT NULL,
    decision_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS camera_landmark_proposals (
    id TEXT PRIMARY KEY,
    target_id TEXT NOT NULL REFERENCES target_resolutions(id),
    model_source_id TEXT NOT NULL REFERENCES evidence_sources(id),
    intrinsics_solution_id TEXT NOT NULL REFERENCES camera_solutions(id),
    status TEXT NOT NULL,
    proposal_json TEXT NOT NULL,
    proposal_digest TEXT NOT NULL REFERENCES artifacts(digest),
    review_json TEXT,
    review_digest TEXT REFERENCES artifacts(digest),
    superseded_by_id TEXT REFERENCES camera_landmark_proposals(id),
    supersession_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS camera_refinement_runs (
    id TEXT PRIMARY KEY,
    source_solution_id TEXT NOT NULL REFERENCES camera_solutions(id),
    result_solution_id TEXT NOT NULL REFERENCES camera_solutions(id),
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    status TEXT NOT NULL,
    config_json TEXT NOT NULL,
    report_digest TEXT NOT NULL REFERENCES artifacts(digest),
    best_render_digest TEXT NOT NULL REFERENCES artifacts(digest),
    best_silhouette_iou REAL NOT NULL,
    segmentation_method TEXT NOT NULL,
    segmentation_confidence TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS comparisons (
    id TEXT PRIMARY KEY,
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    render_digest TEXT REFERENCES artifacts(digest),
    residual_digest TEXT REFERENCES artifacts(digest),
    metrics_json TEXT NOT NULL,
    receipt_digest TEXT REFERENCES artifacts(digest),
    superseded_by_id TEXT REFERENCES comparisons(id),
    supersession_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS coverage_reports (
    id TEXT PRIMARY KEY,
    digest TEXT NOT NULL REFERENCES artifacts(digest),
    report_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS receipts (
    id TEXT PRIMARY KEY,
    digest TEXT NOT NULL REFERENCES artifacts(digest),
    fidelity_level TEXT NOT NULL,
    accepted INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS render_runs (
    id TEXT PRIMARY KEY,
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    camera_solution_id TEXT NOT NULL REFERENCES camera_solutions(id),
    config_json TEXT NOT NULL,
    outputs_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS visual_geometry_rigs (
    id TEXT PRIMARY KEY,
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    camera_solution_id TEXT NOT NULL REFERENCES camera_solutions(id),
    state TEXT NOT NULL,
    config_json TEXT NOT NULL,
    config_digest TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS visual_geometry_scorecards (
    id TEXT PRIMARY KEY,
    rig_id TEXT NOT NULL REFERENCES visual_geometry_rigs(id),
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    render_run_id TEXT NOT NULL REFERENCES render_runs(id),
    status TEXT NOT NULL,
    scorecard_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS manufactured_form_audits (
    id TEXT PRIMARY KEY,
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    status TEXT NOT NULL,
    report_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS visual_baseline_freezes (
    id TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    state TEXT NOT NULL,
    snapshot_json TEXT NOT NULL,
    snapshot_digest TEXT NOT NULL UNIQUE,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS semantic_geometry_bindings (
    id TEXT PRIMARY KEY,
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    object_name TEXT NOT NULL,
    semantic_id TEXT NOT NULL REFERENCES semantic_nodes(id),
    parent_assembly_id TEXT NOT NULL REFERENCES semantic_nodes(id),
    state TEXT NOT NULL,
    record_json TEXT NOT NULL,
    proposal_digest TEXT NOT NULL REFERENCES artifacts(digest),
    decision_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(scene_id, object_name)
);
CREATE TABLE IF NOT EXISTS visual_component_packets (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES semantic_geometry_bindings(id),
    rig_id TEXT NOT NULL REFERENCES visual_geometry_rigs(id),
    render_run_id TEXT NOT NULL REFERENCES render_runs(id),
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    status TEXT NOT NULL,
    packet_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS visual_frequency_scorecards (
    id TEXT PRIMARY KEY,
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    rig_id TEXT NOT NULL REFERENCES visual_geometry_rigs(id),
    status TEXT NOT NULL,
    scorecard_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS visual_defect_diagnoses (
    id TEXT PRIMARY KEY,
    scorecard_id TEXT NOT NULL REFERENCES visual_geometry_scorecards(id),
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    rig_id TEXT NOT NULL REFERENCES visual_geometry_rigs(id),
    render_run_id TEXT NOT NULL REFERENCES render_runs(id),
    status TEXT NOT NULL,
    report_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS exports (
    id TEXT PRIMARY KEY,
    scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    format TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    config_json TEXT NOT NULL,
    worker_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS calibration_runs (
    id TEXT PRIMARY KEY,
    benchmark TEXT NOT NULL,
    gates_json TEXT NOT NULL,
    record_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS measurements (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    value_json TEXT NOT NULL,
    evidence_class TEXT NOT NULL,
    uncertainty_json TEXT NOT NULL,
    provenance_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS measurement_grids (
    id TEXT PRIMARY KEY,
    reference_id TEXT NOT NULL REFERENCES reference_items(id),
    record_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS capture_requests (
    id TEXT PRIMARY KEY,
    request_json TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS video_analysis_runs (
    id TEXT PRIMARY KEY,
    source_reference_id TEXT NOT NULL REFERENCES reference_items(id),
    report_json TEXT NOT NULL,
    report_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS feature_tracks (
    id TEXT PRIMARY KEY,
    source_reference_id TEXT NOT NULL REFERENCES reference_items(id),
    semantic_label TEXT NOT NULL,
    observations_json TEXT NOT NULL,
    confidence REAL NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tier_reviews (
    id TEXT PRIMARY KEY,
    requested_fidelity TEXT NOT NULL,
    decision_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS features (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    parent_component TEXT,
    record_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS components (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    record_json TEXT NOT NULL,
    revision INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS semantic_nodes (
    id TEXT PRIMARY KEY,
    parent_id TEXT REFERENCES semantic_nodes(id),
    node_type TEXT NOT NULL,
    record_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS semantic_edges (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES semantic_nodes(id),
    target_id TEXT NOT NULL REFERENCES semantic_nodes(id),
    relation TEXT NOT NULL,
    record_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reconstruction_portfolios (
    id TEXT PRIMARY KEY,
    category TEXT NOT NULL,
    configuration_json TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reconstruction_candidates (
    id TEXT PRIMARY KEY,
    portfolio_id TEXT NOT NULL REFERENCES reconstruction_portfolios(id),
    lane TEXT NOT NULL,
    status TEXT NOT NULL,
    record_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS repair_proposals (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    config_json TEXT NOT NULL,
    evidence_json TEXT NOT NULL,
    expected_json TEXT NOT NULL,
    result_json TEXT,
    approved_by TEXT,
    approved_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS geometry_runs (
    id TEXT PRIMARY KEY,
    backend TEXT NOT NULL,
    backend_version TEXT NOT NULL,
    evidence_class TEXT NOT NULL,
    commercial_eligible INTEGER NOT NULL,
    config_json TEXT NOT NULL,
    evidence_json TEXT NOT NULL,
    record_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS geometry_consensus (
    id TEXT PRIMARY KEY,
    run_ids_json TEXT NOT NULL,
    report_json TEXT NOT NULL,
    record_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS camera_consensus (
    id TEXT PRIMARY KEY,
    solution_ids_json TEXT NOT NULL,
    report_json TEXT NOT NULL,
    record_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS component_fits (
    id TEXT PRIMARY KEY,
    component_id TEXT NOT NULL REFERENCES components(id),
    status TEXT NOT NULL,
    input_json TEXT NOT NULL,
    result_json TEXT NOT NULL,
    record_digest TEXT NOT NULL REFERENCES artifacts(digest),
    decision_digest TEXT REFERENCES artifacts(digest),
    reviewer TEXT,
    reason TEXT,
    reviewed_at TEXT,
    applied_revision INTEGER,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS datasets (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    manifest_json TEXT NOT NULL,
    record_digest TEXT NOT NULL REFERENCES artifacts(digest),
    rights_state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS training_runs (
    id TEXT PRIMARY KEY,
    dataset_id TEXT NOT NULL REFERENCES datasets(id),
    backend TEXT NOT NULL,
    status TEXT NOT NULL,
    config_json TEXT NOT NULL,
    result_json TEXT,
    checkpoint_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS model_evaluations (
    id TEXT PRIMARY KEY,
    training_run_id TEXT REFERENCES training_runs(id),
    dataset_id TEXT NOT NULL REFERENCES datasets(id),
    metrics_json TEXT NOT NULL,
    predictions_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS visual_oracles (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    config_json TEXT NOT NULL,
    license_json TEXT NOT NULL,
    commercial_eligible INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS generative_requests (
    id TEXT PRIMARY KEY,
    operation TEXT NOT NULL,
    backend TEXT NOT NULL,
    request_json TEXT NOT NULL,
    request_digest TEXT REFERENCES artifacts(digest),
    license_json TEXT NOT NULL DEFAULT '{}',
    cache_key TEXT,
    job_id TEXT REFERENCES jobs(id),
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS generative_results (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES generative_requests(id),
    result_json TEXT NOT NULL,
    record_digest TEXT REFERENCES artifacts(digest),
    status TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS generative_result_request
ON generative_results(request_id);
CREATE TABLE IF NOT EXISTS optimization_runs (
    id TEXT PRIMARY KEY,
    component_id TEXT REFERENCES components(id),
    tier TEXT NOT NULL,
    method TEXT NOT NULL,
    status TEXT NOT NULL,
    config_json TEXT NOT NULL,
    evaluations_json TEXT NOT NULL,
    result_json TEXT NOT NULL,
    record_digest TEXT NOT NULL REFERENCES artifacts(digest),
    decision_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS multiview_search_runs (
    id TEXT PRIMARY KEY,
    cache_key TEXT NOT NULL UNIQUE,
    component_id TEXT NOT NULL REFERENCES components(id),
    camera_solution_id TEXT NOT NULL REFERENCES camera_solutions(id),
    baseline_scene_id TEXT NOT NULL REFERENCES scene_assets(id),
    status TEXT NOT NULL,
    semantic_ids_json TEXT NOT NULL,
    locality_plan_json TEXT NOT NULL,
    config_json TEXT NOT NULL,
    optimization_run_id TEXT REFERENCES optimization_runs(id),
    receipt_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS multiview_search_candidates (
    id TEXT PRIMARY KEY,
    search_id TEXT NOT NULL REFERENCES multiview_search_runs(id),
    candidate_index INTEGER NOT NULL,
    status TEXT NOT NULL,
    parameters_json TEXT NOT NULL,
    scene_id TEXT REFERENCES scene_assets(id),
    render_run_id TEXT REFERENCES render_runs(id),
    comparison_ids_json TEXT NOT NULL,
    error_json TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(search_id,candidate_index)
);
CREATE TABLE IF NOT EXISTS campaigns (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    controller_state TEXT NOT NULL,
    iteration INTEGER NOT NULL,
    config_json TEXT NOT NULL,
    budget_json TEXT NOT NULL,
    result_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS campaign_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id),
    controller_state TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_proposals (
    id TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id),
    iteration INTEGER NOT NULL,
    diagnosis TEXT NOT NULL,
    record_json TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS role_tasks (
    id TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id),
    role TEXT NOT NULL,
    objective TEXT NOT NULL,
    status TEXT NOT NULL,
    priority REAL NOT NULL,
    estimated_cost REAL NOT NULL,
    confidence REAL NOT NULL,
    inputs_json TEXT NOT NULL,
    output_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS context_packets (
    id TEXT PRIMARY KEY,
    campaign_id TEXT REFERENCES campaigns(id),
    component_id TEXT,
    packet_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS synthetic_views (
    id TEXT PRIMARY KEY,
    source_kind TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    record_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS active_learning_cycles (
    id TEXT PRIMARY KEY,
    model_level TEXT NOT NULL,
    status TEXT NOT NULL,
    record_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS active_learning_events (
    id TEXT PRIMARY KEY,
    cycle_id TEXT NOT NULL REFERENCES active_learning_cycles(id),
    revision INTEGER NOT NULL,
    from_status TEXT,
    to_status TEXT NOT NULL,
    snapshot_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    UNIQUE(cycle_id, revision)
);
CREATE TABLE IF NOT EXISTS active_model_revisions (
    id TEXT PRIMARY KEY,
    model_level TEXT NOT NULL,
    model_name TEXT NOT NULL,
    model_revision TEXT NOT NULL,
    training_run_id TEXT NOT NULL REFERENCES training_runs(id),
    cycle_id TEXT NOT NULL UNIQUE REFERENCES active_learning_cycles(id),
    checkpoint_digest TEXT NOT NULL REFERENCES artifacts(digest),
    benchmark_evaluation_id TEXT NOT NULL REFERENCES model_evaluations(id),
    status TEXT NOT NULL,
    reviewed_by TEXT NOT NULL,
    reason TEXT NOT NULL,
    activation_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS active_model_revision_current
ON active_model_revisions(model_level, model_name) WHERE status='ACTIVE';
CREATE TABLE IF NOT EXISTS active_model_rollbacks (
    id TEXT PRIMARY KEY,
    rolled_back_revision_id TEXT NOT NULL REFERENCES active_model_revisions(id),
    restored_revision_id TEXT REFERENCES active_model_revisions(id),
    reviewed_by TEXT NOT NULL,
    reason TEXT NOT NULL,
    receipt_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    UNIQUE(rolled_back_revision_id)
);
CREATE TABLE IF NOT EXISTS surface_coverage_cells (
    id TEXT PRIMARY KEY,
    target_id TEXT,
    component_id TEXT,
    region TEXT NOT NULL,
    record_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS warm_services (
    name TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    record_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS benchmark_policy_reviews (
    id TEXT PRIMARY KEY,
    benchmark TEXT NOT NULL,
    review_kind TEXT NOT NULL,
    state TEXT NOT NULL,
    reviewer TEXT NOT NULL,
    reason TEXT NOT NULL,
    strategy_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL UNIQUE REFERENCES artifacts(digest),
    supersedes_receipt_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS beast_benchmark_runs (
    id TEXT PRIMARY KEY,
    stage INTEGER NOT NULL,
    status TEXT NOT NULL,
    report_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    worker_class TEXT NOT NULL,
    auth_token_hash TEXT NOT NULL,
    capabilities_json TEXT NOT NULL,
    load_json TEXT NOT NULL,
    status TEXT NOT NULL,
    registered_at TEXT NOT NULL,
    last_heartbeat TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS job_requirements (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id),
    requirements_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS job_leases (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id),
    worker_id TEXT NOT NULL REFERENCES workers(id),
    lease_token_hash TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    leased_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS artifact_locations (
    digest TEXT NOT NULL REFERENCES artifacts(digest),
    worker_id TEXT NOT NULL REFERENCES workers(id),
    updated_at TEXT NOT NULL,
    PRIMARY KEY(digest, worker_id)
);
CREATE TABLE IF NOT EXISTS artifact_transfers (
    id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(id),
    expected_digest TEXT NOT NULL,
    expected_size INTEGER NOT NULL,
    media_type TEXT NOT NULL,
    source_name TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    next_offset INTEGER NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS observation_captures (
    id TEXT PRIMARY KEY,
    target_id TEXT NOT NULL,
    source_id TEXT,
    adapter TEXT NOT NULL,
    adapter_version TEXT NOT NULL,
    normalized_request_json TEXT NOT NULL,
    environment_json TEXT NOT NULL,
    rights_decision TEXT NOT NULL,
    status TEXT NOT NULL,
    authority TEXT NOT NULL,
    manifest_digest TEXT REFERENCES artifacts(digest),
    summary_json TEXT NOT NULL,
    limitations_json TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS observation_capture_artifacts (
    capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    role TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    media_type TEXT NOT NULL,
    metadata_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(capture_id, role)
);
CREATE TABLE IF NOT EXISTS observation_events (
    capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    sequence INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    receipt_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    PRIMARY KEY(capture_id, sequence)
);
CREATE TABLE IF NOT EXISTS perceptual_graphs (
    id TEXT PRIMARY KEY,
    capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    graph_type TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    node_count INTEGER NOT NULL,
    edge_count INTEGER NOT NULL,
    authority TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(capture_id, graph_type)
);
CREATE TABLE IF NOT EXISTS design_drift_runs (
    id TEXT PRIMARY KEY,
    figma_capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    storybook_capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    bindings_json TEXT NOT NULL,
    status TEXT NOT NULL,
    report_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    UNIQUE(figma_capture_id, storybook_capture_id, bindings_json)
);
CREATE TABLE IF NOT EXISTS graphics_roundtrips (
    id TEXT PRIMARY KEY,
    capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    source_gltf_digest TEXT NOT NULL REFERENCES artifacts(digest),
    blend_digest TEXT NOT NULL REFERENCES artifacts(digest),
    output_glb_digest TEXT NOT NULL REFERENCES artifacts(digest),
    report_digest TEXT NOT NULL REFERENCES artifacts(digest),
    validation_status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(capture_id)
);
CREATE TABLE IF NOT EXISTS experience_ir_records (
    id TEXT PRIMARY KEY,
    capture_ids_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    authority TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS feature_capsules (
    id TEXT PRIMARY KEY,
    experience_ir_id TEXT NOT NULL REFERENCES experience_ir_records(id),
    kind TEXT NOT NULL,
    framework TEXT NOT NULL,
    status TEXT NOT NULL,
    manifest_digest TEXT NOT NULL REFERENCES artifacts(digest),
    test_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS capsule_evaluations (
    id TEXT PRIMARY KEY,
    capsule_id TEXT NOT NULL REFERENCES feature_capsules(id),
    status TEXT NOT NULL,
    report_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS perception_comparisons (
    id TEXT PRIMARY KEY,
    target_capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    candidate_capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    scope_json TEXT NOT NULL,
    status TEXT NOT NULL,
    score REAL NOT NULL,
    report_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    UNIQUE(target_capture_id, candidate_capture_id, scope_json)
);
CREATE TABLE IF NOT EXISTS frontend_candidate_portfolios (
    id TEXT PRIMARY KEY,
    target_capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    locality_json TEXT NOT NULL,
    thresholds_json TEXT NOT NULL,
    status TEXT NOT NULL,
    selected_candidate_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS frontend_candidates (
    id TEXT PRIMARY KEY,
    portfolio_id TEXT NOT NULL REFERENCES frontend_candidate_portfolios(id),
    capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    parameters_json TEXT NOT NULL,
    comparison_id TEXT REFERENCES perception_comparisons(id),
    score REAL,
    rank INTEGER,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS frontend_patch_proposals (
    id TEXT PRIMARY KEY,
    target_capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    candidate_capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    target_file TEXT NOT NULL,
    base_digest TEXT NOT NULL,
    result_digest TEXT NOT NULL REFERENCES artifacts(digest),
    patch_digest TEXT NOT NULL REFERENCES artifacts(digest),
    status TEXT NOT NULL,
    reviewer TEXT,
    reason TEXT,
    decision_digest TEXT REFERENCES artifacts(digest),
    applied_digest TEXT REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS frontend_global_gate_runs (
    id TEXT PRIMARY KEY,
    portfolio_id TEXT NOT NULL REFERENCES frontend_candidate_portfolios(id),
    candidate_id TEXT NOT NULL REFERENCES frontend_candidates(id),
    comparison_id TEXT NOT NULL REFERENCES perception_comparisons(id),
    status TEXT NOT NULL,
    report_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS perception_workspace_runs (
    id TEXT PRIMARY KEY,
    capture_ids_json TEXT NOT NULL,
    status TEXT NOT NULL,
    router_json TEXT NOT NULL,
    summary_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS perception_specialist_tasks (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES perception_workspace_runs(id),
    specialist TEXT NOT NULL,
    status TEXT NOT NULL,
    compute_units REAL NOT NULL,
    request_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(workspace_id, specialist)
);
CREATE TABLE IF NOT EXISTS perception_findings (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES perception_workspace_runs(id),
    task_id TEXT NOT NULL REFERENCES perception_specialist_tasks(id),
    specialist TEXT NOT NULL,
    kind TEXT NOT NULL,
    authority TEXT NOT NULL,
    confidence REAL NOT NULL,
    evidence_json TEXT NOT NULL,
    finding_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS perception_contradictions (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES perception_workspace_runs(id),
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    record_json TEXT NOT NULL,
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS perception_router_examples (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES perception_workspace_runs(id),
    features_json TEXT NOT NULL,
    selected_specialists_json TEXT NOT NULL,
    outcome_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS perception_router_benchmarks (
    id TEXT PRIMARY KEY,
    dataset_digest TEXT NOT NULL,
    status TEXT NOT NULL,
    active_router TEXT NOT NULL,
    report_json TEXT NOT NULL,
    report_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS media_reconstructions (
    id TEXT PRIMARY KEY,
    capture_id TEXT NOT NULL REFERENCES observation_captures(id),
    mode TEXT NOT NULL,
    status TEXT NOT NULL,
    record_digest TEXT NOT NULL REFERENCES artifacts(digest),
    created_at TEXT NOT NULL,
    UNIQUE(capture_id, mode)
);
CREATE TABLE IF NOT EXISTS model_approvals (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    source_url TEXT NOT NULL,
    expected_digest TEXT NOT NULL,
    license_json TEXT NOT NULL,
    approved_by TEXT NOT NULL,
    reason TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS model_installations (
    id TEXT PRIMARY KEY,
    approval_id TEXT NOT NULL REFERENCES model_approvals(id),
    artifact_digest TEXT NOT NULL REFERENCES artifacts(digest),
    revision TEXT NOT NULL,
    installed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS material_profiles (
    id TEXT PRIMARY KEY,
    region_id TEXT NOT NULL,
    component_id TEXT,
    material_slot TEXT,
    status TEXT NOT NULL,
    record_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
"""


def slugify(value: str) -> str:
    slug = re.sub(r"[^a-z0-9]+", "-", value.lower()).strip("-")
    if not slug:
        raise ProjectError("project name does not produce a valid slug")
    return slug


class ProjectStore:
    def __init__(self, root: Path):
        self.root = root.expanduser().resolve()
        self.project_file = self.root / "project.json"
        self.database_file = self.root / "project.db"

    @classmethod
    def create(
        cls,
        root: Path,
        name: str,
        *,
        target_fidelity: FidelityLevel = FidelityLevel.L3,
        metadata: dict[str, Any] | None = None,
    ) -> ProjectStore:
        store = cls(root)
        if store.project_file.exists() or store.database_file.exists():
            raise ProjectError(f"project already exists: {store.root}")
        store.root.mkdir(parents=True, exist_ok=True)
        for directory in PROJECT_DIRECTORIES:
            (store.root / directory).mkdir(parents=True, exist_ok=True)
        now = utc_now()
        project = {
            "schema_version": 1,
            "id": str(uuid.uuid4()),
            "name": name,
            "slug": slugify(name),
            "created_at": now,
            "updated_at": now,
            "canonical_units": "millimetres",
            "coordinate_system": {"handedness": "right", "up_axis": "Z"},
            "target_fidelity": target_fidelity.value,
            "accepted_fidelity": None,
            "metadata": metadata or {},
        }
        atomic_write_json(store.project_file, project)
        store.initialize_database()
        return store

    @classmethod
    def open(cls, root: Path) -> ProjectStore:
        store = cls(root)
        if not store.project_file.is_file() or not store.database_file.is_file():
            raise ProjectError(f"not a Blender Vision project: {store.root}")
        for directory in PROJECT_DIRECTORIES:
            (store.root / directory).mkdir(parents=True, exist_ok=True)
        store.initialize_database()
        return store

    def initialize_database(self) -> None:
        with self.connection() as connection:
            connection.executescript(SCHEMA)
            scene_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(scene_assets)")
            }
            if "state" not in scene_columns:
                connection.execute(
                    "ALTER TABLE scene_assets ADD COLUMN state TEXT NOT NULL DEFAULT 'DRAFT'"
                )
            if "is_authoritative" not in scene_columns:
                connection.execute(
                    "ALTER TABLE scene_assets ADD COLUMN is_authoritative "
                    "INTEGER NOT NULL DEFAULT 0"
                )
            mask_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(reference_masks)")
            }
            mask_migrations = {
                "creator": "TEXT NOT NULL DEFAULT 'unknown'",
                "backend": "TEXT NOT NULL DEFAULT 'human_manual'",
                "revision": "INTEGER NOT NULL DEFAULT 1",
                "approval_state": "TEXT NOT NULL DEFAULT 'approved'",
                "confidence": "TEXT NOT NULL DEFAULT 'high'",
                "intended_use": "TEXT NOT NULL DEFAULT 'silhouette_evaluation'",
                "visible_components_json": "TEXT NOT NULL DEFAULT '[]'",
                "excluded_components_json": "TEXT NOT NULL DEFAULT '[]'",
                "roi_json": "TEXT NOT NULL DEFAULT '{}'",
                "proposal_id": "TEXT REFERENCES reference_mask_proposals(id)",
                "decision_digest": "TEXT REFERENCES artifacts(digest)",
            }
            for column, declaration in mask_migrations.items():
                if column not in mask_columns:
                    connection.execute(
                        f"ALTER TABLE reference_masks ADD COLUMN {column} {declaration}"
                    )
            reference_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(reference_items)")
            }
            reference_role_migrated = False
            if "evidence_role" not in reference_columns:
                connection.execute(
                    "ALTER TABLE reference_items ADD COLUMN evidence_role TEXT NOT NULL "
                    "DEFAULT 'acceptance_reference'"
                )
                reference_role_migrated = True
            if "acceptance_eligible" not in reference_columns:
                connection.execute(
                    "ALTER TABLE reference_items ADD COLUMN acceptance_eligible INTEGER "
                    "NOT NULL DEFAULT 1"
                )
                reference_role_migrated = True
            if reference_role_migrated:
                connection.execute(
                    "UPDATE reference_items SET evidence_role='source_media',"
                    "acceptance_eligible=0 WHERE media_type NOT LIKE 'image/%'"
                )
                connection.execute(
                    "UPDATE reference_items SET evidence_role='diagnostic_video_frame',"
                    "acceptance_eligible=0 "
                    "WHERE json_extract(metadata_json,'$.video_source_reference_id') IS NOT NULL"
                )
            model_media_types = {
                ".blend": "application/x-blender",
                ".glb": "model/gltf-binary",
                ".gltf": "model/gltf+json",
                ".obj": "model/obj",
                ".ply": "model/ply",
                ".stl": "model/stl",
                ".usdz": "model/vnd.usdz+zip",
            }
            for suffix, media_type in model_media_types.items():
                connection.execute(
                    "UPDATE reference_items SET media_type=? "
                    "WHERE media_type='application/octet-stream' "
                    "AND lower(original_name) LIKE ?",
                    (media_type, f"%{suffix}"),
                )
            connection.execute(
                "UPDATE artifacts SET media_type=("
                "SELECT ri.media_type FROM reference_items ri "
                "WHERE ri.artifact_digest=artifacts.digest "
                "AND ri.media_type LIKE 'model/%' LIMIT 1) "
                "WHERE media_type='application/octet-stream' AND EXISTS("
                "SELECT 1 FROM reference_items ri WHERE ri.artifact_digest=artifacts.digest "
                "AND ri.media_type LIKE 'model/%')"
            )
            conflict_review_columns = {
                row["name"]
                for row in connection.execute("PRAGMA table_info(evidence_conflict_reviews)")
            }
            if "finding_sha256" not in conflict_review_columns:
                connection.execute(
                    "ALTER TABLE evidence_conflict_reviews ADD COLUMN finding_sha256 "
                    "TEXT NOT NULL DEFAULT ''"
                )
            component_fit_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(component_fits)")
            }
            if "decision_digest" not in component_fit_columns:
                connection.execute(
                    "ALTER TABLE component_fits ADD COLUMN decision_digest TEXT "
                    "REFERENCES artifacts(digest)"
                )
            optimization_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(optimization_runs)")
            }
            if "decision_digest" not in optimization_columns:
                connection.execute(
                    "ALTER TABLE optimization_runs ADD COLUMN decision_digest TEXT "
                    "REFERENCES artifacts(digest)"
                )
            camera_solution_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(camera_solutions)")
            }
            if "decision_id" not in camera_solution_columns:
                connection.execute("ALTER TABLE camera_solutions ADD COLUMN decision_id TEXT")
            if "decision_digest" not in camera_solution_columns:
                connection.execute(
                    "ALTER TABLE camera_solutions ADD COLUMN decision_digest TEXT "
                    "REFERENCES artifacts(digest)"
                )
            landmark_proposal_columns = {
                row["name"]
                for row in connection.execute("PRAGMA table_info(camera_landmark_proposals)")
            }
            landmark_proposal_migrations = {
                "superseded_by_id": ("TEXT REFERENCES camera_landmark_proposals(id)"),
                "supersession_digest": "TEXT REFERENCES artifacts(digest)",
            }
            for column, declaration in landmark_proposal_migrations.items():
                if column not in landmark_proposal_columns:
                    connection.execute(
                        f"ALTER TABLE camera_landmark_proposals ADD COLUMN {column} {declaration}"
                    )
            measurement_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(measurements)")
            }
            if "provenance_digest" not in measurement_columns:
                connection.execute(
                    "ALTER TABLE measurements ADD COLUMN provenance_digest TEXT "
                    "REFERENCES artifacts(digest)"
                )
            comparison_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(comparisons)")
            }
            comparison_migrations = {
                "receipt_digest": "TEXT REFERENCES artifacts(digest)",
                "superseded_by_id": "TEXT REFERENCES comparisons(id)",
                "supersession_digest": "TEXT REFERENCES artifacts(digest)",
            }
            for column, declaration in comparison_migrations.items():
                if column not in comparison_columns:
                    connection.execute(f"ALTER TABLE comparisons ADD COLUMN {column} {declaration}")
            pursuit_columns = {
                row["name"]
                for row in connection.execute("PRAGMA table_info(evidence_pursuit_runs)")
            }
            pursuit_migrations = {
                "lease_token": "TEXT",
                "lease_expires_at": "TEXT",
                "attempt_count": "INTEGER NOT NULL DEFAULT 0",
            }
            for column, declaration in pursuit_migrations.items():
                if column not in pursuit_columns:
                    connection.execute(
                        f"ALTER TABLE evidence_pursuit_runs ADD COLUMN {column} {declaration}"
                    )
            generative_request_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(generative_requests)")
            }
            generative_request_migrations = {
                "request_digest": "TEXT REFERENCES artifacts(digest)",
                "license_json": "TEXT NOT NULL DEFAULT '{}'",
                "cache_key": "TEXT",
                "job_id": "TEXT REFERENCES jobs(id)",
            }
            for column, declaration in generative_request_migrations.items():
                if column not in generative_request_columns:
                    connection.execute(
                        f"ALTER TABLE generative_requests ADD COLUMN {column} {declaration}"
                    )
            generative_result_columns = {
                row["name"] for row in connection.execute("PRAGMA table_info(generative_results)")
            }
            if "record_digest" not in generative_result_columns:
                connection.execute(
                    "ALTER TABLE generative_results ADD COLUMN record_digest TEXT "
                    "REFERENCES artifacts(digest)"
                )
            connection.execute(
                "CREATE UNIQUE INDEX IF NOT EXISTS generative_result_request "
                "ON generative_results(request_id)"
            )
            connection.execute(
                "CREATE UNIQUE INDEX IF NOT EXISTS generative_request_cache_key "
                "ON generative_requests(cache_key) WHERE cache_key IS NOT NULL"
            )
            authoritative = connection.execute(
                "SELECT id FROM scene_assets WHERE is_authoritative=1 LIMIT 1"
            ).fetchone()
            if authoritative is None:
                first = connection.execute(
                    "SELECT id FROM scene_assets WHERE state NOT IN ('REJECTED','SUPERSEDED') "
                    "ORDER BY created_at,id LIMIT 1"
                ).fetchone()
                if first:
                    connection.execute(
                        "UPDATE scene_assets SET is_authoritative=1 WHERE id=?", (first["id"],)
                    )

    @contextmanager
    def connection(self) -> Iterator[sqlite3.Connection]:
        self.root.mkdir(parents=True, exist_ok=True)
        connection = sqlite3.connect(self.database_file, timeout=30)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA journal_mode = WAL")
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA busy_timeout = 30000")
        try:
            yield connection
            connection.commit()
        except Exception:
            connection.rollback()
            raise
        finally:
            connection.close()

    def project(self) -> dict[str, Any]:
        try:
            return json.loads(self.project_file.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise ProjectError(f"invalid project metadata: {error}") from error

    def status(self) -> dict[str, Any]:
        with self.connection() as connection:
            counts = {
                table: connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
                for table in (
                    "artifacts",
                    "reference_items",
                    "reference_derivations",
                    "target_resolutions",
                    "target_resolution_events",
                    "evidence_sources",
                    "rights_ledger",
                    "evidence_source_governance_reviews",
                    "evidence_source_acquisitions",
                    "reference_adoption_proposals",
                    "search_providers",
                    "search_provider_reviews",
                    "search_discovery_runs",
                    "evidence_pursuit_runs",
                    "evidence_conflict_runs",
                    "evidence_conflict_reviews",
                    "evidence_duplicate_runs",
                    "reference_mask_proposals",
                    "reference_masks",
                    "scene_assets",
                    "scene_transitions",
                    "candidate_evaluations",
                    "jobs",
                    "job_provenance",
                    "camera_solutions",
                    "camera_decisions",
                    "camera_landmark_proposals",
                    "camera_refinement_runs",
                    "comparisons",
                    "coverage_reports",
                    "receipts",
                    "render_runs",
                    "exports",
                    "calibration_runs",
                    "measurements",
                    "measurement_grids",
                    "capture_requests",
                    "video_analysis_runs",
                    "feature_tracks",
                    "tier_reviews",
                    "features",
                    "components",
                    "semantic_nodes",
                    "semantic_edges",
                    "reconstruction_portfolios",
                    "reconstruction_candidates",
                    "repair_proposals",
                    "geometry_runs",
                    "geometry_consensus",
                    "camera_consensus",
                    "component_fits",
                    "datasets",
                    "training_runs",
                    "model_evaluations",
                    "visual_oracles",
                    "generative_requests",
                    "generative_results",
                    "optimization_runs",
                    "multiview_search_runs",
                    "multiview_search_candidates",
                    "campaigns",
                    "campaign_events",
                    "agent_proposals",
                    "role_tasks",
                    "context_packets",
                    "synthetic_views",
                    "active_learning_cycles",
                    "active_learning_events",
                    "active_model_revisions",
                    "surface_coverage_cells",
                    "warm_services",
                    "benchmark_policy_reviews",
                    "beast_benchmark_runs",
                    "workers",
                    "job_leases",
                    "model_approvals",
                    "model_installations",
                    "material_profiles",
                    "visual_baseline_freezes",
                    "semantic_geometry_bindings",
                    "visual_component_packets",
                    "visual_frequency_scorecards",
                    "visual_defect_diagnoses",
                )
            }
            job_counts = {
                row["status"]: row["count"]
                for row in connection.execute(
                    "SELECT status, COUNT(*) AS count FROM jobs GROUP BY status"
                ).fetchall()
            }
        return {"project": self.project(), "counts": counts, "jobs": job_counts}

    def add_job(self, operation: str, config: dict[str, Any], cache_key: str | None) -> str:
        job_id = str(uuid.uuid4())
        with self.connection() as connection:
            connection.execute(
                "INSERT INTO jobs"
                "(id, operation, status, cache_key, config_json, created_at) "
                "VALUES(?,?,?,?,?,?)",
                (
                    job_id,
                    operation,
                    JobStatus.QUEUED.value,
                    cache_key,
                    json.dumps(config),
                    utc_now(),
                ),
            )
        self.add_job_event(job_id, "queued", {"operation": operation})
        return job_id

    def record_job_provenance(
        self,
        job_id: str,
        *,
        input_hashes: list[str] | None = None,
        backend: dict[str, Any] | None = None,
        execution: dict[str, Any] | None = None,
        output_hashes: list[str] | None = None,
        metrics: dict[str, Any] | None = None,
        logs: list[str] | None = None,
        failure_class: str | None = None,
    ) -> None:
        now = utc_now()
        with self.connection() as connection:
            row = connection.execute(
                "SELECT * FROM job_provenance WHERE job_id=?", (job_id,)
            ).fetchone()
            current = dict(row) if row else {}
            values = {
                "input_hashes_json": json.dumps(
                    input_hashes
                    if input_hashes is not None
                    else json.loads(current.get("input_hashes_json", "[]"))
                ),
                "backend_json": json.dumps(
                    backend
                    if backend is not None
                    else json.loads(current.get("backend_json", "{}"))
                ),
                "execution_json": json.dumps(
                    execution
                    if execution is not None
                    else json.loads(current.get("execution_json", "{}"))
                ),
                "output_hashes_json": json.dumps(
                    output_hashes
                    if output_hashes is not None
                    else json.loads(current.get("output_hashes_json", "[]"))
                ),
                "metrics_json": json.dumps(
                    metrics
                    if metrics is not None
                    else json.loads(current.get("metrics_json", "{}"))
                ),
                "logs_json": json.dumps(
                    logs if logs is not None else json.loads(current.get("logs_json", "[]"))
                ),
                "failure_class": (
                    failure_class if failure_class is not None else current.get("failure_class")
                ),
            }
            connection.execute(
                "INSERT INTO job_provenance("
                "job_id,input_hashes_json,backend_json,execution_json,output_hashes_json,"
                "metrics_json,logs_json,failure_class,updated_at) VALUES(?,?,?,?,?,?,?,?,?) "
                "ON CONFLICT(job_id) DO UPDATE SET input_hashes_json=excluded.input_hashes_json,"
                "backend_json=excluded.backend_json,execution_json=excluded.execution_json,"
                "output_hashes_json=excluded.output_hashes_json,metrics_json=excluded.metrics_json,"
                "logs_json=excluded.logs_json,failure_class=excluded.failure_class,"
                "updated_at=excluded.updated_at",
                (
                    job_id,
                    values["input_hashes_json"],
                    values["backend_json"],
                    values["execution_json"],
                    values["output_hashes_json"],
                    values["metrics_json"],
                    values["logs_json"],
                    values["failure_class"],
                    now,
                ),
            )

    def add_job_event(self, job_id: str, event: str, payload: dict[str, Any]) -> None:
        with self.connection() as connection:
            connection.execute(
                "INSERT INTO job_events(job_id, event, payload_json, created_at) VALUES(?,?,?,?)",
                (job_id, event, json.dumps(payload), utc_now()),
            )

    def update_job(
        self,
        job_id: str,
        status: JobStatus,
        *,
        result: dict[str, Any] | None = None,
        error: dict[str, Any] | None = None,
    ) -> None:
        timestamp_column = "started_at" if status == JobStatus.RUNNING else "finished_at"
        with self.connection() as connection:
            connection.execute(
                f"UPDATE jobs SET status=?, result_json=?, error_json=?, "
                f"{timestamp_column}=? WHERE id=?",
                (
                    status.value,
                    json.dumps(result) if result is not None else None,
                    json.dumps(error) if error is not None else None,
                    utc_now(),
                    job_id,
                ),
            )
        self.add_job_event(job_id, status.value, result or error or {})

    def job(self, job_id: str) -> dict[str, Any]:
        with self.connection() as connection:
            row = connection.execute("SELECT * FROM jobs WHERE id=?", (job_id,)).fetchone()
            if row is None:
                raise ProjectError(f"unknown job: {job_id}")
            events = connection.execute(
                "SELECT event, payload_json, created_at FROM job_events "
                "WHERE job_id=? ORDER BY sequence",
                (job_id,),
            ).fetchall()
        value = dict(row)
        for key in ("config_json", "result_json", "error_json"):
            value[key.removesuffix("_json")] = json.loads(value.pop(key)) if value[key] else None
        value["cancel_requested"] = bool(value["cancel_requested"])
        value["events"] = [
            {
                "event": row["event"],
                "payload": json.loads(row["payload_json"]),
                "created_at": row["created_at"],
            }
            for row in events
        ]
        return value

    def request_cancel(self, job_id: str) -> None:
        with self.connection() as connection:
            result = connection.execute(
                "UPDATE jobs SET cancel_requested=1 WHERE id=? AND status IN (?, ?)",
                (job_id, JobStatus.QUEUED.value, JobStatus.RUNNING.value),
            )
            if result.rowcount == 0:
                raise ProjectError(f"job cannot be cancelled: {job_id}")
        self.add_job_event(job_id, "cancel_requested", {})

    def list_jobs(self, *, limit: int = 100) -> list[dict[str, Any]]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM jobs ORDER BY created_at DESC LIMIT ?", (max(1, min(limit, 1000)),)
            ).fetchall()
        jobs: list[dict[str, Any]] = []
        for row in rows:
            value = dict(row)
            for key in ("config_json", "result_json", "error_json"):
                value[key.removesuffix("_json")] = (
                    json.loads(value.pop(key)) if value[key] else None
                )
            value["cancel_requested"] = bool(value["cancel_requested"])
            jobs.append(value)
        return jobs

    def claim_next_job(self) -> str | None:
        """Atomically claim the oldest queued job for a local worker."""
        with self.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            row = connection.execute(
                "SELECT id, cancel_requested FROM jobs WHERE status=? ORDER BY created_at LIMIT 1",
                (JobStatus.QUEUED.value,),
            ).fetchone()
            if row is None:
                return None
            if row["cancel_requested"]:
                connection.execute(
                    "UPDATE jobs SET status=?, finished_at=? WHERE id=?",
                    (JobStatus.CANCELLED.value, utc_now(), row["id"]),
                )
                return None
            connection.execute(
                "UPDATE jobs SET status=?, started_at=? WHERE id=? AND status=?",
                (JobStatus.RUNNING.value, utc_now(), row["id"], JobStatus.QUEUED.value),
            )
            return str(row["id"])

    def cancellation_requested(self, job_id: str) -> bool:
        with self.connection() as connection:
            row = connection.execute(
                "SELECT cancel_requested FROM jobs WHERE id=?", (job_id,)
            ).fetchone()
        return bool(row and row[0])

    def cached(self, cache_key: str) -> dict[str, Any] | None:
        with self.connection() as connection:
            row = connection.execute(
                "SELECT result_json FROM cache_entries WHERE cache_key=?", (cache_key,)
            ).fetchone()
        return json.loads(row[0]) if row else None

    def put_cache(self, cache_key: str, operation: str, result: dict[str, Any]) -> None:
        with self.connection() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO cache_entries"
                "(cache_key, operation, result_json, created_at) VALUES(?,?,?,?)",
                (cache_key, operation, json.dumps(result), utc_now()),
            )

    def cache_entries(self, *, limit: int = 100) -> list[dict[str, Any]]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT cache_key, operation, result_json, created_at FROM cache_entries "
                "ORDER BY created_at DESC LIMIT ?",
                (max(1, min(limit, 1000)),),
            ).fetchall()
        return [
            {
                "cache_key": row["cache_key"],
                "operation": row["operation"],
                "result": json.loads(row["result_json"]),
                "created_at": row["created_at"],
            }
            for row in rows
        ]
