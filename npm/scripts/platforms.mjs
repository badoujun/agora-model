/**
 * npm 分发的平台映射（单一数据源）。
 *
 * 平台包名采用 Node 的 process.platform / process.arch 词汇（win32 / x64），
 * 因为 npm 的 os / cpu 字段就是这两个值；Go 的产物名用的是另一套词汇（windows / amd64）。
 * 两套词汇的对应关系只在 goAsset 里出现一次，避免各自拼字符串拼错。
 */

/**
 * npm scope。
 *
 * 为什么必须用 scope：无 scope 的 `agoramodel-<os>-<cpu>` 被 npm 服务端的
 * 「包名反垃圾 / 防抢注筛查」拒绝（403 Package name triggered spam detection）——
 * 该筛查对 `<名字>-<平台>-<架构>` 这种模式特别容易误伤，`do-harness-win32-x64`、
 * `archons-win32-x64-msvc` 都有公开记录。
 * scope 是账号独占的命名空间，包名不参与这类相似度判定。
 */
export const CLI_SCOPE = '@bakeroot';

/** 包基名，同时是命令名与产物文件名的前缀。 */
export const CLI_NAME = 'agoramodel';

/** 主包全名。 */
export const CLI_PACKAGE = `${CLI_SCOPE}/${CLI_NAME}`;

/** 主包里的可执行文件名（转发器）。 */
export const CLI_BIN = 'bin/agoramodel.js';

/**
 * 六个目标平台。
 *
 * - os / cpu：写进平台包的 package.json，npm 据此在**安装期**筛选，无需任何安装脚本
 *   （npm 11+ 默认不执行依赖脚本，postinstall 下载二进制的方案会静默失败）。
 * - goAsset：build.ps1 / make dist 产出的文件名。
 * - binName：平台包内二进制文件名（Windows 需要 .exe 后缀）。
 */
export const PLATFORMS = [
  { os: 'win32', cpu: 'x64', goAsset: 'agoramodel-windows-amd64.exe', binName: 'agoramodel.exe' },
  { os: 'win32', cpu: 'arm64', goAsset: 'agoramodel-windows-arm64.exe', binName: 'agoramodel.exe' },
  { os: 'linux', cpu: 'x64', goAsset: 'agoramodel-linux-amd64', binName: 'agoramodel' },
  { os: 'linux', cpu: 'arm64', goAsset: 'agoramodel-linux-arm64', binName: 'agoramodel' },
  { os: 'darwin', cpu: 'x64', goAsset: 'agoramodel-darwin-amd64', binName: 'agoramodel' },
  { os: 'darwin', cpu: 'arm64', goAsset: 'agoramodel-darwin-arm64', binName: 'agoramodel' },
];

/** 平台标识，形如 `win32-x64`。 */
export function platformKey(p) {
  return `${p.os}-${p.cpu}`;
}

/** 平台包全名，形如 `@bakeroot/agoramodel-win32-x64`。 */
export function packageName(p) {
  return `${CLI_SCOPE}/${CLI_NAME}-${platformKey(p)}`;
}

/** 所有平台标识（供转发器与测试比对）。 */
export function allPlatformKeys() {
  return PLATFORMS.map(platformKey);
}

/**
 * npm 打包产物的文件名，与 npm pack 对 scope 包的命名保持一致：
 * `@bakeroot/agoramodel` → `bakeroot-agoramodel-0.1.0.tgz`
 */
export function tarballFileName(pkgName, version) {
  return `${pkgName.replace(/^@/, '').replace(/\//g, '-')}-${version}.tgz`;
}
