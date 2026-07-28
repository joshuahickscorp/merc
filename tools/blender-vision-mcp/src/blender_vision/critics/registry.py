"""Registry of the thirteen perceptual specialist critics."""

from __future__ import annotations

from blender_vision.critics.accessibility_reviewer import AccessibilityReviewerCritic
from blender_vision.critics.adversarial_acceptance_reviewer import (
    AdversarialAcceptanceReviewerCritic,
)
from blender_vision.critics.base import Critic, CriticRole
from blender_vision.critics.cinematographer import CinematographerCritic
from blender_vision.critics.editorial_art_director import EditorialArtDirectorCritic
from blender_vision.critics.environment_artist import EnvironmentArtistCritic
from blender_vision.critics.groom_artist import GroomArtistCritic
from blender_vision.critics.industrial_designer import IndustrialDesignerCritic
from blender_vision.critics.interaction_designer import InteractionDesignerCritic
from blender_vision.critics.lighting_artist import LightingArtistCritic
from blender_vision.critics.material_artist import MaterialArtistCritic
from blender_vision.critics.organic_artist import OrganicArtistCritic
from blender_vision.critics.performance_engineer import PerformanceEngineerCritic
from blender_vision.critics.product_photographer import ProductPhotographerCritic


def default_critics() -> list[Critic]:
    return [
        ProductPhotographerCritic(),
        CinematographerCritic(),
        IndustrialDesignerCritic(),
        EnvironmentArtistCritic(),
        MaterialArtistCritic(),
        LightingArtistCritic(),
        OrganicArtistCritic(),
        GroomArtistCritic(),
        EditorialArtDirectorCritic(),
        InteractionDesignerCritic(),
        AccessibilityReviewerCritic(),
        PerformanceEngineerCritic(),
        AdversarialAcceptanceReviewerCritic(),
    ]


def critic_by_role(role: CriticRole | str) -> Critic:
    target = CriticRole(role)
    for critic in default_critics():
        if critic.role is target:
            return critic
    raise KeyError(f"unknown critic role: {role}")
