// friday-workflow
// description: 盘点工作区源码目录：每个目录派一个子代理读一遍，再用 go build 作为确定性门，汇总成报告。
// whenToUse: 需要快速搞清楚一个陌生目录结构、并顺带确认构建是否可用时。
// args:
//   pattern:
//     type: string
//     description: 要盘点的文件 glob
//     default: "**/*.go"
//   max_dirs:
//     type: number
//     description: 最多盘点几个目录
//     default: 3

// 一期 facade 的示例脚本：既是 fixture，也是 docs/WORKFLOW.md 第四节用法的可执行版本。
// 元数据块在第一个非 `//` 行（含空行）处结束。

interface DirBrief {
  /** 目录路径（工作区相对）。 */
  path: string;
  /** 一句话说清这个目录装什么。 */
  purpose: string;
  /** 目录里是否有对外入口。 */
  hasEntry: boolean;
}

interface Finding {
  /** 位置：工作区相对路径，可带行号。 */
  where: string;
  /** 一句话说清问题或发现。 */
  what: string;
  /** 依据：读到的内容，或命令与输出。 */
  evidence: string;
  /** verified=已被独立核实；unconfirmed=未能核实，仅存疑。 */
  status: "verified" | "unconfirmed";
  /** 影响程度。数据丢失、崩溃、结果错误才用 high。 */
  severity: "low" | "medium" | "high";
}

interface WorkflowReport {
  /** 两三句话回答"这次跑得怎么样"。 */
  conclusion: string;
  findings: Finding[];
  /** 本次实际执行过的检查与命令。 */
  verified: string[];
  /** 没有覆盖到的检查及原因。 */
  notCovered: string[];
}

phase("摸清目录并逐目录阅读");
const paths = await files.glob(String(args.pattern));
const dirs = [...new Set(paths.map((f) => f.split("/").slice(0, 2).join("/")))].slice(0, Number(args.max_dirs));
log(`正在盘点 ${dirs.length} 个目录`);

const briefs = (
  await Promise.all(
    dirs.map((d) =>
      agent(`读目录·${d}`, "你只读文件、不修改任何东西，并用中文回答。").ask<DirBrief>(
        `阅读 ${d} 下的文件，说清这个目录装什么、是否有对外入口。`,
      ),
    ),
  )
).filter((b) => b !== undefined && b.path !== "");

phase("用构建作为确定性门");
const build = await world.run("go", ["build", "./..."]);

const findings: Finding[] = briefs.map((b) => ({
  where: b.path,
  what: b.purpose,
  evidence: `是否含入口：${b.hasEntry}`,
  status: "unconfirmed",
  severity: "low",
}));
if (build.exitCode !== 0) {
  findings.push({
    where: "go build ./...",
    what: "构建未通过",
    evidence: build.stderr.slice(0, 400),
    status: "verified",
    severity: "high",
  });
}

return {
  conclusion: `盘点了 ${briefs.length} 个目录；构建${build.exitCode === 0 ? "通过" : "未通过"}。目录结论均为子代理单方面说法，未经独立复核。`,
  findings,
  verified: ["go build ./..."],
  notCovered: ["测试未运行", "目录外的代码未盘点"],
};
