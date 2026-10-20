#!/usr/bin/env node
/**
 * 把 dist/ 下的 Go 产物打包成 npm 可分发的 7 个包：
 *   1 个主包 agoramodel + 6 个平台包 agoramodel-<os>-<cpu>
 * 输出到 npm/dist/（不提交）。
 *
 * 用法：
 *   node npm/scripts/build.mjs [--version=1.2.3]
 *
 * 前置：先产出 Go 产物
 *   pwsh -File build.ps1 -Target dist      # Windows
 *   make dist                              # Linux / macOS
 */

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

import {
  CLI_BIN,
  CLI_NAME,
  CLI_PACKAGE,
  PLATFORMS,
  packageName,
  platformKey,
  tarballFileName,
} from './platforms.mjs';
import { createPackageTarball } from './tarball.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const npmDir = path.resolve(here, '..');
const repoRoot = path.resolve(npmDir, '..');
const outDir = path.join(npmDir, 'dist');
const goDistDir = path.join(repoRoot, 'dist');
const shimPath = path.join(npmDir, 'bin', 'agoramodel.js');
const readmePath = path.join(npmDir, 'README.md');
const licensePath = path.join(repoRoot, 'LICENSE');

/** 仓库尚无 tag 时的回落版本；正式发布请显式传 --version。
 *  与 cmd/agoramodel/main.go 内置的版本号保持一致，避免 npm 包与控制台显示不一致。 */
const DEFAULT_VERSION = '0.2.0';
const NODE_ENGINES = '>=18';

function log(message) {
  process.stdout.write(`[npm-build] ${message}\n`);
}

function fail(message) {
  process.stderr.write(`[npm-build] ${message}\n`);
  process.exit(1);
}

