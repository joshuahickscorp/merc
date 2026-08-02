# Bounded media rendering contract

The billable unit for this lane is one declared output pixel. The closed scene
JSON is an input commitment and validation boundary, not a proxy for physical
work; quotes and settlement therefore use `render_width × render_height` per
scene. This keeps the price authority aligned with the renderer benchmark's
pixels/second unit and prevents a tiny scene document from underpricing a large
canvas.

`media_rendering` is a deterministic vector-scene rasterisation lane, not image
generation. A buyer supplies a closed JSON scene containing a background colour
and at most 256 clipped rectangles. The governed builtin cell renders one P6
portable-pixmap artifact at the declared 16..1024 canvas size.

The control plane validates the closed scene, dimensions, pixel bound, one
primary plus one independent result, byte-exact agreement, and fixed-point
settlement. The agent performs the same scene validation before rasterising.
There is no model download, prompt-to-image claim, external asset fetch, or
unbounded renderer command surface. This lane does not activate the image
generation route.
