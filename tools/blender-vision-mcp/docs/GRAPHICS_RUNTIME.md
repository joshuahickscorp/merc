# Browser graphics and runtime 3D evidence

`browser.graphics` is an isolated Chromium sensor for Canvas, WebGL, WebGL2, and WebGPU runtime
evidence. It instruments context creation, draw calls, buffer and texture uploads, and shader stage
metadata before page code executes. It records canvas geometry, drawing-buffer size, context
attributes, renderer limits, extensions, frame timing, fixed-time frame screenshots, and whether a
WebGPU adapter or configured WebGPU canvas was actually observed.

The sensor deliberately does not collect shader source content. It never turns opaque GPU buffers
into asserted geometry. An owned application can expose a narrow `__VISIONMCP_SCENE__` hook with
camera, mesh, material, transform, and animation records. Those public runtime records are
`OBSERVED`; a self-contained glTF compiled from them is explicitly `DERIVED`.

`vision.inspect_graphics` captures or discloses the resulting `GraphicsFrameGraph`.
`vision.reconstruct` can pass a materialized glTF through Blender's safe, auto-exec-disabled worker,
save an editable BLEND candidate, export GLB, re-open it through Blender, and run structural GLB
validation. The candidate is never auto-promoted.

The owned WebGL fixture proves:

- WebGL2 classification and draw/resource instrumentation;
- deterministic 0/500/1000 ms frames and a 90-degree runtime transform;
- explicit camera, triangle mesh, material, and animation evidence;
- self-contained glTF materialization;
- real Blender import, editable BLEND output, GLB export, and structural validation.

The round-trip report remains unaccepted because fixed camera equivalence, color-managed material
equivalence, and browser-versus-Blender frame residuals have not been calibrated. This distinction
is an acceptance blocker, not a warning hidden in logs.
