// AgoraModel 端到端冒烟共享运行库（Node，Windows / Linux / macOS 共用）。
//
// 为什么是 Node 而不是 PowerShell：冒烟脚本原先只有 tools/smoke/*.ps1，只能在 Windows 上跑，
// 且用了 curl.exe / Start-Process -WindowStyle 等 Windows 专属调用。Node 20+ 本来就是
// 构建前端（web/）的前置依赖，改写为 .mjs 后三个平台共用同一份断言，不必再维护两套脚本。
//
// 与 PowerShell 版本的对应关系：
//   Assert-That      → runSmoke() 里的 rec.assertThat()
//   Invoke-Curl      → httpRequest()
//   Start-Process    → startLogged()
//   Read-SharedText  → handle.logText / readText()
//   Find-BytesInDB   → dbContains()
//   Start-Sleep      → sleep()

import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/** 仓库根目录（本文件位于 tools/smoke/lib/）。 */
export const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..');

export const isWindows = process.platform === 'win32';

/** 本机平台的产物后缀 / Go 词汇，用于挑出「本机能跑的那份产物」。 */
export const exeSuffix = isWindows ? '.exe' : '';

/** 本机对应的 GOOS/GOARCH（与 Makefile / build.ps1 的产物命名一致）。 */
export function hostGoTarget() {
  const goos = { win32: 'windows', linux: 'linux', darwin: 'darwin' }[process.platform] ?? process.platform;
  const goarch = { x64: 'amd64', arm64: 'arm64' }[process.arch] ?? process.arch;
  return { goos, goarch };
}

/** 本机平台对应的产物文件名（如 agoramodel-linux-amd64）。 */
export function hostArtifactName() {
  const { goos, goarch } = hostGoTarget();
  return `agoramodel-${goos}-${goarch}${goos === 'windows' ? '.exe' : ''}`;
}

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * Go 构建环境：一律关闭 CGO（与 ADR-001 的发布基线一致）。
 *
 * GOPROXY 仅在调用方未显式设置时给镜像兜底：受限网络（内网 / 大陆直连）连不上
 * proxy.golang.org 时表现为 `go build` 拉依赖超时。CI 里会显式覆盖成官方源。
 */
export function goEnv(extra = {}) {
  const env = { CGO_ENABLED: '0', ...extra };
  if (!process.env.GOPROXY) {
    env.GOPROXY = 'https://goproxy.cn,direct';
  }
  return env;
}

/** 建一个本脚本私有的临时工作目录。 */
export function makeTempDir(prefix) {
  return fs.mkdtempSync(path.join(os.tmpdir(), `${prefix}-`));
}

/** 读取文本文件；不存在或暂时读不到时返回空串（对应 Read-SharedText）。 */
export function readText(file) {
  try {
    return fs.readFileSync(file, 'utf8');
  } catch {
    return '';
  }
}

// ---------------------------------------------------------------- 子进程管理

/** 当前脚本启动的全部子进程（退出时兜底清理，避免端口被占用残留）。 */
const children = new Set();

function alive(child) {
  return child && child.exitCode === null && child.signalCode === null;
}

function killChild(child) {
  if (!alive(child)) return;
  try {
    // Windows 上 SIGKILL 等价于 TerminateProcess；网关与服务包装器都是单进程，无需杀进程树。
    child.kill('SIGKILL');
  } catch {
    /* 进程可能刚好自行退出 */
  }
  children.delete(child);
}

process.on('exit', () => {
  for (const child of children) killChild(child);
});

/**
 * 启动一个把 stdout / stderr 落盘并且可随时读取的进程。
 *
 * 与 PowerShell 的 Start-Process -RedirectStandardOutput 的差异：日志用 fs.writeSync 同步
 * 写入 fd，因此进程仍在写日志时也能立刻读到（PowerShell 版本靠 FileShare.ReadWrite 绕开
 * 文件锁，Node 的读取本身就不独占）。
 *
 * @param {{name: string, command: string, args?: string[], env?: object, cwd?: string}} options
 *   name 是日志文件前缀（<name>.out / <name>.out.err）。
 */
