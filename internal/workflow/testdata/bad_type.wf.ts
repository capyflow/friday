// friday-workflow
// description: 故意留下类型错误的 fixture（断言 check 会拦下它）。
// whenToUse: 仅用于测试。
const n: number = "这不是数字";
phase("占位阶段");
const r = await world.run("true", []);
return { conclusion: r.stdout, findings: [], verified: [String(n)], notCovered: [] };
