from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any, Protocol

ArtifactSink = Callable[[str, bytes, str, dict[str, Any] | None], dict[str, Any]]


@dataclass(slots=True)
class CaptureOutcome:
    """The adapter's evidence summary after all artifacts have reached the sink."""

    summary: dict[str, Any] = field(default_factory=dict)
    limitations: list[str] = field(default_factory=list)
    graphs: list[dict[str, Any]] = field(default_factory=list)


class SensorAdapter(Protocol):
    """Strict boundary implemented by every perceptual sensor."""

    name: str
    version: str

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        """Return a stable, JSON-serializable target."""

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        """Validate and fill all environment-affecting capture defaults."""

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        """Describe the concrete sensor environment used in the capture identity."""

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        """Capture evidence and stream every artifact to the durable sink."""