export function startLogged({ name, command, args = [], env = {}, cwd = repoRoot }) {
  const logPath = `${name}.out`;
  const errPath = `${name}.out.err`;
  fs.mkdirSync(path.dirname(logPath), { recursive: true });
  fs.writeFileSync(logPath, '');
  fs.writeFileSync(errPath, '');

  const outFd = fs.openSync(logPath, 'a');
  const errFd = fs.openSync(errPath, 'a');

  let outText = '';
  let errText = '';

  const child = spawn(command, args, {
    cwd,
    env: { ...process.env, ...env },
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  });
  children.add(child);

  const appendOut = (chunk) => {
    outText += chunk.toString('utf8');
    try {
      fs.writeSync(outFd, chunk);
    } catch {
      /* 忽略写失败：内存副本仍然可用 */
    }
  };
  const appendErr = (chunk) => {
    errText += chunk.toString('utf8');
    try {
      fs.writeSync(errFd, chunk);
    } catch {
      /* 同上 */
    }
  };

  child.stdout.on('data', appendOut);
  child.stderr.on('data', appendErr);
  child.on('error', (err) => appendErr(Buffer.from(`[spawn error] ${err.message}\n`)));
  child.on('exit', () => children.delete(child));

  return {
    name,
    command,
    pid: child.pid,
    logPath,
    errPath,
    /** 已输出的 stdout 文本（等价于 Read-SharedText <log>）。 */
    get logText() {
      return outText;
    },
    /** 已输出的 stderr 文本。 */
    get errText() {
      return errText;
    },
    get exited() {
      return !alive(child);
    },
    get exitCode() {
      return child.exitCode;
    },
    /** 等待退出，返回是否在超时前退出。 */
    waitForExit(timeoutMs) {
      if (!alive(child)) return Promise.resolve(true);
      return new Promise((resolve) => {
        const timer = setTimeout(() => {
          child.removeListener('exit', onExit);
          resolve(false);
        }, timeoutMs);
        function onExit() {
          clearTimeout(timer);
          resolve(true);
        }
        child.once('exit', onExit);
      });
    },
    stop() {
      killChild(child);
    },
  };
}

/** 停止全部子进程（对应 PowerShell 版本 finally 里的 Stop-Process）。 */
export async function stopAll(settleMs = 400) {
  for (const child of [...children]) killChild(child);
  if (settleMs > 0) await sleep(settleMs);
}

// ------------------------------------------------------------------ HTTP 请求

function tryParseJson(text) {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

/**
 * 发起一次 HTTP 请求（对应 Invoke-Curl / Invoke-Http）。
 *
 * 与 curl 的行为对齐：连接失败 / 超时返回 code=0 而不是抛异常（调用方断言状态码即可）；
 * 不跟随重定向；headersText 是「Name: value」逐行的响应头原文，供正则断言使用。
 *
 * @param {string} url
 * @param {{method?: string, headers?: object, body?: string|Buffer, timeoutMs?: number,
 *          abortAfterMs?: number}} options
 *   abortAfterMs 用于模拟客户端中途断开（对应 curl --max-time 1）。
 */
export async function httpRequest(url, options = {}) {
  const { method = 'GET', headers = {}, body, timeoutMs = 30000, abortAfterMs } = options;
  const controller = new AbortController();
  const timers = [];
  let aborted = '';

  const abort = (reason) => {
    if (!controller.signal.aborted) {
      aborted = reason;
      controller.abort();
    }
  };
  if (timeoutMs) timers.push(setTimeout(() => abort('timeout'), timeoutMs));
  if (abortAfterMs) timers.push(setTimeout(() => abort('aborted'), abortAfterMs));

  try {
    const response = await fetch(url, {
      method,
      headers,
      body,
      signal: controller.signal,
      redirect: 'manual',
    });
    const text = await response.text();
    const lines = [...response.headers.entries()].map(([key, value]) => `${key}: ${value}`);
    // Set-Cookie 可能有多条，entries() 会合并成一条，单独补齐以便断言会话 Cookie。
    for (const cookie of response.headers.getSetCookie?.() ?? []) {
      lines.push(`set-cookie: ${cookie}`);
    }
    return {
      code: response.status,
      headersText: lines.join('\r\n'),
      body: text,
      json: tryParseJson(text),
    };
  } catch (err) {
    const reason = aborted || (err?.name === 'AbortError' ? 'aborted' : err?.message || String(err));
    return { code: 0, headersText: '', body: '', json: null, error: reason };
  } finally {
    for (const timer of timers) clearTimeout(timer);
  }
}

/** 轮询直到 URL 可用（默认 200），返回是否就绪。 */
export async function waitForHttp(url, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const res = await httpRequest(url, { timeoutMs: 2000 });
    if (res.code >= 200 && res.code < 300) return true;
    if (Date.now() >= deadline) return false;
    await sleep(200);
  }
}

