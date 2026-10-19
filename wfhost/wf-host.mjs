#!/usr/bin/env node
// Friday 动态工作流宿主 CLI。由 Go 侧 spawn，两个子命令：
//
//   wf-host check <file.wf.ts>
//       → 一行 JSON：{ok, meta, phases, commands, agents, askSites, diagnostics, typescript}
//         退出码 0 = 无诊断，10 = 有诊断（Go 据此拒绝进入确认卡，且不执行任何东西）
//   wf-host run <file.wf.ts> [--args '<json>'] [--args-file <path>]
//       → stdin/stdout 换行帧 JSON-RPC；结束时发 {"op":"result","value":...} 或 {"op":"error",...}
//
// 帧约定（与 internal/workflow/frame.go 对应）：
//   host→Go  {"id":N,"op":"ask"|"world.run"|"files.glob"|"files.read"|"files.grep"|"git.*",...}
//   host→Go  {"op":"event","kind":"phase"|"log"|"report",...}
//   Go→host  {"id":N,"result":...} 或 {"id":N,"error":"..."}
//   Go→host  {"op":"cancel"}
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import ts from "typescript";
import { checkScript } from "./lib/check.mjs";
import { startRun } from "./lib/run.mjs";

const argv = process.argv.slice(2);
const cmd = argv[0];

function emit(frame) {
  process.stdout.write(JSON.stringify(frame) + "\n");
}

function fail(message, code = 2) {
  process.stderr.write(message + "\n");
  process.exit(code);
}

function flag(name) {
  const i = argv.indexOf(name);
  return i < 0 ? null : (argv[i + 1] ?? null);
}

if (cmd === "version" || cmd === "--version") {
  emit({ node: process.version, typescript: ts.version });
  process.exit(0);
}

if (cmd === "check") {
  const file = argv[1];
  if (!file) fail("用法：wf-host check <file.wf.ts>");
  let result;
  try {
    result = checkScript(path.resolve(file));
  } catch (e) {
    fail(`check 未能完成：${e?.stack ?? e}`);
  }
  emit(result);
  process.exit(result.ok ? 0 : 10);
}

if (cmd !== "run") {
  fail(`未知子命令：${cmd ?? "(空)"}\n用法：wf-host check|run <file.wf.ts> [...] [--args '<json>']\n      wf-host version`);
}

const file = argv[1];
if (!file) fail("用法：wf-host run <file.wf.ts> [--args '<json>'] [--args-file <path>]");

let argsProvided = {};
try {
  const inline = flag("--args");
  const fromFile = flag("--args-file");
  if (inline !== null) argsProvided = JSON.parse(inline);
  else if (fromFile !== null) argsProvided = JSON.parse(fs.readFileSync(fromFile, "utf8"));
} catch (e) {
  fail(`入参 JSON 解析失败：${e?.message ?? e}`);
}

let started;
try {
  started = startRun({ scriptPath: path.resolve(file), argsProvided, sendFrame: emit });
} catch (e) {
  emit({ op: "error", message: e?.message ?? String(e), stage: "setup" });
  process.exit(10);
}

let buf = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  buf += chunk;
  let nl;
  while ((nl = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, nl);
    buf = buf.slice(nl + 1);
    if (line.trim() === "") continue;
    let frame;
    try {
      frame = JSON.parse(line);
    } catch {
      continue; // 非 JSON 行忽略
    }
    started.handleFrame(frame);
  }
});

// Go 关掉管道 = 取消
process.stdin.on("end", () => started.handleFrame({ op: "cancel" }));

process.on("unhandledRejection", (reason) => {
  emit({
    op: "error",
    message: `未处理的 Promise 拒绝：${reason instanceof Error ? reason.message : String(reason)}`,
    stage: "runtime",
  });
  process.exit(11);
});

started.done
  .then((value) => {
    emit({ op: "result", value });
    process.exit(0);
  })
  .catch((e) => {
    emit({ op: "error", message: e?.message ?? String(e), stack: e?.stack ?? null, stage: "runtime" });
    process.exit(11);
  });
