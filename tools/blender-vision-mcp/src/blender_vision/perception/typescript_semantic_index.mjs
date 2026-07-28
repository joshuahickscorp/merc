import { readFileSync } from "node:fs";
import { isAbsolute, relative, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const request = JSON.parse(readFileSync(0, "utf8"));
const root = resolve(request.root);
const packageRoot = resolve(request.typescript_package);
const packageDocument = JSON.parse(
  readFileSync(resolve(packageRoot, "package.json"), "utf8"),
);
const major = Number(String(packageDocument.version).split(".")[0]);
const selected = new Set(
  request.files.map((item) => resolve(root, item).replaceAll("\\", "/")),
);
const referenceLimit = Number(request.reference_limit);

function confined(fileName) {
  const absolute = resolve(fileName);
  const path = relative(root, absolute);
  return path !== "" && !path.startsWith("..") && !isAbsolute(path);
}

function normalizedRelative(fileName) {
  return relative(root, resolve(fileName)).replaceAll("\\", "/");
}

function declarationName(node, is) {
  return node.name && is.isIdentifier(node.name) ? node.name : undefined;
}

function isDeclaration(node, is) {
  return (
    is.isFunctionDeclaration(node)
    || is.isClassDeclaration(node)
    || is.isInterfaceDeclaration(node)
    || is.isTypeAliasDeclaration(node)
    || is.isEnumDeclaration(node)
    || is.isVariableDeclaration(node)
  );
}

function exported(node, modifierFlags) {
  let current = node;
  while (current && current.kind !== undefined) {
    if (Number(current.modifierFlags ?? 0) & Number(modifierFlags.Export)) {
      return true;
    }
    current = current.parent;
  }
  return false;
}

function diagnosticRecord(diagnostic, sourceFileForDiagnostic) {
  const file = diagnostic.file
    ?? (diagnostic.fileName ? sourceFileForDiagnostic(diagnostic.fileName) : undefined);
  const start = Number(diagnostic.start ?? diagnostic.pos ?? 0);
  const location = file?.getLineAndCharacterOfPosition
    ? file.getLineAndCharacterOfPosition(start)
    : undefined;
  const messageValue = diagnostic.messageText
    ?? diagnostic.message
    ?? diagnostic.text
    ?? "";
  const message = typeof messageValue === "string"
    ? messageValue
    : String(messageValue.messageText ?? messageValue);
  return {
    code: Number(diagnostic.code ?? 0),
    category: ["warning", "error", "suggestion", "message"][
      Number(diagnostic.category)
    ] ?? "unknown",
    file: file?.fileName && confined(file.fileName)
      ? normalizedRelative(file.fileName)
      : null,
    line: location ? location.line + 1 : null,
    character: location ? location.character + 1 : null,
    message,
  };
}

function buildIndex(engine) {
  const {
    checkerFor,
    diagnostics,
    formatKind,
    is,
    modifierFlags,
    projectFor,
    sourceFiles,
    symbolDeclarations,
    symbolForIdentifier,
    typeToString,
  } = engine;
  const files = sourceFiles()
    .filter((sourceFile) => selected.has(resolve(sourceFile.fileName).replaceAll("\\", "/")))
    .sort((left, right) => left.fileName.localeCompare(right.fileName));
  const symbols = [];
  const symbolByDeclaration = new Map();
  const declarationKey = (fileName, start, name) =>
    `${normalizedRelative(fileName)}:${start}:${name}`;

  for (const sourceFile of files) {
    const checker = checkerFor(sourceFile);
    function visit(node) {
      if (isDeclaration(node, is)) {
        const nameNode = declarationName(node, is);
        if (nameNode) {
          const name = nameNode.text ?? nameNode.getText(sourceFile);
          const start = nameNode.getStart(sourceFile);
          const location = sourceFile.getLineAndCharacterOfPosition(start);
          const compilerSymbol = checker.getSymbolAtLocation(nameNode);
          const type = checker.getTypeAtLocation(nameNode);
          const record = {
            id: declarationKey(sourceFile.fileName, start, name),
            path: normalizedRelative(sourceFile.fileName),
            name,
            line: location.line + 1,
            character: location.character + 1,
            start,
            kind: formatKind(node.kind),
            exported: exported(node, modifierFlags),
            type: type ? typeToString(checker, type, node) : "unknown",
            compiler_symbol_name: compilerSymbol?.name ?? name,
          };
          symbols.push(record);
          symbolByDeclaration.set(record.id, record);
        }
      }
      node.forEachChild(visit);
    }
    visit(sourceFile);
  }

  const imports = [];
  for (const sourceFile of files) {
    const checker = checkerFor(sourceFile);
    for (const moduleNode of sourceFile.imports ?? []) {
      const compilerSymbol = checker.getSymbolAtLocation(moduleNode);
      const declarations = compilerSymbol
        ? symbolDeclarations(compilerSymbol, projectFor(sourceFile))
        : [];
      const target = declarations
        .map((item) => item.getSourceFile?.().fileName)
        .find((fileName) => fileName && confined(fileName));
      const location = sourceFile.getLineAndCharacterOfPosition(
        moduleNode.getStart(sourceFile),
      );
      imports.push({
        source_path: normalizedRelative(sourceFile.fileName),
        module: moduleNode.text ?? moduleNode.getText(sourceFile).replaceAll('"', ""),
        line: location.line + 1,
        resolved_path: target ? normalizedRelative(target) : null,
        resolution: target ? "workspace" : "external",
      });
    }
  }

  const references = [];
  let referenceTruncated = false;
  for (const sourceFile of files) {
    const checker = checkerFor(sourceFile);
    function visit(node) {
      if (references.length >= referenceLimit) {
        referenceTruncated = true;
        return;
      }
      if (is.isIdentifier(node)) {
        const compilerSymbol = symbolForIdentifier(checker, node);
        const declarations = compilerSymbol
          ? symbolDeclarations(compilerSymbol, projectFor(sourceFile))
          : [];
        const targetNode = declarations.find((item) => {
          const targetFile = item.getSourceFile?.().fileName;
          return targetFile && selected.has(resolve(targetFile).replaceAll("\\", "/"));
        });
        if (targetNode) {
          const targetFile = targetNode.getSourceFile();
          const targetNameNode = declarationName(targetNode, is);
          if (targetNameNode) {
            const targetName = targetNameNode.text ?? targetNameNode.getText(targetFile);
            const targetStart = targetNameNode.getStart(targetFile);
            const targetId = declarationKey(targetFile.fileName, targetStart, targetName);
            if (symbolByDeclaration.has(targetId)) {
              const start = node.getStart(sourceFile);
              const location = sourceFile.getLineAndCharacterOfPosition(start);
              if (
                sourceFile.fileName !== targetFile.fileName
                || start !== targetStart
              ) {
                references.push({
                  source_path: normalizedRelative(sourceFile.fileName),
                  line: location.line + 1,
                  character: location.character + 1,
                  name: node.text ?? node.getText(sourceFile),
                  target_symbol_id: targetId,
                });
              }
            }
          }
        }
      }
      node.forEachChild(visit);
    }
    visit(sourceFile);
  }

  return {
    schema: "vision.typescript-semantic-index/v1",
    engine: major >= 7 ? "typescript-native-compiler-api" : "typescript-compiler-api",
    typescript_version: packageDocument.version,
    files: files.map((sourceFile) => normalizedRelative(sourceFile.fileName)),
    symbols,
    imports,
    references,
    reference_truncated: referenceTruncated,
    diagnostics: diagnostics()
      .slice(0, 200)
      .map((item) => diagnosticRecord(item, (fileName) =>
        files.find((sourceFile) => resolve(sourceFile.fileName) === resolve(fileName)))),
  };
}

async function typescriptSevenEngine() {
  const apiEntry = resolve(packageRoot, "dist/api/sync/api.js");
  const isEntry = resolve(packageRoot, "dist/ast/is.js");
  const astEntry = resolve(packageRoot, "dist/ast/index.js");
  const [{ API }, is, ast] = await Promise.all([
    import(pathToFileURL(apiEntry).href),
    import(pathToFileURL(isEntry).href),
    import(pathToFileURL(astEntry).href),
  ]);
  const api = new API({ cwd: root });
  const openFiles = request.files.map((item) => resolve(root, item));
  const snapshot = api.updateSnapshot({
    openProjects: [resolve(root, request.tsconfig)],
    openFiles,
  });
  const projects = snapshot.getProjects();
  if (!projects.length) {
    snapshot.dispose();
    api.close();
    throw new Error("TypeScript compiler did not load a project");
  }
  const projectForFileName = (fileName) =>
    snapshot.getDefaultProjectForFile(fileName)
    ?? projects.find((candidate) => candidate.program.getSourceFile(fileName));
  return {
    api,
    snapshot,
    engine: {
      checkerFor: (sourceFile) => projectForFileName(sourceFile.fileName).checker,
      diagnostics: () => projects.flatMap((project) => [
          ...project.program.getConfigFileParsingDiagnostics(),
          ...project.program.getProgramDiagnostics(),
          ...project.program.getSyntacticDiagnostics(),
          ...project.program.getSemanticDiagnostics(),
        ]),
      formatKind: ast.formatSyntaxKind,
      is,
      modifierFlags: ast.ModifierFlags,
      projectFor: (sourceFile) => projectForFileName(sourceFile.fileName),
      sourceFiles: () => openFiles
        .map((fileName) => projectForFileName(fileName)?.program.getSourceFile(fileName))
        .filter(Boolean),
      symbolDeclarations: (symbol, activeProject) =>
        (symbol.declarations ?? [])
          .map((handle) => handle.resolve(activeProject))
          .filter(Boolean),
      symbolForIdentifier: (checker, node) =>
        checker.getResolvedSymbol(node)
        ?? checker.getSymbolAtLocation(node),
      typeToString: (checker, type, node) => checker.typeToString(type, node),
    },
  };
}

async function legacyEngine() {
  const entry = resolve(packageRoot, "lib/typescript.js");
  const imported = await import(pathToFileURL(entry).href);
  const ts = imported.default ?? imported;
  const configPath = resolve(root, request.tsconfig);
  const config = ts.readConfigFile(configPath, ts.sys.readFile);
  if (config.error) {
    throw new Error(ts.flattenDiagnosticMessageText(config.error.messageText, "\n"));
  }
  const parsed = ts.parseJsonConfigFileContent(config.config, ts.sys, root);
  const program = ts.createProgram({
    rootNames: parsed.fileNames,
    options: parsed.options,
    projectReferences: parsed.projectReferences,
  });
  const checker = program.getTypeChecker();
  return {
    api: null,
    snapshot: null,
    engine: {
      checkerFor: () => checker,
      diagnostics: () => ts.getPreEmitDiagnostics(program),
      formatKind: (kind) => ts.SyntaxKind[kind] ?? String(kind),
      is: ts,
      modifierFlags: ts.ModifierFlags,
      projectFor: () => null,
      sourceFiles: () => program.getSourceFiles(),
      symbolDeclarations: (symbol) => symbol.declarations ?? [],
      symbolForIdentifier: (_checker, node) => {
        let symbol = checker.getSymbolAtLocation(node);
        if (symbol && symbol.flags & ts.SymbolFlags.Alias) {
          symbol = checker.getAliasedSymbol(symbol);
        }
        return symbol;
      },
      typeToString: (_checker, type, node) => checker.typeToString(type, node),
    },
  };
}

let resources;
try {
  resources = major >= 7 ? await typescriptSevenEngine() : await legacyEngine();
  process.stdout.write(`${JSON.stringify(buildIndex(resources.engine))}\n`);
} finally {
  resources?.snapshot?.dispose();
  resources?.api?.close();
}