// ------------------------------------------------------------------ 断言与流程

/** 断言收集器：对应 PowerShell 版本的 Assert-That / $results / 末尾统计。 */
export function createRecorder() {
  const results = [];
  const assertThat = (name, ok, detail = '') => {
    results.push({ name, ok: Boolean(ok), detail });
    console.log(`  [${ok ? 'PASS' : 'FAIL'}] ${name}${detail ? ` - ${detail}` : ''}`);
  };
  const summary = (label = '冒烟结果') => {
    const failed = results.filter((item) => !item.ok);
    console.log('');
    console.log(`== ${label}：${results.length - failed.length}/${results.length} 通过 ==`);
    if (failed.length > 0) {
      for (const item of failed) console.log(`  FAIL: ${item.name} (${item.detail})`);
      return 1;
    }
    console.log('全部通过');
    return 0;
  };
  return { assertThat, summary, results };
}

/**
 * 冒烟脚本的统一收尾：跑主流程 → 打印统计 → 清理子进程 → 返回退出码。
 * 主流程抛异常时记一条 FAIL 并给出栈，避免「脚本崩了但退出码为 0」。
 */
export async function runSmoke(main, { label = '冒烟结果' } = {}) {
  const recorder = createRecorder();
  let exitCode = 1;
  try {
    await main(recorder);
    exitCode = recorder.summary(label);
  } catch (err) {
    console.error(`[fatal] ${err?.stack ?? err}`);
    recorder.assertThat('脚本异常终止', false, String(err?.message ?? err));
    exitCode = recorder.summary(label);
  } finally {
    await stopAll();
  }
  process.exit(exitCode);
}

/** 解析 --key=value 形式的命令行参数（缺省时用 defaults）。 */
export function parseArgs(defaults = {}, argv = process.argv.slice(2)) {
  const options = { ...defaults };
  for (const arg of argv) {
    const matched = arg.match(/^--([^=]+)=(.*)$/);
    if (matched) {
      const [, key, value] = matched;
      options[key] = /^\d+$/.test(value) ? Number(value) : value;
    }
  }
  return options;
}

// -------------------------------------------------------------- 构建与网关启动

/** 执行命令并捕获输出（失败即抛错，除非 allowFailure）。 */
export function run(command, args, { cwd = repoRoot, env = {}, allowFailure = false } = {}) {
  const result = spawnSync(command, args, {
    cwd,
    env: { ...process.env, ...env },
    encoding: 'utf8',
    windowsHide: true,
  });
  if (result.error) {
    if (allowFailure) return { code: -1, stdout: '', stderr: String(result.error.message) };
    throw new Error(`无法执行 ${command}：${result.error.message}`);
  }
  if (result.status !== 0 && !allowFailure) {
    throw new Error(
      `${command} ${args.join(' ')} 失败（exit=${result.status}）\n${result.stdout ?? ''}\n${result.stderr ?? ''}`,
    );
  }
  return { code: result.status, stdout: result.stdout ?? '', stderr: result.stderr ?? '' };
}

/** 构建开发用二进制到 workDir，返回可执行文件路径。 */
export function buildGateway({ workDir, name = 'agoramodel-smoke', ldflags = '' }) {
  const exe = path.join(workDir, `${name}${exeSuffix}`);
  const args = ['build', '-trimpath'];
  if (ldflags) args.push(`-ldflags=${ldflags}`);
  args.push('-o', exe, './cmd/agoramodel');
  run('go', args, { env: goEnv() });
  return exe;
}

