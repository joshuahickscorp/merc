# Code perception and the shared workspace

`code.repository` takes a bounded, content-addressed snapshot of caller-authorized
source without executing it or installing dependencies. Python declarations use
the standard AST; JavaScript, TypeScript, CSS, markup, shader, configuration, and
asset relationships use explicitly labeled static patterns. The resulting
`CodeGraph` links files, symbols, components, hooks, routes, stores, selectors,
tokens, assets, tests, stories, shaders, and build configuration.

Governed runtime instrumentation can add explicit visual-node-to-source bindings.
`vision.explain_region` accepts a source capture and follows a visual region into
those bindings and selector origins. A `vision.query` request with
`operation=visual_blast_radius` computes the local components, states, viewports,
screenshots, animations, 3D passes, tests, and assets that should rerun during
search. It always requires a final global gate.

`vision.progress` can run the persistent perception workspace for an explicit
capture set. Its deterministic router chooses from sixteen named specialists by
available graph type and expected information gain under a compute budget. Every
task, finding, evidence citation, contradiction, missing observation, proposed
next action, predicted information gain, and compute unit is stored.

Router training examples are accumulated from actual routed runs. Fixed benchmark
cases are scored inside the service against deterministic, best-single, and
uniform baselines at a bounded specialist count. Caller-supplied scores are never
accepted. A learned candidate is activated only after strict improvement without
case regressions; otherwise it is marked `REFUTED` and deterministic routing
remains active.
