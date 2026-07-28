# ExperienceIR and clean-room Feature Capsules

`ExperienceIRCompiler` consumes only verified, content-addressed perceptual graphs. It compacts
layout constraints, observed states and transitions, responsive rules, input causality, motion
tracks and replay contracts, explicit graphics runtime records, design components/tokens, and
accessibility citations into one derived ExperienceIR. Raw screenshots, HTML, design-source JSON,
and protected assets are not embedded.

`vision.transplant_feature` compiles that IR into a framework-targeted Feature Capsule. A capsule
contains semantic purpose, clean-room behavior, evidence digests, owned component/token mappings,
accessibility behavior, performance budget, implementation interface, generated replay tests,
thresholds, limitations, and source-governance restrictions.

Framework emitters currently produce a minimal integration contract for React, vanilla web,
Three.js, React Three Fiber, Storybook, and Blender previs. They are generated from the normalized
behavior contract; reference source is never copied.

Asset substitution is deny-by-default. Every shipped mapping needs an exact replacement digest and
an `OWNED`, `LICENSED`, `SYNTHETIC_OWNED`, `INTERNAL_AUTHORIZED`, or `CC0` rights state. Public
visibility is deliberately insufficient.

`vision.evaluate` and `vision.verify(capsule_id=...)` re-hash the capsule and generated tests,
reapply the clean-room scan, and require coverage of every captured state, interaction, responsive
viewport, motion track, reduced-motion variant, and the mandatory global non-regression gate.
Tampering moves the capsule to `REJECTED`; failed evidence remains in the artifact store.
