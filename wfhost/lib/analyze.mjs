// 编排脚本的静态分析：类型检查（注入 facade.d.ts）+ lint 规则 + ask 站点的 schema 合成。
//
// 关键设计：脚本被编译在一个 async 箭头函数体内（同 ZCode「compiled inside a function body」），
// 因此 `export`/`import`/`declare` 天然成为语法错误，顶层 `return` 与顶层 await 合法。
// 包装前缀写在**同一行**，使诊断行号与原脚本完全一致，仅第 1 行列号需要减去前缀长度。
import ts from "typescript";
import { schemaForType, UnsupportedType } from "./schema.mjs";

const PREFIX = "(async () => {";
const SUFFIX = "\n})()";

export const MAIN_NAME = "/friday-workflow.ts";
export const FACADE_NAME = "/facade.d.ts";

const compilerOptions = {
  target: ts.ScriptTarget.ES2022,
  module: ts.ModuleKind.ESNext,
  strict: true,
  noUncheckedIndexedAccess: false,
  noEmit: true,
  skipLibCheck: true,
  types: [],
  lib: ["lib.es2022.d.ts"],
};

/** 用内存里的脚本与 facade 建一个 Program。 */
export function buildProgram({ scriptText, facadeText }) {
  const wrapped = PREFIX + scriptText + SUFFIX;
  const files = new Map([
    [MAIN_NAME, wrapped],
    [FACADE_NAME, facadeText],
  ]);
  const base = ts.createCompilerHost(compilerOptions, true);
  const host = {
    ...base,
    getSourceFile: (fileName, languageVersion, onError) => {
      const text = files.get(fileName);
      if (text !== undefined) {
        return ts.createSourceFile(fileName, text, languageVersion, true, ts.ScriptKind.TS);
      }
      return base.getSourceFile(fileName, languageVersion, onError);
    },
    fileExists: (f) => files.has(f) || base.fileExists(f),
    readFile: (f) => (files.has(f) ? files.get(f) : base.readFile(f)),
  };
  const program = ts.createProgram([MAIN_NAME, FACADE_NAME], compilerOptions, host);
  const sourceFile = program.getSourceFile(MAIN_NAME);
  const checker = program.getTypeChecker();
  return { program, sourceFile, checker, wrapped };
}

/** 把 tsc 诊断映射回原脚本的行列。 */
export function mapDiagnostics(program, sourceFile, list) {
  const out = [];
  for (const d of list) {
    if (!d.file || d.file.fileName !== MAIN_NAME) continue;
    const pos = ts.getLineAndCharacterOfPosition(d.file, d.start ?? 0);
    const line = pos.line + 1; // tsc 0 基
    let column = pos.character + 1;
    if (line === 1) column -= PREFIX.length;
    out.push({
      line,
      column: Math.max(column, 1),
      code: `TS${d.code}`,
      message: flattenMessage(d.messageText),
    });
  }
  return out;
}

function flattenMessage(m) {
  if (typeof m === "string") return m;
  let s = m.messageText;
  let chain = m.next;
  while (chain) {
    s += ` ${typeof chain === "string" ? chain : chain.messageText}`;
    chain = chain.next;
  }
  return s;
}

function isCallTo(node, propName, objectName) {
  if (!ts.isCallExpression(node)) return false;
  const e = node.expression;
  if (!ts.isPropertyAccessExpression(e)) return false;
  return e.name.text === propName && identifierRoot(e.expression) === objectName;
}

function identifierRoot(expr) {
  if (ts.isIdentifier(expr)) return expr.text;
  if (ts.isPropertyAccessExpression(expr)) return identifierRoot(expr.expression);
  return "";
}

function isAskCall(node) {
  if (!ts.isCallExpression(node)) return false;
  const e = node.expression;
  return ts.isPropertyAccessExpression(e) && e.name.text === "ask";
}

function isPhaseCall(node) {
  if (!ts.isCallExpression(node)) return false;
  return ts.isIdentifier(node.expression) && node.expression.text === "phase";
}

/** 找到节点所处的语句及其在父块语句表里的下标。 */
function statementContext(node) {
  let cur = node;
  while (cur && !ts.isStatement(cur) && !ts.isSourceFile(cur)) cur = cur.parent;
  if (!cur || !cur.parent) return null;
  const list = ts.isSourceFile(cur.parent) ? cur.parent.statements : cur.parent.statements ?? cur.parent.members;
  if (!list) return null;
  return { stmt: cur, stmts: list, blockEnd: cur.parent.end, index: list.indexOf(cur) };
}

