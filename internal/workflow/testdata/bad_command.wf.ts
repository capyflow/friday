// friday-workflow
// description: 故意把命令名写成变量（R1：确认卡无法静态枚举命令集）。
// whenToUse: 仅用于测试。
// args:
//   tool:
//     type: string
//     required: true
//     description: 要跑的命令名
const tool = String(args.tool);
phase("跑一条命令");
const r = await world.run(tool, ["--version"]);
return { conclusion: r.stdout, findings: [], verified: [], notCovered: [] };
