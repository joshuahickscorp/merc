# Compiler-grade TypeScript source authority

`code.repository` retains its non-executing static fallback, but it no longer has to treat
TypeScript and JavaScript as regex-only text. A caller can supply an already installed TypeScript
package inside the governed repository:

```json
{
  "typescript_package_path": "node_modules/typescript",
  "typescript_tsconfig": "tsconfig.json",
  "semantic_reference_limit": 50000
}
```

VisionMCP does not install or download that dependency. It rejects absolute paths, traversal,
symlinks, packages other than `typescript`, versions older than 5, and compiler files outside the
repository. The capture identity binds:

- TypeScript package version;
- compiler API, AST helpers, package metadata, and the TypeScript 7 native executable when used;
- SHA-256 of every compiler authority file;
- the resolved Node executable, version, size, and SHA-256;
- the repository manifest and `tsconfig.json`;
- the semantic reference cap.

The bundled analyzer supports the TypeScript 5/6 compiler API and TypeScript 7 native compiler API.
It emits compiler-resolved declarations, exported state, declaration kind, inferred type,
workspace-versus-external imports, symbol references, and syntactic/semantic diagnostics. The raw
index is content-addressed as `source.typescript-semantic-index`, while `CodeGraph` contains its
digest and `RESOLVES_IMPORT` / `REFERENCES_SYMBOL` edges.

Files not covered by the configured `tsconfig.json` are opened as compiler-managed inferred
projects, so standalone frontend entry points remain compiler-indexed. The fallback remains
explicit as `semantic_index.enabled=false` and `engine=static-pattern-fallback`; it cannot be
reported as compiler-semantic evidence.

## Bidirectional runtime traces

`SourceIntelligenceService.source_to_pixel_trace` follows a compiler-indexed file/symbol through an
observed runtime binding into linked layout, interaction, responsive, motion, or graphics graphs.
It returns the matched runtime nodes, evidence references, CSS-pixel bounds, and capture citations.
An uninstrumented symbol remains `HYPOTHESIS`; a binding without a matching runtime observation is
only `DERIVED`.

`event_to_source_trace` performs the reverse lookup from an observed `InteractionGraph` or
`StateGraph` edge through its event target into the bound source nodes. It returns `OBSERVED` only
when the event edge, runtime binding, and source node all exist in their content-addressed graphs.
