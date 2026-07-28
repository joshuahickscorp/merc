from __future__ import annotations

import json
from pathlib import Path

FacetSeed = tuple[str, int, str]

APP: list[FacetSeed] = [
    ("Webpage screenshot capture", 96, "Real Chromium fixture proven"),
    ("DOM capture", 94, "CDP-backed structural capture"),
    ("Accessibility-tree capture", 92, "Strong semantic evidence"),
    ("Computed-style capture", 94, "Layout and style evidence retained"),
    ("Font and CSS inspection", 91, "Useful for faithful reconstruction"),
    ("Asset inventory", 90, "Includes source and rights handling"),
    ("Network/runtime observation", 90, "Network, console and performance evidence"),
    ("Canvas/WebGL inspection", 86, "Real WebGL2 fixture tested"),
    ("WebGPU inspection", 58, "Instrumentation exists; no capable hardware test"),
    ("Responsive breakpoint discovery", 88, "Governed sweeps and responsive graph"),
    ("Interaction-state discovery", 87, "Hover, focus, pressed, pointer, keyboard, touch"),
    ("Bounded state crawling", 86, "Good, but cannot guarantee every hidden state"),
    ("Animation sampling", 90, "CSS/WAAPI, scrolling, sticky and reduced-motion cases"),
    ("Behavioral reconstruction", 85, "Strong ExperienceIR and Feature Capsule design"),
    (
        "Visual component decomposition",
        80,
        "Good graph structure; arbitrary pages remain difficult",
    ),
    ("Design-token extraction", 84, "Tokens and drift represented"),
    ("Component/variant inference", 79, "Fixture-proven; production ambiguity remains"),
    ("Figma export ingestion", 76, "File-based governed export only"),
    ("Live authenticated Figma", 45, "Not demonstrated"),
    ("Storybook ingestion", 77, "Export-based fixture proven"),
    ("Production Storybook integration", 55, "Not externally demonstrated"),
    ("Repository understanding", 81, "Files, symbols, routes, hooks, stores and assets"),
    (
        "JavaScript/TypeScript semantic precision",
        70,
        "Static patterns, not compiler/LSP-grade resolution",
    ),
    ("Source-to-pixel tracing", 83, "Strong architecture and governed fixture evidence"),
    ("Visual blast-radius analysis", 84, "Useful for limiting changes"),
    ("CSS repair search", 88, "Locality-constrained candidates with global gate"),
    ("Regression rejection", 93, "Rejects locally good and globally bad candidates"),
    ("Atomic repair application", 91, "Base digest, backup, confinement and review"),
    ("Failed-attempt preservation", 96, "Excellent auditability"),
    ("Clean-room feature transplant", 88, "Prevents copying raw protected source"),
    ("Unauthorized-asset rejection", 94, "Strong policy enforcement"),
    ("Screenshot to editable frontend", 75, "Candidate generation, not universal pixel perfection"),
    (
        "Video to interface understanding",
        71,
        "Narrative graphs and motion; limited semantic interpretation",
    ),
    ("Desktop screenshot to interface", 69, "Synthetic snapshot tested, not broad production UI"),
    ("Visual and text fusion", 74, "Useful target input, but no mature intent compiler"),
    ("Text-only frontend generation", 62, "Not the system's demonstrated strength"),
    (
        "Complete frontend scaffolding",
        68,
        "Contracts exist; full generator is not comprehensively proven",
    ),
    ("Backend generation", 38, "Database, APIs, jobs and business rules not yet compiled"),
    (
        "Authentication/authorization buildout",
        32,
        "No complete application-auth construction benchmark",
    ),
    (
        "Data-model inference",
        39,
        "Visual references cannot establish authoritative backend semantics",
    ),
    (
        "Automated accessibility remediation",
        74,
        "Strong capture; full remediation coverage not demonstrated",
    ),
    ("Performance optimization", 72, "Measurement exists; autonomous optimization is limited"),
    ("Chromium production confidence", 87, "Strongest supported browser"),
    ("Safari/Firefox parity", 54, "Not claimed or demonstrated"),
    ("Mobile-browser/device fidelity", 62, "Simulation stronger than physical-device evidence"),
    ("Application test generation", 84, "Feature capsules can generate tests"),
    ("End-to-end application acceptance", 81, "Strong gates where authoritative references exist"),
    ("Deployment/hosting", 35, "Not part of the proven build pipeline"),
    (
        "Arbitrary production-site generalization",
        66,
        "Controlled fixtures stronger than external proof",
    ),
    (
        "Fully autonomous application delivery",
        64,
        "Human decisions remain for requirements, backend and acceptance",
    ),
]

