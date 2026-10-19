// friday-workflow
// description: T0 端到端竖切：读文件、跑命令、派两个子代理，汇总成报告。
// whenToUse: 仅用于 internal/workflow 的测试，验证宿主与 Go 侧的跨界协议。
// args:
//   greeting:
//     type: string
//     description: 写进结论里的问候语
//     default: hello
interface Note {
  /** 被描述的文件。 */
  file: string;
  /** 一句话说明它是什么。 */
  what: string;
}

phase("读一个文件并跑一条命令");
const note = await files.read("a.txt");
const say = await world.run("echo", [String(args.greeting) + " from world.run"]);
if (say.exitCode !== 0) {
  return { conclusion: "echo 未通过", findings: [], verified: [], notCovered: ["echo"] };
}
report({ stage: "read", bytes: note.length });

phase("派两个子代理并发各写一条说明");
const notes = await Promise.all([
  agent("说明员·a").ask<Note>(`用一句话说清 a.txt 是什么：${note.slice(0, 40)}`),
  agent("说明员·b").ask(`把这句话翻译成英文：${note.slice(0, 40)}`),
]);
report({ stage: "agents", count: notes.length });

return {
  conclusion: `命令输出「${say.stdout.trim()}」；拿到 ${notes.length} 条子代理结果。`,
  findings: [],
  verified: ["echo", "files.read"],
  notCovered: [],
};