function git(args) {
  try {
    return execFileSync('git', args, {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    return '';
  }
}

/** 把 git describe 的输出规范成合法 semver；不是 semver 则返回空串。 */
function normalizeVersion(raw) {
  if (!raw) {
    return '';
  }
  let value = String(raw).trim().replace(/^v/, '');
  // 1.2.3-4-gabcdef(-dirty) → 1.2.3-4.gabcdef(.dirty)：git describe 的形式不是合法 semver
  const described = value.match(/^(\d+\.\d+\.\d+)-(\d+)-g([0-9a-fA-F]+)(-dirty)?$/);
  if (described) {
    value = `${described[1]}-${described[2]}.g${described[3]}${described[4] ? '.dirty' : ''}`;
  }
  return /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(value) ? value : '';
}

function resolveVersion() {
  const flag = process.argv.find((arg) => arg.startsWith('--version='));
  if (flag) {
    const value = normalizeVersion(flag.slice('--version='.length));
    if (!value) {
      fail(`--version 不是合法 semver：${flag.slice('--version='.length)}`);
    }
    return value;
  }

  if (process.env.AGORAMODEL_VERSION) {
    const value = normalizeVersion(process.env.AGORAMODEL_VERSION);
    if (!value) {
      fail(`AGORAMODEL_VERSION 不是合法 semver：${process.env.AGORAMODEL_VERSION}`);
    }
    return value;
  }

  const derived = normalizeVersion(git(['describe', '--tags', '--always', '--dirty']));
  if (derived) {
    log(`版本来自 git describe：${derived}`);
    return derived;
  }

  log(`仓库尚无 semver tag，回落到 ${DEFAULT_VERSION}；正式发布请传 --version=X.Y.Z`);
  return DEFAULT_VERSION;
}

/**
 * 从 git remote 推出 GitHub 的 owner / repo；不是 GitHub 地址时返回 null。
 *
 * 需要覆盖的 remote 形式：
 *   https://github.com/owner/repo.git
 *   git@github.com:owner/repo.git              （scp 风格，冒号是分隔符）
 *   ssh://git@ssh.github.com:443/owner/repo.git（URL 风格，冒号后面是端口）
 *
 * 端口是最容易踩的一处：`ssh.github.com:443/owner/repo` 里的 443 是端口而不是
 * owner。早先直接对整串 remote 用 `/github\.com[/:]([^/]+)\//` 取 owner，会把
 * `443` 抓进去，于是发出去的包 homepage 变成 github.com/443/owner/repo。
 */
function parseGitHubRemote(raw) {
  if (!raw) {
    return null;
  }

  // 1. 去掉 scheme://（https:// / ssh:// / git:// …）
  let value = String(raw).trim().replace(/^[a-z][a-z0-9+.-]*:\/\//i, '');
  // 2. 去掉 userinfo（git@ / x-access-token@）
  value = value.replace(/^[^@/]+@/, '');

  // 3. 切出 host 与剩余部分；host 与路径之间可能是 '/' 也可能是 ':'。
  const hostMatch = value.match(/^([^/:]+)(.*)$/);
  if (!hostMatch) {
    return null;
  }
  const host = hostMatch[1].toLowerCase();
  // 白名单而不是 endsWith：只有 github.com 与 SSH over 443 的 ssh.github.com 会出现在本仓库。
  if (host !== 'github.com' && host !== 'ssh.github.com') {
    return null;
  }

  // 4. 去掉端口（`:443/...`）或 scp 分隔冒号（`:owner/...`），再吃掉开头的 '/'
  const rest = hostMatch[2]
    .replace(/^:\d+(?=\/)/, '')
    .replace(/^:/, '')
    .replace(/^\//, '');

  const parts = rest.replace(/\.git$/i, '').split('/');
  // GitHub 仓库固定是 owner/repo 两级；多出来的路径段说明不是仓库地址。
  if (parts.length !== 2 || !parts[0] || !parts[1]) {
    return null;
  }
  return { owner: parts[0], repo: parts[1] };
}

/** 从 git remote 推导仓库元信息；推不出来就不写这些字段（不编造）。 */
function repositoryInfo() {
  const parsed = parseGitHubRemote(git(['remote', 'get-url', 'origin']));
  if (!parsed) {
    return {};
  }
  const url = `https://github.com/${parsed.owner}/${parsed.repo}`;
  return {
    homepage: url,
    repository: { type: 'git', url: `${url}.git` },
    bugs: { url: `${url}/issues` },
  };
}

function writeJson(file, value) {
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

/**
 * 从仓库根的 LICENSE 读取 SPDX 标识与版权持有者。
 * 以 LICENSE 为单一数据源，避免 package.json 里的 license / author 与许可证文本漂移。
 */
function readLicenseInfo() {
  const text = fs.readFileSync(licensePath, 'utf8');
  const id = text.includes('MIT License') ? 'MIT' : '';
  if (!id) {
    fail(`无法从 ${path.relative(repoRoot, licensePath)} 识别许可证类型（目前只认 MIT License 首行）`);
  }
  const holder = text.match(/^Copyright \(c\) \d{4} (.+)$/m)?.[1]?.trim() ?? '';
  return { id, holder };
}

function platformManifest(platform, version, repo, license) {
  return {
    name: packageName(platform),
    version,
    description: `AgoraModel 网关（${platform.os} ${platform.cpu}）预编译二进制；由主包 ${CLI_PACKAGE} 在安装时按平台自动选用，请勿手动引入`,
    os: [platform.os],
    cpu: [platform.cpu],
    files: [`bin/${platform.binName}`],
    license: license.id,
    // yarn berry：包内是直接执行的二进制，不应被压缩进 .yarn/cache
    preferUnplugged: true,
    ...repo,
  };
}

function cliManifest(version, repo, license) {
  return {
    name: CLI_PACKAGE,
    version,
    description:
      '本地优先的 AI 模型接入网关 + 配置控制台：所有 Agent 只对接一个固定入口，新增供应商只配置一次（单文件二进制，零运行时依赖）',
    keywords: ['ai', 'gateway', 'openai', 'llm', 'proxy', 'claude', 'codex', 'anthropic', 'router'],
    license: license.id,
    ...(license.holder ? { author: license.holder } : {}),
    ...repo,
    // bin 的 key 是**命令名**，不是包名：scope 包安装后命令仍叫 agoramodel
    bin: { [CLI_NAME]: CLI_BIN },
    files: [CLI_BIN],
    engines: { node: NODE_ENGINES },
    // 平台包由 npm 在安装期按 os / cpu 自动筛选。
    // 这里必须是 optionalDependencies 而不是 postinstall 下载：npm 11+ 默认不执行依赖脚本。
    optionalDependencies: Object.fromEntries(PLATFORMS.map((p) => [packageName(p), version])),
  };
}

function main() {
  const version = resolveVersion();
  const repo = repositoryInfo();

  if (!fs.existsSync(shimPath)) {
    fail(`缺少转发器 ${path.relative(repoRoot, shimPath)}`);
  }
  if (!fs.existsSync(readmePath)) {
    fail(`缺少 ${path.relative(repoRoot, readmePath)}`);
  }
  if (!fs.existsSync(licensePath)) {
    fail(`缺少 ${path.relative(repoRoot, licensePath)}`);
  }
  const license = readLicenseInfo();

  const missing = PLATFORMS.map((p) => path.join(goDistDir, p.goAsset)).filter(
    (file) => !fs.existsSync(file)
  );
  if (missing.length > 0) {
    fail(
      `缺少 ${missing.length} 个 Go 产物：\n  ${missing
        .map((file) => path.relative(repoRoot, file))
        .join('\n  ')}\n请先执行：pwsh -File build.ps1 -Target dist（或 make dist）`
    );
  }

  fs.rmSync(outDir, { recursive: true, force: true });

  for (const platform of PLATFORMS) {
    const dir = path.join(outDir, 'platforms', platformKey(platform));
    const binDir = path.join(dir, 'bin');
    fs.mkdirSync(binDir, { recursive: true });

    const target = path.join(binDir, platform.binName);
    fs.copyFileSync(path.join(goDistDir, platform.goAsset), target);
    if (platform.binName.endsWith('.exe') === false) {
      // Windows 上 chmod 是空操作，这里只是让本地产物目录看起来正确；
      // 真正决定 tgz 权限位的是 tarball.mjs（它在 tar 头里显式写入 mode）。
      fs.chmodSync(target, 0o755);
    }
    fs.copyFileSync(licensePath, path.join(dir, 'LICENSE'));
    writeJson(path.join(dir, 'package.json'), platformManifest(platform, version, repo, license));
    log(`${packageName(platform)} → ${path.relative(repoRoot, dir)}`);
  }

  const cliDir = path.join(outDir, 'cli');
  fs.mkdirSync(path.join(cliDir, 'bin'), { recursive: true });
  fs.copyFileSync(shimPath, path.join(cliDir, CLI_BIN));
  fs.copyFileSync(readmePath, path.join(cliDir, 'README.md'));
  fs.copyFileSync(licensePath, path.join(cliDir, 'LICENSE'));
  writeJson(path.join(cliDir, 'package.json'), cliManifest(version, repo, license));
  log(`${CLI_PACKAGE} → ${path.relative(repoRoot, cliDir)}`);

  // 自己打 tgz 而不是调 `npm pack`：Windows 上 chmod 是空操作，npm pack 出来的
  // Linux / macOS 平台包里二进制会是 644，装到 Linux 上无法执行。
  const tarballDir = path.join(outDir, 'tarballs');
  const packTargets = [
    ...PLATFORMS.map((platform) => ({
      dir: path.join(outDir, 'platforms', platformKey(platform)),
      label: packageName(platform),
    })),
    { dir: cliDir, label: CLI_PACKAGE },
  ];

  for (const target of packTargets) {
    const filename = tarballFileName(target.label, version);
    const packed = createPackageTarball(target.dir, path.join(tarballDir, filename));
    const size = (packed.tarballBytes / 1024 / 1024).toFixed(2);
    log(`${filename}（${packed.files.length} 个文件，${size} MB）`);
  }

  log(`完成：${PLATFORMS.length + 1} 个包，版本 ${version}`);
  log(`产物：${path.relative(repoRoot, tarballDir)}`);
  log('下一步：node npm/scripts/publish.mjs --dry-run');
}

// 导出纯函数供测试使用；只有直接执行本文件时才真正构建。
export { cliManifest, normalizeVersion, parseGitHubRemote, platformManifest, repositoryInfo };

const invokedDirectly =
  process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href;

if (invokedDirectly) {
  main();
}
