from __future__ import annotations

import json
from pathlib import Path

from blender_vision.app_build.specification import (
    AcceptanceTestGraph,
    APIContractGraph,
    ApplicationReferencePacket,
    AuthPolicyGraph,
    BusinessRuleGraph,
    DataModelGraph,
    DeploymentGraph,
    ObservabilityGraph,
    ProductSpecIR,
    ReferenceCompletenessReport,
    UserJourneyGraph,
)

MODELS = {
    "product-spec-ir.schema.json": ProductSpecIR,
    "user-journey-graph.schema.json": UserJourneyGraph,
    "data-model-graph.schema.json": DataModelGraph,
    "api-contract-graph.schema.json": APIContractGraph,
    "auth-policy-graph.schema.json": AuthPolicyGraph,
    "business-rule-graph.schema.json": BusinessRuleGraph,
    "deployment-graph.schema.json": DeploymentGraph,
    "observability-graph.schema.json": ObservabilityGraph,
    "acceptance-test-graph.schema.json": AcceptanceTestGraph,
    "application-reference-packet.schema.json": ApplicationReferencePacket,
    "reference-completeness-report.schema.json": ReferenceCompletenessReport,
}


def main() -> None:
    destination = Path(__file__).parents[1] / "schemas"
    for filename, model in MODELS.items():
        schema = model.model_json_schema()
        schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
        schema["$id"] = f"https://compute.exchange/schemas/{filename}"
        (destination / filename).write_text(
            json.dumps(schema, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    print(f"wrote {len(MODELS)} application authority schemas to {destination}")


if __name__ == "__main__":
    main()
