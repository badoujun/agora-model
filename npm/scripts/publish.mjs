#!/usr/bin/env node
/**
 * 发布 npm/dist/tarballs 下的 7 个包。
 *
 * 顺序固定：先发 6 个平台包，再发主包——主包的 optionalDependencies 指向平台包，
 * 反过来发会让先安装的人拿到一个拉不到二进制的版本。
 *
 * 发布的是构建阶段自己打的 tgz（不是让 npm 重新打包）：tgz 里的可执行位在构建时
 * 已按平台写正确，重新打包会把它丢掉。
 *
 * 用法：
 *   node npm/scripts/publish.mjs --dry-run            # 只走流程不发布，先看清单
 *   node npm/scripts/publish.mjs                      # 正式发布（需先 npm login）
 *   node npm/scripts/publish.mjs --tag=next           # 发到 next dist-tag
 *   node npm/scripts/publish.mjs --otp=123456         # 账号开了 2FA 时
 *   node npm/scripts/publish.mjs --registry=https://...  # 发布到私有源
 *
 * CI：在 GitHub Actions 的 OIDC 环境（npm trusted publishing）下无需任何 token，
 * 凭据由 npm CLI 在 publish 那一刻自行换取；此时不做登录预检（见 usingTrustedPublishing）。
 *
 * 前置：node npm/scripts/build.mjs
 */

import { execSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { CLI_PACKAGE, PLATFORMS, packageName, platformKey, tarballFileName } from './platforms.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const npmDir = path.resolve(here, '..');
const distDir = path.join(npmDir, 'dist');
const tarballDir = path.join(distDir, 'tarballs');

/**
 * 默认发到官方 registry。
 *
 * 刻意不沿用 `npm config get registry`：国内环境常把它设成只读镜像
 * （如 https://registry.npmmirror.com），发布必然失败且报错晦涩。
 * 需要发到私有源时显式传 --registry。
 */
const DEFAULT_REGISTRY = 'https://registry.npmjs.org';

const argv = process.argv.slice(2);
const dryRun = argv.includes('--dry-run');

function log(message) {
  process.stdout.write(`[npm-publish] ${message}\n`);
}

function fail(message) {
  process.stderr.write(`[npm-publish] ${message}\n`);
  process.exit(1);
}

/**
 * 读取 `--name=value`，并按白名单校验。
 * 这些值会被拼进 shell 命令，因此必须限定字符集（路径一类的值我们不从命令行取）。
 */
function optionValue(name, pattern) {
  const hit = argv.find((arg) => arg.startsWith(`${name}=`));
  if (!hit) {
    return undefined;
  }
  const value = hit.slice(name.length + 1);
  if (pattern && !pattern.test(value)) {
    fail(`${name} 的值含非法字符：${value}`);
  }
  return value;
}

// registry 只允许 URL 里合法的一组字符——绝不能让 `;`、`&`、`|`、空白、反引号等
// shell 元字符进来（它们会被拼进下面的 npm 命令）。
const registry =
  optionValue('--registry', /^https?:\/\/[A-Za-z0-9._\-:@[\]/]+$/) ?? DEFAULT_REGISTRY;
const tag = optionValue('--tag', /^[A-Za-z0-9._-]+$/);
const otp = optionValue('--otp', /^[0-9]{4,10}$/);

/** 供 shell 使用的引用。传入的值都已过白名单，这里只处理空格与引号。 */
function shellQuote(value) {
  if (process.platform === 'win32') {
    return /[\s"^&|<>%]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value;
  }
  return /[^A-Za-z0-9._\-/=:@+]/.test(value) ? `'${value.replace(/'/g, `'\\''`)}'` : value;
}

/**
 * 执行 npm 命令。
 *
 * Windows 上 npm 实际是 npm.cmd，只能经 shell 执行；这里传单个命令字符串而不是
 * 参数数组，避免 Node 24 的 DEP0190 弃用警告，引用也由我们自己控制。
 */
function runNpm(args, options = {}) {
  execSync(['npm', ...args.map(shellQuote)].join(' '), { stdio: 'inherit', ...options });
}

/**
 * 是否处于 CI 的 OIDC 环境（npm trusted publishing）。
 *
 * 判定条件与 npm CLI 自身一致：GitHub Actions 上同时存在
 * ACTIONS_ID_TOKEN_REQUEST_URL 与 ACTIONS_ID_TOKEN_REQUEST_TOKEN
 * （后者来自 workflow 的 `permissions: id-token: write`）。
 *
 * 该环境下 runner 上没有任何长期凭据：npm 只在 `npm publish` 那一刻用 OIDC token
 * 换短期 token，所以 `npm whoami` 必然失败，不能拿它当发布前的登录检查。
 */
function usingTrustedPublishing() {
  return Boolean(
    process.env.GITHUB_ACTIONS &&
      process.env.ACTIONS_ID_TOKEN_REQUEST_URL &&
      process.env.ACTIONS_ID_TOKEN_REQUEST_TOKEN,
  );
}

function readVersion(pkgJsonPath) {
  return JSON.parse(fs.readFileSync(pkgJsonPath, 'utf8')).version;
}

function publish(filename, label) {
  // --access public 是 scope 包必需的：scope 默认按私有处理，不显式声明会被拒
  const args = ['publish', filename, '--registry', registry, '--access', 'public'];
  if (dryRun) {
    args.push('--dry-run');
  }
  if (tag) {
    args.push('--tag', tag);
  }
  if (otp) {
    args.push('--otp', otp);
  }
  log(`${dryRun ? '[dry-run] ' : ''}发布 ${label}`);
  // cwd 设为 tgz 所在目录：命令行里只出现我们生成的文件名，不含仓库路径
  runNpm(args, { cwd: tarballDir });
}

function main() {
  const cliPkgJson = path.join(distDir, 'cli', 'package.json');
  if (!fs.existsSync(cliPkgJson)) {
    fail('未找到 npm/dist，请先执行：node npm/scripts/build.mjs');
  }
  const version = readVersion(cliPkgJson);

  const expected = [];
  for (const platform of PLATFORMS) {
    const name = packageName(platform);
    const pkgJson = path.join(distDir, 'platforms', platformKey(platform), 'package.json');
    if (!fs.existsSync(pkgJson)) {
      fail(`缺少平台包清单：${path.relative(npmDir, pkgJson)}\n请重新执行：node npm/scripts/build.mjs`);
    }
    const theirs = readVersion(pkgJson);
    if (theirs !== version) {
      fail(`版本不一致：${name} 是 ${theirs}，主包是 ${version}；请重新构建`);
    }
    expected.push({ filename: tarballFileName(name, version), label: `${name}@${version}` });
  }
  expected.push({
    filename: tarballFileName(CLI_PACKAGE, version),
    label: `${CLI_PACKAGE}@${version}`,
  });

  for (const item of expected) {
    if (!fs.existsSync(path.join(tarballDir, item.filename))) {
      fail(`缺少 tarball：${item.filename}\n请重新执行：node npm/scripts/build.mjs`);
    }
  }

  if (!dryRun) {
    if (usingTrustedPublishing()) {
      // OIDC 下没有可查询的登录身份，跳过 whoami：npm 会在下面 publish 时自己换 token。
      log('凭据：GitHub Actions OIDC（trusted publishing），跳过 whoami 预检');
    } else {
      try {
        execSync(`npm whoami --registry ${shellQuote(registry)}`, {
          stdio: ['ignore', 'pipe', 'ignore'],
        });
      } catch {
        fail(`未登录 ${registry}，请先执行：npm login --registry=${registry}`);
      }
    }
  }

  log(`registry：${registry}`);
  log(`待发布版本：${version}（${PLATFORMS.length} 个平台包 + 1 个主包）`);
  for (const item of expected) {
    publish(item.filename, item.label);
  }

  if (dryRun) {
    log('dry-run 完成：没有真正发布');
  } else {
    log(`完成：${CLI_PACKAGE}@${version} 已发布`);
    log(`安装：npm install -g ${CLI_PACKAGE}`);
  }
}

main();
