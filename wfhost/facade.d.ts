/**
 * Friday 动态工作流 facade（一期子集）。
 *
 * 本文件只供 `wf-host check` 的类型检查使用——脚本里不要照抄 `declare`：
 * 脚本被编译在一个 async 函数体内，ambient 声明在函数体内非法（同 ZCode 的规则）。
 * 一期不提供：hole()、artifact.*、model() 与 per-agent 模型路由、升级提问。
 */

/** 一次 ask 的待决结果。await 它，或与别的节点一起交给 Promise.all。 */
interface WfNode<T> extends PromiseLike<T> {}

/** 子代理的人格，在 agent() 创建时冻结。所有子代理的工作工具面由 Go 侧统一决定。 */
interface WfPersona {
  /** 描述该角色与判断标准的系统提示词。 */
  system?: string;
}

/**
 * 子代理 = 一段独立会话，跨多次 ask 保留上下文。
 * 同一子代理上的 ask 排队串行（先入先出）——这就是"校准"的代价。
 */
interface WfAgent {
  /**
   * 交一个任务给该子代理。给了类型参数时，回答是按合成出的 schema 校验过的值；
   * 不给时，回答是子代理的最终文本。类型参数必须是本脚本里声明的 interface 或 type alias。
   */
  ask<T = string>(instructions: string): WfNode<T>;
}

/** 创建一个子代理。每次调用都是全新空会话；要共享上下文就共享返回值。 */
declare function agent(name?: string, persona?: string | WfPersona): WfAgent;

/** 标记一个阶段的开始：只影响进度呈现，不返回任何东西。名字必须是编译期字面量，且必填。 */
declare function phase(name: string): void;

/** 运行一条命令的结果。非零退出码是**返回值**，不是异常——去读 exitCode。 */
interface WorldRunResult {
  /** 进程退出码。 */
  exitCode: number;
  /** 捕获到的 stdout（UTF-8，有上限）。 */
  stdout: string;
  /** 捕获到的 stderr（UTF-8，有上限）。 */
  stderr: string;
}

/** 执行命令。cmd 必须是编译期字符串字面量（确认卡要整批展示），运行期值只进 args。 */
declare const world: {
  run(cmd: string, args?: string[], opts?: { timeoutMs?: number }): WfNode<WorldRunResult>;
};

/** files.grep 的单条命中。 */
interface GrepMatch {
  /** 工作区相对路径。 */
  path: string;
  /** 一基行号。 */
  line: number;
  /** 命中行全文。 */
  text: string;
}

/** 工作区的只读视图。结果会被记录，脚本侧没有任何写能力——写是子代理的活。 */
declare const files: {
  /** 匹配 glob 的工作区相对路径，已排序。命中过多会 reject（不截断）。 */
  glob(pattern: string): WfNode<string[]>;
  /** 以 UTF-8 读一个工作区文件。有大小上限。 */
  read(path: string): WfNode<string>;
  /** 用正则搜内容，可用 glob 收窄范围。命中过多会 reject（不截断）。 */
  grep(pattern: string, glob?: string): WfNode<GrepMatch[]>;
};

/** git.status() 的工作区状态。 */
interface GitStatus {
  /** 当前分支；HEAD 游离时缺省。 */
  branch?: string;
  /** 无任何已暂存/修改/未跟踪内容时为 true。 */
  clean: boolean;
  staged: string[];
  unstaged: string[];
  untracked: string[];
}

/** git.log() 的一条提交。 */
interface GitCommit {
  hash: string;
  /** 提交信息首行。 */
  subject: string;
  author: string;
  /** ISO 8601 作者日期。 */
  date: string;
}

/** 只读 git 视图，同样被记录。不在仓库里时每个调用都会可捕获地 reject。 */
declare const git: {
  /** 变更过的文件（工作区相对）。给了 base 就与该 ref 比。 */
  changedFiles(base?: string): WfNode<string[]>;
  /** 与 base（默认 HEAD）的 unified diff，可收窄到一个路径。 */
  diff(base?: string, path?: string): WfNode<string>;
  /** 当前工作区状态。 */
  status(): WfNode<GitStatus>;
  /** 最近若干条提交，新在前。默认 20。 */
  log(count?: number): WfNode<GitCommit[]>;
};

/** 本次运行的入参：来自流程定义头部的声明，已按类型与默认值校验过。 */
declare const args: Readonly<Record<string, unknown>>;

/** 给人看的进度叙述。 */
declare function log(message: string): void;

/**
 * 即时上报一条中间结果。上报项会随完成通知送达，**run 失败也送**——
 * 所以边跑边报，不要把 findings 攒到最后的数组里 return。
 */
declare function report(item: unknown): void;

/** 两个阶段之间的类型化流：一个阶段 send()，另一个用 for await 抽干。 */
interface WfChannel<T> extends AsyncIterable<T> {
  /** 入队一条；同步、永不等待。close() 之后再 send 抛 ChannelClosed。 */
  send(item: T): void;
  /** 结束流。幂等。没有生产者会再 send 却也没 close 的 channel 会让 run 以 ChannelDeadlock 失败。 */
  close(): void;
}

/** 创建一个 channel。name 只用于死锁信息，不是身份。 */
declare function channel<T>(name?: string): WfChannel<T>;

/** 把一个 async 块作为并发阶段立刻启动，返回它的 promise。 */
declare function future<T>(body: () => Promise<T>): Promise<T>;