THREE_D: list[FacetSeed] = [
    ("Reference-image ingestion", 95, "Immutable, governed and traceable"),
    ("Video-frame triage", 88, "Actual ffmpeg-backed processing"),
    ("Reference provenance/rights", 96, "Among the strongest system capabilities"),
    ("Target identity resolution", 84, "Good grounding and ambiguity handling"),
    ("Measurement extraction", 84, "Strong with calibrated references"),
    ("Unit/scale handling", 88, "Strict synthetic metric benchmark passed"),
    ("Camera calibration", 86, "Strong controlled evidence"),
    ("Camera refinement", 84, "Multiple refinement and consensus mechanisms"),
    ("Arbitrary-photo camera recovery", 68, "Depends heavily on landmarks and reference quality"),
    ("Multiview consensus", 83, "Architecture and controlled tests are strong"),
    ("Landmark management", 85, "Explicit and governed"),
    ("Mask/silhouette processing", 88, "Useful comparison foundation"),
    ("Coverage analysis", 91, "Explicitly identifies unseen geometry"),
    ("Hidden-geometry discipline", 97, "Refuses to call unseen surfaces verified"),
    ("Semantic part decomposition", 83, "Particularly suitable for product objects"),
    ("Parametric component generation", 84, "Strong modeling foundation"),
    ("Hard-surface product modeling", 82, "Best native modeling category"),
    ("Mechanical detail reconstruction", 75, "Sensitive to resolution and measurements"),
    ("Organic-form reconstruction", 53, "Not comprehensively supported"),
    ("Character modeling", 35, "No serious demonstrated character pipeline"),
    ("Sculptural anatomy", 30, "No demonstrated anatomy or sculpting authority"),
    (
        "Mesh topology quality",
        74,
        "Editable output proven; optimal topology not broadly benchmarked",
    ),
    ("Retopology", 61, "Not a mature demonstrated specialty"),
    ("UV generation", 58, "Not strongly evidenced"),
    ("Texture reconstruction", 65, "Material evidence exists; exact recovery remains weak"),
    ("PBR material construction", 73, "Governed materials with limited universal fidelity proof"),
    ("Material identification", 69, "Visual ambiguity remains substantial"),
    ("Lighting estimation", 66, "No universal calibrated inverse-lighting solution"),
    ("Reflection/highlight matching", 61, "Difficult unresolved inverse-rendering problem"),
    ("Environment reconstruction", 64, "Partial evidence and proposal capability"),
    ("Scene organization", 86, "Blender artifacts remain editable and structured"),
    ("Browser WebGL scene inspection", 87, "Buffers, shaders, textures and draw metadata"),
    ("Explicit scene-hook extraction", 92, "Excellent where the owned hook is present"),
    ("Arbitrary WebGL scene recovery", 72, "Obfuscated and custom engines remain difficult"),
    ("Browser scene to glTF", 89, "Runtime compiler demonstrated"),
    ("glTF to editable Blender", 94, "Real Blender import succeeded"),
    ("Blender to GLB export", 94, "Real export and reimport succeeded"),
    ("GLB structural validation", 92, "Strong artifact verification"),
    ("Round-trip editability", 92, "One of the clearest production-ready capabilities"),
    ("Single-image depth", 46, "No licensed production model backend"),
    ("Sensor-depth ingestion", 79, "Properly governed when real depth is supplied"),
    ("Model-derived depth governance", 88, "Correct observed versus derived distinction"),
    ("Single-image complete geometry", 38, "Hidden geometry cannot be authoritatively recovered"),
    ("Ordinary photo-set to product model", 69, "Plausible candidates, not assured fidelity"),
    ("Calibrated multiview to product model", 82, "Strongest photo-based workflow"),
    ("Video to static 3D model", 66, "Camera and global motion only partly calibrated"),
    ("Browser runtime to 3D model", 91, "Best end-to-end 3D workflow"),
    ("Text-only 3D generation", 55, "No production-quality general text-to-geometry benchmark"),
    ("Visual and text 3D generation", 70, "Useful for constrained targets, not arbitrary assets"),
    ("Dimensional accuracy", 82, "Only when metric calibration and measurements exist"),
    ("Manufacturing/CAD authority", 64, "Not equivalent to tolerance-certified CAD"),
    ("Visual equivalence evaluation", 78, "Strong architecture; target-wide proof incomplete"),
    ("Fixed-camera equivalence", 75, "Remaining camera and material residuals are explicit"),
    ("3D repair search", 76, "Governed lanes exist; broad target evidence is limited"),
    ("Candidate portfolio generation", 83, "Good uncertainty-preserving approach"),
    ("Uncertainty reporting", 96, "Properly labels hypotheses and candidates"),
    ("Rigging", 32, "Not demonstrated"),
    ("Character animation", 28, "Not demonstrated"),
    ("Object animation reconstruction", 61, "Runtime evidence with limited Blender proof"),
    ("Simulation/physics recreation", 30, "Not demonstrated"),
    ("LOD/performance preparation", 70, "Not a complete game-asset pipeline"),
    ("Game-engine-ready asset delivery", 68, "Topology, UV, LOD and collision remain incomplete"),
    ("Arbitrary production-object generalization", 65, "No external physical multiview benchmark"),
    ("Autonomous finished 3D delivery", 63, "Human review remains essential"),
]

