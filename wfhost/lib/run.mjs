// `wf-host run`：在 vm 沙箱里执行编排脚本，一切副作用都回 Go 侧履约。
//
// 不变量（docs/WORKFLOW.md 第三节）：
//   1. 沙箱里只有 facade 注入的全局——没有 fs/process/fetch/eval/定时器；
//   2. 每个跨界调用都带 runId 相关的上下文回到 Go，由 Go 过权限、记账、写轨迹；
//   3. 脚本禁用 Date.now / Math.random / new Date（决定论），运行期也拦。
import fs from "node:fs";
import vm from "node:vm";
import ts from "typescript";
import { buildProgram, analyze, rewriteAskSites } from "./analyze.mjs";
import { parseMeta, validateArgs } from "./meta.mjs";

export class RunCancelled extends Error {}

function denied(what) {
  return () => {
    throw new Error(`工作流脚本禁用 ${what}：脚本必须可重放（见 docs/WORKFLOW.md 第五节 R8）`);
  };
}

function deepFreeze(o) {
  if (o && typeof o === "object" && !Object.isFrozen(o)) {
    Object.freeze(o);
    for (const v of Object.values(o)) deepFreeze(v);
  }
  return o;
}

/**
 * 启动一次执行。设置阶段全同步（读文件、静态分析、转译、建沙箱），
 * 返回 done（脚本 return 值）与 handleFrame（把 Go 的应答帧喂进来），CLI 拿到返回值之后再接 stdin。
 * @param {{scriptPath:string, argsProvided?:object, sendFrame:(f:object)=>void, cancel?:Promise<void>}} opts
 */