/**
 * 决定本次冒烟用哪个网关二进制。
 *
 * 传了 --exe=<path> 就直接复用（CI 用它验证**发布产物本身**，不必重复 go build）；
 * 否则现场构建一个开发二进制（本地开发的默认路径）。
 */
export function ensureGateway({ workDir, name, ldflags = '', exe = '' }) {
  if (!exe) {
    return buildGateway({ workDir, name, ldflags });
  }
  const resolved = path.resolve(repoRoot, exe);
  if (!fs.existsSync(resolved)) {
    throw new Error(`--exe 指定的二进制不存在：${resolved}`);
  }
  return resolved;
}

/** 启动 mock 上游并等待其监听就绪（原来的 Start-Sleep 2 秒改成就绪探测）。 */
export async function startMock({ workDir, port = 9999, name = 'mock' }) {
  const mock = startLogged({
    name: path.join(workDir, name),
    command: 'node',
    args: ['--no-deprecation', 'tools/mock-upstream/server.mjs'],
    env: { PORT: String(port) },
  });
  const ready = await waitForHttp(`http://127.0.0.1:${port}/v1/models`, 15000);
  if (!ready) {
    throw new Error(`mock 上游未在 ${port} 端口监听\n--- mock 输出 ---\n${mock.logText}\n${mock.errText}`);
  }
  return mock;
}

/** 启动网关进程（不等待就绪，由调用方决定等日志 / 等退出）。 */
export function startGateway({
  exe,
  workDir,
  name,
  config,
  dataDir,
  port,
  extraArgs = [],
  env = {},
  logLevel = 'debug',
}) {
  return startLogged({
    name: path.join(workDir, name),
    command: exe,
    args: ['--config', config, '--data-dir', dataDir, '--port', String(port), '--log-level', logLevel, ...extraArgs],
    env,
  });
}

/** 从启动日志里等待网关 Key 明文（Phase 2 起由数据库生成，只在首次启动打印一次）。 */
export async function waitForGatewayKey(handle, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const matched = handle.logText.match(/gateway_key=(gw-[0-9a-f]+)/);
    if (matched) return matched[1];
    if (Date.now() >= deadline) return null;
    await sleep(200);
  }
}

/** 在 SQLite 主库与 WAL 里按字节搜索字符串（断言明文凭证没有落库 / 日志已落库）。 */
export function dbContains(dataDir, needle) {
  const target = Buffer.from(needle, 'utf8');
  for (const file of ['agora.db', 'agora.db-wal']) {
    if (fs.existsSync(path.join(dataDir, file)) && fs.readFileSync(path.join(dataDir, file)).includes(target)) {
      return true;
    }
  }
  return false;
}

/** 取进程常驻内存（字节），用于性能冒烟的「内存增长可控」断言。 */
export function processRssBytes(pid) {
  try {
    if (isWindows) {
      const out = run(
        'powershell',
        ['-NoProfile', '-Command', `(Get-Process -Id ${pid}).WorkingSet64`],
        { allowFailure: true },
      );
      const value = Number.parseInt(out.stdout.trim(), 10);
      return Number.isFinite(value) ? value : 0;
    }
    if (process.platform === 'linux') {
      // /proc/<pid>/statm 第二个字段是常驻页数
      const fields = readText(`/proc/${pid}/statm`).trim().split(/\s+/);
      const pages = Number.parseInt(fields[1] ?? '0', 10);
      return Number.isFinite(pages) ? pages * 4096 : 0;
    }
    // macOS / BSD：ps -o rss= 输出 KB
    const out = run('ps', ['-o', 'rss=', '-p', String(pid)], { allowFailure: true });
    const kb = Number.parseInt(out.stdout.trim(), 10);
    return Number.isFinite(kb) ? kb * 1024 : 0;
  } catch {
    return 0;
  }
}

/** 递归复制目录（Node 16.7+ 自带 fs.cpSync）。 */
export function copyDir(src, dest) {
  fs.rmSync(dest, { recursive: true, force: true });
  fs.cpSync(src, dest, { recursive: true });
}

/** 写入 UTF-8 文本（无 BOM，对应 PowerShell 的 UTF8Encoding($false)）。 */
export function writeText(file, content) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content, 'utf8');
  return file;
}