SYSTEM: list[FacetSeed] = [
    ("Evidence traceability", 97, "Immutable references and cited artifacts"),
    ("Authority labeling", 98, "Observed, derived and hypothetical states remain distinct"),
    ("Provenance and licensing", 96, "Strong source and backend governance"),
    ("Deterministic reproduction", 94, "Strong fixture and artifact reproducibility"),
    ("Artifact tamper detection", 95, "Content-addressed verification"),
    ("Capture resumption/idempotency", 93, "Interruption-safe and idempotent capture"),
    ("Security/isolation defaults", 91, "Private networking disabled and allowlisted by default"),
    ("Secret redaction", 91, "Governed URL and payload redaction"),
    ("Candidate transaction safety", 94, "Atomic promotion and rollback discipline"),
    ("Global regression gates", 93, "Local repairs cannot bypass global acceptance"),
    ("MCP tool surface completeness", 92, "Broad stable public tool surface"),
    ("Query/explanation interface", 88, "Unified graph query and explanation"),
    ("Specialist workspace architecture", 90, "Sixteen evidence-oriented specialists"),
    ("Contradiction tracking", 94, "Contradictions and missing observations remain explicit"),
    ("Compute accounting", 88, "Per-task and specialist accounting"),
    ("Deterministic routing", 86, "Stable and benchmarked"),
    ("Learned routing", 65, "Learned candidate correctly refuted"),
    ("Active-learning governance", 86, "Fixed evaluation and rollback gates"),
    ("Actual trained production model", 45, "No production specialist checkpoint trained"),
    ("Distributed protocol", 84, "Authenticated leases and digest-verified artifacts"),
    ("Physical heterogeneous deployment", 57, "Not executed across physical hosts"),
    ("Automated test coverage", 94, "Strong fast and governed integration suite"),
    ("Real-runtime testing", 87, "Real Chrome and Blender lanes"),
    ("Packaging/release reproducibility", 94, "Clean wheel and source archive verification"),
    (
        "External production validation",
        62,
        "Owned fixtures stronger than external production evidence",
    ),
]


