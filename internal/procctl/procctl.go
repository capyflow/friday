// procctl 收拢子进程的平台差异，供 ACP agent（internal/acp）、MCP 服务器
// （internal/mcp）与安装命令共用同一套纪律：
//
//   - Prepare（Start 前）：Unix 设独立进程组，Windows 设 CREATE_NO_WINDOW——
//     GUI/sidecar 进程拉起 npx 这类控制台程序时不闪黑窗。
//   - Attach（Start 后）：挂接"整树击杀"载体。Unix 的进程组在 Prepare 已就位；
//     Windows 把子进程塞进 Job Object（KILL_ON_JOB_CLOSE + TerminateJobObject），
//     击杀 npx→node→agent 自 spawn 的孙进程，且父进程崩溃时内核回收 job 句柄
//     也连带终结整树。参考 Zed crates/util/src/process.rs 的同款实现。
//   - ShellCommandContext：跑一条 shell 命令行（安装命令等固定可信字符串）。
//     Unix 走 sh -c；Windows 优先 Git Bash（官方安装脚本多为 curl|bash），
//     找不到退 PowerShell（npm install -g 这类仍可用）。
//
// 已知竞态（Zed 同款，接受）：Windows 上 Start 返回与 AssignProcessToJobObject
// 之间出生的孙进程不在 job 里，整树击杀会漏掉；彻底修好需要 CREATE_SUSPENDED，
// 代价不值。
package procctl
