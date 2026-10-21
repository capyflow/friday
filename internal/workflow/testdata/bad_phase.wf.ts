// friday-workflow
// description: 故意违反三条 lint：ask 落在任何 phase 之前（R3）、末尾阶段没有步子（R2）、使用 Date.now（R8）。
// whenToUse: 仅用于测试。
const started = Date.now();
const text = await agent("无阶段归属的写手").ask("随便写一句话。");

phase("收尾");
const r = await world.run("true", []);

phase("末尾空阶段");
return { conclusion: text + String(started) + r.stdout, findings: [], verified: [], notCovered: [] };
