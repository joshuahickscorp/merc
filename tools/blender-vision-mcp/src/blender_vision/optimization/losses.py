from __future__ import annotations

from dataclasses import dataclass, fields


@dataclass(frozen=True, slots=True)
class LossWeights:
    silhouette: float = 1.0
    feature: float = 1.0
    depth: float = 1.0
    normal: float = 1.0
    measurement: float = 1.0
    constraint: float = 1.0
    complexity: float = 0.01

    def __post_init__(self) -> None:
        if any(getattr(self, item.name) < 0 for item in fields(self)):
            raise ValueError("loss weights cannot be negative")


@dataclass(frozen=True, slots=True)
class LossTerms:
    silhouette: float = 0.0
    feature: float = 0.0
    depth: float = 0.0
    normal: float = 0.0
    measurement: float = 0.0
    constraint: float = 0.0
    complexity: float = 0.0


def weighted_loss(terms: LossTerms, weights: LossWeights) -> float:
    return sum(getattr(terms, item.name) * getattr(weights, item.name) for item in fields(terms))
