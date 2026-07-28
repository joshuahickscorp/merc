# Governed design and component intelligence

VisionMCP normalizes caller-supplied Figma JSON exports and Storybook index/manifest JSON into the
same evidence-linked `DesignSystemGraph`. These adapters do not use ambient credentials and do not
contact private design accounts.

The Figma adapter preserves hierarchy, component and component-set identity, instance bindings,
variants, auto-layout fields, appearance, typography, styles, variables, plugin/source bindings,
and optional caller-supplied rendered evidence. The Storybook adapter preserves component/story
hierarchy, args, controls, tags, import/export symbols, framework identity, and exported tokens.
Every node cites the immutable source artifact.

`vision.observe` accepts `design.figma_export` and `design.storybook_export` as adapter names with a
typed file target. `vision.compare` binds components by normalized semantic identity or an explicit
binding map, then emits a content-addressed drift report covering:

- missing or one-off components;
- missing component variants/states;
- token value or presence drift;
- exact Figma-to-Storybook component bindings;
- the source graph artifact citations used for every conclusion.

The owned export fixture proves traceable reuse for Button and Card, exact token agreement, and
both required Button variants. A deliberately degraded Storybook export is rejected for a missing
secondary variant and a changed action-color token.

This tranche proves structured export ingestion and drift detection. Live Figma API access,
Storybook play-function execution, and pixel parity against a production design screen require an
explicitly authorized account/session and remain separate benchmark claims.
