#!/usr/bin/env node
'use strict';

/**
 * agoramodel 的 npm 入口：把调用原样转发给当前平台的预编译二进制。
 *
 * 二进制由 optionalDependencies 里的平台包提供（agoramodel-<os>-<cpu>）。
 * 之所以不用 postinstall 下载：npm 11+ 默认不执行依赖的安装脚本（本项目构建时
 * 就踩过 esbuild 的 allow-scripts 提示），postinstall 方案会静默失败；
 * 而 os / cpu 字段的筛选发生在安装期，由 npm 自己完成，不需要执行任何脚本。
 *
 * 该转发器只负责交互式调用；注册系统服务时（agoramodel install）真正的服务进程
 * 是平台包里的二进制副本，与这里无关。
 */

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

// 与 npm/scripts/platforms.mjs 保持一致（npm/test/npm-package.test.mjs 会断言这点）。
// 必须用 scope：无 scope 的 agoramodel-<os>-<cpu> 会被 npm 服务端的
// 「包名反垃圾 / 防抢注筛查」拒绝（403 Package name triggered spam detection）。
const PACKAGE_SCOPE = '@bakeroot';
const PACKAGE_NAME = 'agoramodel';
const PACKAGE_FULL_NAME = `${PACKAGE_SCOPE}/${PACKAGE_NAME}`;

const RELEASES_URL = 'https://github.com/badoujun/agora-model/releases';

/** 与 npm/scripts/platforms.mjs 保持一致（npm/test/platforms.test.cjs 会断言这点）。 */
const SUPPORTED_PLATFORMS = [
  'win32-x64',
  'win32-arm64',
  'linux-x64',
  'linux-arm64',
  'darwin-x64',
  'darwin-arm64',
];

function platformKey(platform = process.platform, arch = process.arch) {
  return `${platform}-${arch}`;
}

function isSupported(key) {
  return SUPPORTED_PLATFORMS.includes(key);
}

function binaryName(platform = process.platform) {
  return platform === 'win32' ? `${PACKAGE_NAME}.exe` : PACKAGE_NAME;
}

/**
 * 解析平台包里二进制的绝对路径。
 *
 * 用 require.resolve 而不是硬编码 node_modules 路径：npm / pnpm / yarn 对
 * 依赖的提升（hoisting）布局各不相同，require.resolve 由 Node 按各自的解析规则处理。
 */
function resolveBinary(platform = process.platform, arch = process.arch) {
  const key = platformKey(platform, arch);
  if (!isSupported(key)) {
    throw new Error(
      `不支持的平台：${key}\n已发布的平台：${SUPPORTED_PLATFORMS.join('、')}\n` +
        `可改用单文件二进制：${RELEASES_URL}`
    );
  }

  const pkg = `${PACKAGE_SCOPE}/${PACKAGE_NAME}-${key}`;
  let pkgJson;
  try {
    pkgJson = require.resolve(`${pkg}/package.json`);
  } catch {
    throw new Error(
      `未找到平台包 ${pkg}。\n` +
        `请重新安装：npm install -g ${PACKAGE_FULL_NAME}\n` +
        `若使用 pnpm / yarn，请确认没有禁用可选依赖（--no-optional / --ignore-optional）。`
    );
  }
  return path.join(path.dirname(pkgJson), 'bin', binaryName(platform));
}

/** 兜底恢复可执行位：个别 npm 客户端解包时会丢掉 tar 里的 mode。 */
function ensureExecutable(binary, platform = process.platform) {
  if (platform === 'win32') {
    return;
  }
  try {
    fs.accessSync(binary, fs.constants.X_OK);
  } catch {
    try {
      fs.chmodSync(binary, 0o755);
    } catch {
      // 恢复失败就把问题留给 spawn，它会给出更具体的错误
    }
  }
}

function main() {
  let binary;
  try {
    binary = resolveBinary();
  } catch (err) {
    process.stderr.write(`${err.message}\n`);
    process.exit(1);
  }
  ensureExecutable(binary);

  const child = spawn(binary, process.argv.slice(2), { stdio: 'inherit' });

  child.on('error', (err) => {
    process.stderr.write(`启动 ${binary} 失败：${err.message}\n`);
    process.exit(1);
  });

  // 只在非 Windows 转发信号：Windows 没有真正的信号语义，child.kill() 是直接终止进程，
  // 反而会打断 Go 侧的优雅关闭；而共享控制台下的 Ctrl+C 本来就会同时送达子进程。
  if (process.platform !== 'win32') {
    for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
      process.on(signal, () => {
        if (!child.killed) {
          try {
            child.kill(signal);
          } catch {
            // 子进程可能已退出
          }
        }
      });
    }
  }

  child.on('exit', (code, signal) => {
    if (signal) {
      // 以同种信号结束自己，让上层看到真实的终止原因
      try {
        process.kill(process.pid, signal);
      } catch {
        process.exit(1);
      }
      return;
    }
    process.exit(code === null ? 1 : code);
  });
}

if (require.main === module) {
  main();
}

module.exports = {
  SUPPORTED_PLATFORMS,
  platformKey,
  isSupported,
  binaryName,
  resolveBinary,
};
