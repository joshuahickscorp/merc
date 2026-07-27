"""Inverse lighting: rigs, solve, joint material/light optimisation, critics."""

from blender_vision.lighting.critic import (
    LIGHTING_CRITICS,
    LightingCriticContext,
    inject_lighting_failure,
    run_lighting_critics,
)
from blender_vision.lighting.joint import JointSolveResult, joint_solve
from blender_vision.lighting.rigs import (
    RIG_NAMES,
    LightingRig,
    apply_rig_script,
    get_rig,
    list_rigs,
)
from blender_vision.lighting.solve import (
    GeometryContext,
    LightingObservation,
    solve_lighting,
)

__all__ = [
    "LIGHTING_CRITICS",
    "RIG_NAMES",
    "GeometryContext",
    "JointSolveResult",
    "LightingCriticContext",
    "LightingObservation",
    "LightingRig",
    "apply_rig_script",
    "get_rig",
    "inject_lighting_failure",
    "joint_solve",
    "list_rigs",
    "run_lighting_critics",
    "solve_lighting",
]
