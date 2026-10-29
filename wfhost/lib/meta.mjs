// 流程定义头部的元数据块（行注释式）：
//
//   // friday-workflow
//   // description: 一句话说明这个流程做什么。
//   //   可以折行续写。
//   // whenToUse: 什么时候该用它。
//   // args:
//   //   target:
//   //     type: string
//   //     description: 要检查的目录
//   //     required: true
//   //   deep:
//   //     type: boolean
//   //     default: false
//
// 与 ZCode 的 .dwf.ts 相比改用行注释而非块注释：块注释里出现 `*/`（例如 glob "**/*.go"）
// 会提前终止注释、把 YAML 变成语法错误，而 TS 层面无法补救。其余字段一一对应，可一行 sed 迁移。
const ARG_TYPES = new Set(["string", "number", "boolean", "json"]);

function unquote(v) {
  const s = v.trim();
  if (s.startsWith('"') && s.endsWith('"') && s.length > 1) return s.slice(1, -1);
  if (s === "true") return true;
  if (s === "false") return false;
  if (s === "null") return null;
  if (/^-?\d+(\.\d+)?$/.test(s)) return Number(s);
  if (s.startsWith("{") || s.startsWith("[")) {
    try {
      return JSON.parse(s);
    } catch {
      return s;
    }
  }
  return s;
}

/** 取脚本开头的 `// friday-workflow` 行注释块正文；没有则返回 null。 */
export function metaBlock(scriptText) {
  const lines = scriptText.split("\n");
  let i = 0;
  while (i < lines.length && lines[i].trim() === "") i += 1;
  if (!/^\s*\/\/\s*friday-workflow\s*$/.test(lines[i] ?? "")) return null;
  i += 1;
  const body = [];
  for (; i < lines.length; i += 1) {
    const l = lines[i];
    if (/^\s*\/\/\s*$/.test(l)) {
      body.push("");
      continue;
    }
    const m = /^\s*\/\/\s?(.*)$/.exec(l);
    if (!m) break;
    body.push(m[1]);
  }
  return body.join("\n");
}

/** 解析元数据块。返回 {description, whenToUse, args:{name:{type,description,required,default}}} 与错误列表。 */
export function parseMeta(scriptText) {
  const block = metaBlock(scriptText);
  if (block === null) return { meta: null, errors: ["缺少 `// friday-workflow` 元数据块（description/whenToUse/args）"] };

  const errors = [];
  const meta = { description: "", whenToUse: "", args: {} };
  // 去掉行首统一缩进后按缩进层级解析
  const lines = block.split("\n").map((l) => l.replace(/\s+$/, ""));
  let mode = "top"; // top | args | argField
  let curArg = null;
  let lastKey = null;

  for (const raw of lines) {
    if (raw.trim() === "") continue;
    const indent = raw.length - raw.trimStart().length;
    const line = raw.trim();

    if (mode === "args" && indent === 0) mode = "top";
    if (mode === "argField" && indent < 4) {
      mode = "args";
      curArg = null;
    }

    if (mode === "top") {
      const kv = /^([A-Za-z][\w]*):\s*(.*)$/.exec(line);
      if (!kv) {
        if (indent > 0 && lastKey) meta[lastKey] = `${meta[lastKey]} ${line}`.trim();
        else errors.push(`无法解析的元数据行：${line}`);
        continue;
      }
      const [, key, value] = kv;
      if (key === "args") {
        mode = "args";
        lastKey = null;
        continue;
      }
      if (key !== "description" && key !== "whenToUse") {
        errors.push(`未知的元数据键：${key}`);
        continue;
      }
      meta[key] = value;
      lastKey = key;
      continue;
    }

    if (mode === "args") {
      const nameKv = /^([A-Za-z][\w-]*):\s*$/.exec(line);
      if (!nameKv) {
        errors.push(`args 下应为 "名字:" 形式的参数声明，得到：${line}`);
        continue;
      }
      curArg = nameKv[1];
      meta.args[curArg] = { type: "string", required: false, description: "", default: undefined };
      lastKey = null;
      mode = "argField";
      continue;
    }

    // argField
    const fv = /^([A-Za-z][\w]*):\s*(.*)$/.exec(line);
    if (!fv || !curArg) {
      errors.push(`参数 ${curArg ?? "?"} 下有无法解析的行：${line}`);
      continue;
    }
    const [, key, value] = fv;
    const spec = meta.args[curArg];
    if (key === "type") {
      if (!ARG_TYPES.has(value)) errors.push(`参数 ${curArg} 的 type 必须是 string/number/boolean/json，得到：${value}`);
      spec.type = value;
    } else if (key === "required") {
      spec.required = unquote(value) === true;
    } else if (key === "default") {
      spec.default = unquote(value);
    } else if (key === "description") {
      spec.description = value;
    } else {
      errors.push(`参数 ${curArg} 上有未知键：${key}`);
    }
    lastKey = key;
  }

  for (const [name, spec] of Object.entries(meta.args)) {
    if (!meta.description) errors.push("元数据缺少 description");
    if (spec.required && spec.default !== undefined) errors.push(`参数 ${name} 同时声明了 required 与 default`);
  }
  if (!meta.description) errors.push("元数据缺少 description");
  if (!meta.whenToUse) errors.push("元数据缺少 whenToUse");
  return { meta, errors };
}

/** 按元数据声明校验并补默认值；返回 {value, errors}。 */
export function validateArgs(meta, provided) {
  const errors = [];
  const out = {};
  const given = provided && typeof provided === "object" ? provided : {};
  const declared = meta?.args ?? {};

  for (const name of Object.keys(given)) {
    if (!(name in declared)) errors.push(`未声明的参数：${name}`);
  }
  for (const [name, spec] of Object.entries(declared)) {
    const raw = given[name];
    if (raw === undefined || raw === null || raw === "") {
      if (spec.required) {
        errors.push(`缺少必填参数：${name}`);
        continue;
      }
      if (spec.default !== undefined) out[name] = spec.default;
      continue;
    }
    switch (spec.type) {
      case "string":
        if (typeof raw !== "string") errors.push(`参数 ${name} 应为 string`);
        else out[name] = raw;
        break;
      case "number": {
        const n = typeof raw === "number" ? raw : Number(raw);
        if (!Number.isFinite(n)) errors.push(`参数 ${name} 应为 number`);
        else out[name] = n;
        break;
      }
      case "boolean":
        if (typeof raw === "boolean") out[name] = raw;
        else if (raw === "true") out[name] = true;
        else if (raw === "false") out[name] = false;
        else errors.push(`参数 ${name} 应为 boolean`);
        break;
      default:
        out[name] = raw; // json：原样透传
    }
  }
  return { value: out, errors };
}