export function startRun({ scriptPath, argsProvided = {}, sendFrame, cancel }) {
  const scriptText = fs.readFileSync(scriptPath, "utf8");

  // 1. 重新做一次静态分析，拿到 ask 站点与 schema（check 已经过一次，这里保证执行的是同一份形态）
  const facadeText = fs.readFileSync(new URL("../facade.d.ts", import.meta.url), "utf8");
  const { wrapped, sourceFile, checker } = buildProgram({ scriptText, facadeText });
  const lint = analyze({ sourceFile, checker, scriptText });
  const blocking = lint.diagnostics.filter((d) => d.rule === "R1" || d.rule === "R7" || d.rule === "R8");
  if (blocking.length > 0) {
    throw new Error(`脚本未通过预检，未执行任何操作：\n${blocking.map((d) => `  L${d.line}:${d.column} [${d.rule}] ${d.message}`).join("\n")}`);
  }

  // 2. 元数据与入参
  const { meta, errors: metaErrors } = parseMeta(scriptText);
  if (metaErrors.length > 0) throw new Error(`元数据不合法：${metaErrors.join("；")}`);
  const { value: argv, errors: argErrors } = validateArgs(meta, argsProvided);
  if (argErrors.length > 0) throw new Error(`入参不合法：${argErrors.join("；")}`);

  // 3. 注入 schema 引用 → 转译成 JS（顶层 await/return 由 buildProgram 的 async 包装承载）
  const sites = lint.askSites.map((s) => s.schema);
  const rewritten = rewriteAskSites(wrapped, lint.askSites);
  const js = ts.transpileModule(rewritten, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, isolatedModules: true },
    fileName: scriptPath,
  }).outputText;

  // 4. RPC 管线
  const pending = new Map();
  let nextId = 1;
  let cancelled = false;
  const request = (op, payload) =>
    new Promise((resolve, reject) => {
      if (cancelled) return reject(new RunCancelled("run 已被取消"));
      const id = nextId++;
      pending.set(id, { resolve, reject });
      sendFrame({ id, op, ...payload });
    });

  function handleFrame(f) {
    if (f && f.op === "cancel") {
      cancelled = true;
      for (const { reject } of pending.values()) reject(new RunCancelled("run 已被取消"));
      pending.clear();
      return true;
    }
    if (f && f.id && pending.has(f.id)) {
      const { resolve, reject } = pending.get(f.id);
      pending.delete(f.id);
      if (f.error) reject(new Error(f.error));
      else resolve(f.result);
      return true;
    }
    return false;
  }

  // 5. facade 沙箱
  const seqs = new Map();
  let anon = 0;
  const makeAgent = (name, persona) => {
    const key = name ?? `anon-${anon++}`;
    return {
      ask(instructions, schemaRef) {
        const seq = (seqs.get(key) ?? 0) + 1;
        seqs.set(key, seq);
        return request("ask", {
          agent: name ?? null,
          persona: persona ?? null,
          seq,
          instructions: String(instructions),
          // 无类型参数的 ask 不带 schema 字段（而不是 null）：Go 侧据此判断是否走结构化应答
          schema: schemaRef === undefined || schemaRef === null ? undefined : schemaRef,
        }).then((res) => {
          if (schemaRef) return res?.value;
          return res?.text ?? (res?.value === undefined ? "" : String(res.value));
        });
      },
    };
  };

  const schemas = (i) => sites[i] ?? null;

  const sandbox = {
    agent: makeAgent,
    phase: (name) => sendFrame({ op: "event", kind: "phase", name: String(name) }),
    log: (message) => sendFrame({ op: "event", kind: "log", message: String(message) }),
    report: (item) => {
      let serialized;
      try {
        serialized = JSON.parse(JSON.stringify(item));
      } catch {
        throw new TypeError("report 的条目必须可 JSON 序列化（函数、Date、class 实例一律拒绝）");
      }
      sendFrame({ op: "event", kind: "report", item: serialized });
    },
    args: deepFreeze({ ...argv }),
    world: {
      run: (cmd, args, opts) =>
        request("world.run", { cmd: String(cmd), args: args ?? [], timeoutMs: opts?.timeoutMs ?? null }).then((r) => ({
          exitCode: r?.exitCode ?? -1,
          stdout: r?.stdout ?? "",
          stderr: r?.stderr ?? "",
        })),
    },
    files: {
      glob: (pattern) => request("files.glob", { pattern: String(pattern) }).then((r) => r ?? []),
      read: (p) => request("files.read", { path: String(p) }).then((r) => (typeof r === "string" ? r : r?.text ?? "")),
      grep: (pattern, glob) => request("files.grep", { pattern: String(pattern), glob: glob ?? null }).then((r) => r ?? []),
    },
    git: {
      changedFiles: (base) => request("git.changedFiles", { base: base ?? null }).then((r) => r ?? []),
      diff: (base, p) => request("git.diff", { base: base ?? null, path: p ?? null }).then((r) => (typeof r === "string" ? r : r?.text ?? "")),
      status: () => request("git.status", {}),
      log: (count) => request("git.log", { count: count ?? null }).then((r) => r ?? []),
    },
    channel: (name) => makeChannel(name ?? "channel"),
    future: (body) => {
      try {
        return Promise.resolve(body());
      } catch (e) {
        return Promise.reject(e);
      }
    },
    __s: schemas,
    // 决定论：时间源与随机源在沙箱里不存在
    Date: Object.assign(denied("new Date()"), { now: denied("Date.now()"), parse: denied("Date.parse()"), UTC: denied("Date.UTC()") }),
    Math: Object.assign(Object.create(Math), { random: denied("Math.random()") }),
  };

  const context = vm.createContext(sandbox, {
    codeGeneration: { strings: false, wasm: false }, // eval / new Function 一律不可能
  });

  if (cancel) {
    cancel.then(() => handleFrame({ op: "cancel" })).catch(() => {});
  }

  const started = vm.runInContext(js, context, { filename: scriptPath });
  const done = Promise.resolve(started).then((v) => {
    if (cancelled) throw new RunCancelled("run 已被取消");
    try {
      return JSON.parse(JSON.stringify(v ?? null));
    } catch {
      throw new TypeError("脚本 return 的值必须可 JSON 序列化");
    }
  });
  return { done, handleFrame };
}

/** channel：Go 侧不参与，纯沙箱内原语；未 close 的 channel 由调用方负责（一期不做 deadlock 检测）。 */
function makeChannel(name) {
  const q = { items: [], waiters: [], closed: false };
  const ch = {
    send(item) {
      if (q.closed) throw new Error(`ChannelClosed：${name} 已关闭`);
      const w = q.waiters.shift();
      if (w) w({ value: item, done: false });
      else q.items.push(item);
    },
    close() {
      if (q.closed) return;
      q.closed = true;
      for (const w of q.waiters.splice(0)) w({ value: undefined, done: true });
    },
  };
  ch[Symbol.asyncIterator] = () => ({
    next() {
      if (q.items.length) return Promise.resolve({ value: q.items.shift(), done: false });
      if (q.closed) return Promise.resolve({ value: undefined, done: true });
      return new Promise((res) => q.waiters.push(res));
    },
    return() {
      q.closed = true;
      return Promise.resolve({ value: undefined, done: true });
    },
  });
  return ch;
}
