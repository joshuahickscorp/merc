# Archetype worked examples

These JSON snippets are declarative parameter sets consumed by
`blender_vision.procedural.library.get_archetype` / the scene grammar.

## 42U rack, 600 mm frame

```json
{
  "archetype": "rack_shell",
  "params": {
    "u_count": 42,
    "frame_width_m": 0.6,
    "depth_m": 1.0
  }
}
```

## Populate U3–U28 with 4U GPU drawers and three blanking gaps

```json
{
  "ops": [
    {"kind": "place", "archetype": "rack_shell", "instance_id": "rack_L_00",
     "params": {"u_count": 42, "frame_width_m": 0.6, "depth_m": 1.0}},
    {"kind": "leave_gap", "rack_id": "rack_L_00",
     "u_ranges": [[9, 10], [17, 18], [25, 26]]},
    {"kind": "populate_rack", "rack_id": "rack_L_00",
     "u_start": 3, "u_end": 28, "archetype": "gpu_drawer", "u_height": 4,
     "params": {"u_height": 4, "gpu_count": 8, "depth_m": 0.85}}
  ]
}
```

## Instance 24 racks along a 2.4 m-pitch cold aisle

```json
{
  "ops": [
    {"kind": "place", "archetype": "rack_shell", "instance_id": "rack_L_00",
     "location": [-1.1, 1.2, 0.0],
     "params": {"u_count": 42, "frame_width_m": 0.6, "depth_m": 1.0}},
    {"kind": "repeat_along", "source_id": "rack_L_00",
     "axis": "y", "count": 24, "pitch_m": 0.6, "id_prefix": "rack_L"}
  ]
}
```

## Status matrix state without remeshing

```json
{
  "ops": [
    {"kind": "place", "archetype": "status_light_matrix", "instance_id": "status_entry",
     "params": {"cols": 8, "rows": 4, "status": "ok"},
     "state": {"status": "ok"}},
    {"kind": "vary_state", "selector": "status_entry", "state": {"status": "warn"}}
  ]
}
```

`vary_state` mutates instance state only. `mesh_key` is unchanged so Blender
collection instancing remains valid.