/** phase 标记的作用域 = 从该标记起、其所在块的剩余部分（含嵌套块）。 */
function phaseScope(marker) {
  const ctxPos = marker.getStart();
  const c = statementContext(marker);
  if (!c) return { start: ctxPos, end: ctxPos };
  return { start: ctxPos, end: c.blockEnd };
}

/** 取 buildProgram 注入的那层 async 箭头函数（`(async () => { ... })()`）。 */
function wrapperArrow(sourceFile) {
  const stmt = sourceFile.statements[0];
  if (!stmt || !ts.isExpressionStatement(stmt)) return undefined;
  const call = stmt.expression;
  const inner = ts.isCallExpression(call) ? call.expression : call;
  const fn = ts.isParenthesizedExpression(inner) ? inner.expression : inner;
  return ts.isArrowFunction(fn) ? fn : undefined;
}

/**
 * 跑全部 lint 规则与 schema 合成。
 * @returns {{diagnostics: Array<{rule:string,line:number,column:number,message:string}>, phases: string[], commands: string[], agents: string[], askSites: Array<{start:number,end:number,schema:object|null,raw:boolean}>}}
 */
export function analyze({ sourceFile, checker, scriptText }) {
  const diagnostics = [];
  const phases = [];
  const scopes = [];
  const commands = new Set();
  const agentsSeen = new Map();
  const agents = [];
  const askSites = [];
  const effectSites = []; // 每个 ask / world.run 的位置，用于 phase 覆盖检查

  const add = (rule, node, message) => {
    const pos = sourceFile.getLineAndCharacterOfPosition(node.getStart());
    let column = pos.character + 1;
    if (pos.line === 0) column -= PREFIX.length;
    diagnostics.push({ rule, line: pos.line + 1, column: Math.max(column, 1), message });
  };

  const visit = (node) => {
    // R1：world.run 的命令名必须是字面量
    if (isCallTo(node, "run", "world")) {
      const first = node.arguments[0];
      if (!first || !ts.isStringLiteral(first)) {
        add("R1", node, "world.run 的第一个参数必须是字符串字面量：命令集要在确认卡上整批展示");
      } else {
        commands.add(first.text);
      }
      effectSites.push({ node, kind: "world.run" });
    }

    // phase 标记：名字须为字面量且唯一
    if (isPhaseCall(node)) {
      const first = node.arguments[0];
      if (!first || !ts.isStringLiteral(first)) {
        add("R3", node, "phase 的名字必须是字符串字面量");
      } else {
        if (!phases.includes(first.text)) phases.push(first.text);
        scopes.push({ name: first.text, node, ...phaseScope(node), hits: 0 });
      }
    }

    // R4：agent 字面量名不得重复
    if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === "agent") {
      const first = node.arguments[0];
      if (first && ts.isStringLiteral(first)) {
        const name = first.text;
        if (agentsSeen.has(name)) {
          add("R4", node, `子代理名 "${name}" 重复：一个 run 内非空名字必须唯一（循环里请构造名字）`);
        } else {
          agentsSeen.set(name, true);
          agents.push(name);
        }
      }
    }

    // ask 站点：合成 schema
    if (isAskCall(node)) {
      const typeArgs = node.expression.typeArguments ?? node.typeArguments;
      if (!typeArgs || typeArgs.length === 0) {
        askSites.push({ start: node.getStart(), end: node.getEnd(), schema: null, raw: true });
      } else {
        const typeNode = typeArgs[0];
        const errors = [];
        let schema = null;
        try {
          const t = checker.getTypeFromTypeNode(typeNode);
          schema = schemaForType(checker, t, { errors, seen: new Set(), depth: 0 });
        } catch (err) {
          if (err instanceof UnsupportedType) errors.push(err.message);
          else throw err;
        }
        if (errors.length > 0) {
          add("R7", typeNode, `ask 的类型参数无法合成 schema：${errors.join("；")}`);
        }
        // 改写定位：类型参数挂在本 CallExpression 上（node.typeArguments），
        // 所以 ".ask" 之后到 "<" 起、参数逐个取原文，不去找括号（类型里可能有 "() => void"）。
        askSites.push({
          start: node.getStart(),
          end: node.getEnd(),
          typeArgsStart: node.expression.getEnd(),
          args: node.arguments.map((a) => sourceFile.text.slice(a.getStart(), a.getEnd())),
          schema,
          raw: false,
          typeName: sourceFile.text.slice(typeNode.getStart(), typeNode.getEnd()),
        });
      }
      effectSites.push({ node, kind: "ask" });
    }

    // 决定论：Date / Math.random / eval / new Function 一律禁止
    if (ts.isPropertyAccessExpression(node)) {
      const root = identifierRoot(node.expression);
      const prop = node.name.text;
      if (root === "Date" && prop === "now") add("R8", node, "禁用 Date.now()：脚本必须可重放（时间由 Go 侧经 args 或世界读取给出）");
      if (root === "Math" && prop === "random") add("R8", node, "禁用 Math.random()：脚本必须可重放");
    }
    if (ts.isNewExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === "Date") {
      add("R8", node, "禁用 new Date()：脚本必须可重放");
    }
    if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) && ["eval", "Function", "setTimeout", "setInterval"].includes(node.expression.text)) {
      add("R8", node, `禁用 ${node.expression.text}：facade 未提供，宿主沙箱里不存在`);
    }

    // 一期未提供的 facade 成员，给出比 "Cannot find name" 更明确的提示
    if (ts.isIdentifier(node) && ["hole", "artifact", "model", "budget"].includes(node.text)) {
      const parent = node.parent;
      const isUse = !(ts.isPropertyAccessExpression(parent) && parent.name === node);
      if (isUse) add("R9", node, `${node.text}() 一期未提供（见 docs/WORKFLOW.md 第十四节）`);
    }

    ts.forEachChild(node, visit);
  };

  ts.forEachChild(sourceFile, visit);

  // 作用域切分：标记声明到"下一个标记"为止（同 ZCode 的 lexical stamping 近似）。
  // 注意一期限制：并发阶段（future）的步子按文档顺序归属，可能落到相邻阶段窗口里，
  // 所以 R2 只在顺序流程上可靠；这条写进 docs/WORKFLOW.md 的编写契约里。
  scopes.sort((a, b) => a.start - b.start);
  for (let i = 0; i < scopes.length - 1; i += 1) {
    if (scopes[i + 1].start < scopes[i].end) scopes[i].end = scopes[i + 1].start;
  }

  // R2/R3：phase 覆盖与空阶段
  for (const eff of effectSites) {
    const p = eff.node.getStart();
    if (!scopes.some((s) => p >= s.start && p < s.end)) {
      add("R3", eff.node, `${eff.kind} 不在任何 phase 里：每个阶段都要有 phase 标记，确认卡按它画节点`);
    } else {
      for (const s of scopes) if (p >= s.start && p < s.end) s.hits += 1;
    }
  }
  for (const s of scopes) {
    if (s.hits === 0) add("R2", s.node, `phase "${s.name}" 里没有任何 ask 或 world.run：纯脚本逻辑不构成阶段`);
  }

  // R5：必须有一个带值的顶层 return
  const body = sourceFile.statements[0];
  let hasReturn = false;
  const findTopReturn = (n) => {
    if (ts.isReturnStatement(n) && n.expression) hasReturn = true;
    if (ts.isFunctionDeclaration(n) || ts.isFunctionExpression(n) || ts.isArrowFunction(n) || ts.isMethodDeclaration(n)) return;
    ts.forEachChild(n, findTopReturn);
  };
  const wrappedArrow = wrapperArrow(sourceFile);
  if (wrappedArrow && ts.isBlock(wrappedArrow.body)) {
    for (const st of wrappedArrow.body.statements) findTopReturn(st);
  }
  if (!hasReturn) add("R5", body, "脚本必须以一个带值的 return 结束：那才是带回给小五的 WorkflowReport");

  // R10：interface 不得带泛型参数
  const declCheck = (n) => {
    if ((ts.isInterfaceDeclaration(n) || ts.isTypeAliasDeclaration(n)) && (n.typeParameters?.length ?? 0) > 0) {
      add("R7", n, `不支持泛型类型 ${n.name.text}：schema 合成需要具体形态`);
    }
    ts.forEachChild(n, declCheck);
  };
  ts.forEachChild(sourceFile, declCheck);

  diagnostics.sort((a, b) => a.line - b.line || a.column - b.column);
  return { diagnostics, phases, commands: [...commands], agents, askSites };
}

/**
 * 把 `obj.ask<T>(instructions)` 改写成 `obj.ask(instructions, __s(i))`，让运行期能拿到合成的 schema。
 * 位置来自 AST，不能用字符串查找——`agent(...).ask<T>(x)` 的第一个左括号属于内层调用。
 * i 必须与 analyze 返回的 askSites 下标一致（遍历顺序即源码顺序）。
 */
export function rewriteAskSites(text, askSites) {
  let out = "";
  let cursor = 0;
  askSites.forEach((site, i) => {
    out += text.slice(cursor, site.start);
    if (site.raw || !site.args) {
      out += text.slice(site.start, site.end);
    } else {
      out += text.slice(site.start, site.typeArgsStart); // 到 ".ask" 为止
      out += `(${[...site.args, `__s(${i})`].join(", ")})`;
    }
    cursor = site.end;
  });
  out += text.slice(cursor);
  return out;
}