def slug(value: str) -> str:
    output = []
    for character in value.lower():
        if character.isalnum():
            output.append(character)
        elif not output or output[-1] != "_":
            output.append("_")
    return "".join(output).strip("_")


def grade(score: int) -> str:
    boundaries = (
        (93, "A"),
        (90, "A-"),
        (87, "B+"),
        (83, "B"),
        (80, "B-"),
        (77, "C+"),
        (73, "C"),
        (70, "C-"),
        (67, "D+"),
        (63, "D"),
        (60, "D-"),
        (0, "F"),
    )
    return next(label for minimum, label in boundaries if score >= minimum)


def runtimes(domain: str, name: str) -> list[str]:
    lowered = name.lower()
    if domain == "app":
        values = ["chromium", "node"]
        if any(
            token in lowered for token in ("backend", "data-model", "authentication", "deployment")
        ):
            values.extend(["database", "container"])
        if "safari/firefox" in lowered:
            values.extend(["firefox", "webkit"])
        if "webgpu" in lowered:
            values.append("webgpu_hardware")
        return sorted(set(values))
    if domain == "3d":
        values = ["blender"]
        if "browser" in lowered or "webgl" in lowered:
            values.append("chromium")
        if "single-image depth" in lowered:
            values.append("licensed_depth_backend")
        return sorted(set(values))
    values = ["python"]
    if "physical heterogeneous" in lowered:
        values.append("second_physical_host")
    return sorted(set(values))


def metrics(domain: str, name: str) -> dict[str, dict[str, object]]:
    values: dict[str, dict[str, object]] = {
        "acceptance_gate_pass_rate": {"op": "==", "value": 1.0},
        "p0_defects": {"op": "==", "value": 0},
        "p1_defects": {"op": "==", "value": 0},
    }
    lowered = name.lower()
    if "dimensional" in lowered:
        values["dimension_error_percent"] = {"op": "<=", "value": 1.0}
    if "silhouette" in lowered:
        values["heldout_silhouette_iou"] = {"op": ">=", "value": 0.92}
    if "performance" in lowered:
        values["global_regressions"] = {"op": "==", "value": 0}
    if domain == "system" and "tamper" in lowered:
        values["tamper_cases_rejected"] = {"op": ">=", "value": 1}
    return values


def build(domain: str, rows: list[FacetSeed]) -> list[dict[str, object]]:
    result = []
    prefix = "app" if domain == "app" else "3d" if domain == "3d" else "system"
    for name, score, assessment in rows:
        facet_id = f"{prefix}.{slug(name)}"
        reference_class = {
            "app": [
                "complete_application_reference_packet",
                "owned_or_authorized_visual_reference",
            ],
            "3d": ["declared_geometry_reference_class", "owned_or_authorized_multimodal_reference"],
            "system": ["fixed_immutable_benchmark_corpus"],
        }[domain]
        result.append(
            {
                "id": facet_id,
                "domain": domain,
                "name": name,
                "baseline_score": score,
                "baseline_grade": grade(score),
                "baseline_assessment": assessment,
                "required_reference_class": reference_class,
                "required_real_runtimes": runtimes(domain, name),
                "required_external_or_holdout_tests": [f"heldout.{facet_id}"],
                "required_metrics": metrics(domain, name),
                "required_receipts": [f"{facet_id}.acceptance_receipt"],
                "known_blockers": [assessment] if score < 60 else [],
                "current_score": score,
                "status": "PROVEN_BELOW_100",
                "evidence": [],
                "reproduction_commands": [],
            }
        )
    return result


def main() -> None:
    destination = Path(__file__).parents[1] / "benchmarks" / "100_plus" / "original_facets.json"
    document = {
        "schema_version": "1",
        "generated_from": "VisionMCP capability grading report preserved 2026-07-25",
        "facets": build("app", APP) + build("3d", THREE_D) + build("system", SYSTEM),
    }
    destination.write_text(
        json.dumps(document, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    print(f"wrote {len(document['facets'])} facets to {destination}")


if __name__ == "__main__":
    main()
