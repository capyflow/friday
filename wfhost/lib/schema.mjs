// 从 TS 类型合成 JSON Schema（一期最小实现）。
//
// 只支持脚本里能安全擦除并校验的形态：基本类型、字面量联合、数组、嵌套 interface、
// 可选属性、JSDoc 注释（→ description）。其余形态（泛型参数、条件类型、映射类型、
// enum、函数类型、Date、tuple）由 check 侧作为诊断拒绝（docs/WORKFLOW.md 第五节 R7）。
import ts from "typescript";

const MAX_DEPTH = 6;

/** 类型形态不被一期支持时抛出，由调用方转成诊断。 */
export class UnsupportedType extends Error {}

function docOf(symbol, tc) {
  if (!symbol) return undefined;
  const parts = symbol.getDocumentationComment(tc);
  const text = ts.displayPartsToString(parts).trim();
  return text.length > 0 ? text : undefined;
}

function primitiveName(type, tc) {
  const name = tc.typeToString(type);
  return name;
}

/**
 * @param {ts.TypeChecker} tc
 * @param {ts.Type} type
 * @param {{errors: string[], seen: Set<string>, depth: number}} ctx
 */
export function schemaForType(tc, type, ctx) {
  const { errors, seen } = ctx;
  if (ctx.depth > MAX_DEPTH) {
    errors.push(`类型递归过深：${primitiveName(type, tc)}`);
    return {};
  }

  // unknown / any：不做约束
  if (type.getFlags() & (ts.TypeFlags.Any | ts.TypeFlags.Unknown)) return {};

  // null / undefined
  if (type.getFlags() & ts.TypeFlags.Null) return { type: "null" };

  // 联合：字面量联合折成 enum，含 undefined 的当作"可选"由上层处理
  if (type.isUnion()) {
    const nonUndefined = type.types.filter(
      (t) => !(t.getFlags() & (ts.TypeFlags.Undefined | ts.TypeFlags.Void)),
    );
    if (nonUndefined.length === 0) {
      errors.push(`类型没有可用形态：${primitiveName(type, tc)}`);
      return {};
    }
    if (nonUndefined.length === 1 && nonUndefined.length !== type.types.length) {
      return schemaForType(tc, nonUndefined[0], ctx); // T | undefined → 由 required 表达可选
    }
    const literals = nonUndefined.every((t) => t.getFlags() & (ts.TypeFlags.StringLiteral | ts.TypeFlags.NumberLiteral | ts.TypeFlags.BooleanLiteral));
    if (literals) {
      const values = nonUndefined.map((t) =>
        t.getFlags() & ts.TypeFlags.BooleanLiteral ? primitiveName(t, tc) === "true" : t.value,
      );
      const kind = typeof values[0];
      return { type: kind === "string" ? "string" : kind === "number" ? "number" : "boolean", enum: values };
    }
    return { anyOf: nonUndefined.map((t) => schemaForType(tc, t, ctx)) };
  }

  if (type.getFlags() & ts.TypeFlags.Boolean) return { type: "boolean" };
  if (type.getFlags() & ts.TypeFlags.String) return { type: "string" };
  if (type.getFlags() & ts.TypeFlags.Number) return { type: "number" };

  if (type.getFlags() & ts.TypeFlags.Enum) {
    errors.push(`不支持 enum 类型：${primitiveName(type, tc)}（改用字面量联合）`);
    return {};
  }
  if (type.getFlags() & ts.TypeFlags.Date) {
    errors.push(`不支持 Date 类型：${primitiveName(type, tc)}（脚本要求决定论，用 ISO 字符串）`);
    return {};
  }

  if (type.isIntersection()) {
    const merged = {};
    for (const p of type.types) Object.assign(merged, schemaForType(tc, p, ctx));
    return merged;
  }

  // 数组与只读数组（只用公开 API：按符号名判定，再取类型实参）
  const targetName = type.symbol?.getName?.() ?? type.symbol?.name;
  if (targetName === "Array" || targetName === "ReadonlyArray") {
    const arg = tc.getTypeArguments ? tc.getTypeArguments(type) : [];
    if (!arg || arg.length !== 1) {
      errors.push(`数组需要单一元素类型：${primitiveName(type, tc)}`);
      return { type: "array", items: {} };
    }
    ctx.depth += 1;
    const items = schemaForType(tc, arg[0], ctx);
    ctx.depth -= 1;
    return { type: "array", items };
  }

  if (type.isTupleType?.()) {
    errors.push(`不支持 tuple 类型：${primitiveName(type, tc)}（改用 interface 或数组）`);
    return {};
  }

  if ((type.getFlags() & ts.TypeFlags.Object) === 0) {
    errors.push(`不支持的类型形态：${primitiveName(type, tc)}`);
    return {};
  }

  // 对象 / interface
  const symbol = type.aliasSymbol ?? type.getSymbol();
  const key = symbol ? symbol.getName() : undefined;
  if (key && key !== "__type" && seen.has(key)) {
    errors.push(`类型自引用不被支持：${key}`);
    return {};
  }
  if (key && key !== "__type") seen.add(key);

  const props = {};
  const required = [];
  for (const sym of tc.getPropertiesOfType(type)) {
    const propType = tc.getTypeOfSymbolAtLocation(sym, sym.valueDeclaration ?? type.symbol?.declarations?.[0]);
    if (!propType) continue;
    const optional =
      (sym.getFlags() & ts.SymbolFlags.Optional) !== 0 ||
      sym.declarations?.some((d) => ts.isPropertySignature(d) && d.questionToken !== undefined) ||
      hasUndefined(propType);
    ctx.depth += 1;
    const sub = schemaForType(tc, propType, ctx);
    ctx.depth -= 1;
    const desc = docOf(sym, tc);
    if (desc) sub.description = desc;
    props[sym.getName()] = sub;
    if (!optional) required.push(sym.getName());
  }
  if (key && key !== "__type") seen.delete(key);

  const out = { type: "object", properties: props };
  if (required.length > 0) out.required = required;
  const typeDesc = docOf(symbol, tc);
  if (typeDesc) out.description = typeDesc;
  return out;
}

function hasUndefined(type) {
  if (!type.isUnion()) return false;
  return type.types.some((t) => t.getFlags() & (ts.TypeFlags.Undefined | ts.TypeFlags.Void));
}
