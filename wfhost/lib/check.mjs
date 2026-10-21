// `wf-host check`：类型检查 + lint + 元数据与 schema 合成，产出单个 JSON 结果给 Go。
//
// 有诊断即 ok=false —— Go 侧据此拒绝进入确认卡，且不执行任何东西（docs/WORKFLOW.md 第五节）。
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";
import { buildProgram, mapDiagnostics, analyze } from "./analyze.mjs";
import { parseMeta } from "./meta.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
export const FACADE_PATH = path.join(here, "..", "facade.d.ts");

function lineOf(text, index) {
  let line = 1;
  for (let i = 0; i < index; i++) if (text[i] === "\n") line += 1;
  return line;
}

/** args.<name> 必须已在元数据里声明。 */
function checkArgUsage(scriptText, meta, declaredNames) {
  const out = [];
  const re = /\bargs\.([A-Za-z_$][\w$]*)/g;
  let m;
  while ((m = re.exec(scriptText)) !== null) {
    const name = m[1];
    if (!declaredNames.has(name)) {
      out.push({ rule: "R11", line: lineOf(scriptText, m.index), column: 1, message: `用了未声明的参数 args.${name}（元数据 args 里没有这一项）` });
    }
  }
  if (!meta) out.push({ rule: "R11", line: 1, column: 1, message: "没有元数据块，args 一律视为未声明" });
  return out;
}

export function checkScript(scriptPath) {
  const scriptText = fs.readFileSync(scriptPath, "utf8");
  const facadeText = fs.readFileSync(FACADE_PATH, "utf8");

  const { program, sourceFile, checker } = buildProgram({ scriptText, facadeText });
  const list = [...program.getSyntacticDiagnostics(sourceFile), ...program.getSemanticDiagnostics(sourceFile)];
  const tscDiags = mapDiagnostics(program, sourceFile, list).map((d) => ({ source: "tsc", ...d }));

  const lint = analyze({ sourceFile, checker, scriptText });
  const lintDiags = lint.diagnostics.map((d) => ({ source: "lint", ...d }));

  const { meta, errors: metaErrors } = parseMeta(scriptText);
  const declared = new Set(Object.keys(meta?.args ?? {}));
  const argDiags = checkArgUsage(scriptText, meta, declared);
  const metaDiags = metaErrors.map((message) => ({ rule: "R12", line: 1, column: 1, message, source: "lint" }));

  const all = [...tscDiags, ...lintDiags, ...argDiags, ...metaDiags].sort(
    (a, b) => a.line - b.line || a.column - b.column || String(a.source).localeCompare(String(b.source)),
  );

  // 有诊断就不给出派生数据：确认卡只在 OK 时用它们，留着只会让人误信
  const usable = all.length === 0;

  return {
    ok: all.length === 0,
    script: scriptPath,
    meta: meta ?? null,
    phases: usable ? lint.phases : [],
    commands: usable ? lint.commands : [],
    agents: usable ? lint.agents : [],
    askSites: usable
      ? lint.askSites.map((s, i) => ({ index: i, raw: s.raw, typeName: s.typeName ?? null, schema: s.schema }))
      : [],
    diagnostics: all,
    typescript: ts.version,
  };
}
