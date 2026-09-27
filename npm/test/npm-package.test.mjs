import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createRequire } from 'node:module';
import { test } from 'node:test';

import { cliManifest, normalizeVersion, platformManifest } from '../scripts/build.mjs';
import {
  CLI_NAME,
  CLI_PACKAGE,
  CLI_SCOPE,
  PLATFORMS,
  allPlatformKeys,
  packageName,
  platformKey,
  tarballFileName,
} from '../scripts/platforms.mjs';

// 转发器是 CommonJS（要能 require.resolve 定位平台包），这里用 createRequire 加载它。
const require = createRequire(import.meta.url);
const shim = require('../bin/agoramodel.js');

/** 与仓库根 LICENSE 对应的包元数据（build.mjs 从 LICENSE 解析出同样的内容）。 */
const TEST_LICENSE = { id: 'MIT', holder: 'badoujun' };

test('平台表覆盖六个目标平台', () => {
  assert.deepEqual(allPlatformKeys().sort(), [
    'darwin-arm64',
    'darwin-x64',
    'linux-arm64',
    'linux-x64',
    'win32-arm64',
    'win32-x64',
  ]);
});

test('转发器的平台表与 platforms.mjs 保持一致', () => {
  // 两处各有一份清单（shim 不能 import ESM），这个断言防止它们漂移。
  assert.deepEqual([...shim.SUPPORTED_PLATFORMS].sort(), allPlatformKeys().sort());
});

test('平台包名由 scope + os / cpu 推导', () => {
  for (const platform of PLATFORMS) {
    const name = packageName(platform);
    assert.equal(name, `${CLI_SCOPE}/${CLI_NAME}-${platformKey(platform)}`);
    // 必须是 scope 包：无 scope 的 agoramodel-<os>-<cpu> 会被 npm 的包名反垃圾筛查拒绝
    assert.ok(name.startsWith('@'), `${name} 应带 scope`);
  }
  assert.equal(CLI_PACKAGE, `${CLI_SCOPE}/${CLI_NAME}`);
});

test('tarball 文件名与 npm pack 对 scope 包的命名一致', () => {
  assert.equal(tarballFileName('@bakeroot/agoramodel', '0.1.0'), 'bakeroot-agoramodel-0.1.0.tgz');
  assert.equal(
    tarballFileName('@bakeroot/agoramodel-win32-x64', '0.1.0'),
    'bakeroot-agoramodel-win32-x64-0.1.0.tgz'
  );
  // 无 scope 的包名也能正常处理
  assert.equal(tarballFileName('plain-pkg', '1.2.3'), 'plain-pkg-1.2.3.tgz');
});

test('转发器按平台选择二进制名', () => {
  assert.equal(shim.binaryName('win32'), 'agoramodel.exe');
  assert.equal(shim.binaryName('linux'), 'agoramodel');
  assert.equal(shim.binaryName('darwin'), 'agoramodel');
});

test('转发器识别支持的平台', () => {
  assert.equal(shim.isSupported('linux-x64'), true);
  assert.equal(shim.isSupported('win32-arm64'), true);
  assert.equal(shim.isSupported('sunos-x64'), false);
  assert.equal(shim.isSupported('linux-ia32'), false);
});

test('转发器在不支持的平台上给出可读错误', () => {
  assert.throws(
    () => shim.resolveBinary('sunos', 'sparc'),
    (err) => {
      assert.match(err.message, /不支持的平台：sunos-sparc/);
      assert.match(err.message, /releases/);
      return true;
    }
  );
});

test('转发器在缺少平台包时给出安装提示', () => {
  // 本仓库的 node_modules 里不会存在这些平台包，因此必然走到「未找到」分支。
  assert.throws(
    () => shim.resolveBinary('linux', 'arm64'),
    (err) => {
      assert.match(err.message, /未找到平台包 @bakeroot\/agoramodel-linux-arm64/);
      assert.match(err.message, /npm install -g @bakeroot\/agoramodel/);
      return true;
    }
  );
});

test('主包清单：bin、许可证、引擎与可选依赖', () => {
  const manifest = cliManifest('1.2.3', {}, TEST_LICENSE);

  assert.equal(manifest.name, CLI_PACKAGE);
  assert.equal(manifest.name, '@bakeroot/agoramodel');
  assert.equal(manifest.version, '1.2.3');
  // bin 的 key 是命令名（不含 scope）：装完命令仍叫 agoramodel
  assert.deepEqual(manifest.bin, { agoramodel: 'bin/agoramodel.js' });
  assert.equal(manifest.engines.node, '>=18');
  assert.equal(manifest.license, 'MIT');
  assert.equal(manifest.author, 'badoujun');

  // 关键：平台包必须以 optionalDependencies 声明，且版本与主包严格一致，
  // 否则 npm 会装上不匹配的二进制（或干脆装不上任何二进制）。
  const expected = PLATFORMS.map((p) => packageName(p)).sort();
  assert.deepEqual(Object.keys(manifest.optionalDependencies).sort(), expected);
  for (const [name, version] of Object.entries(manifest.optionalDependencies)) {
    assert.equal(version, '1.2.3', `${name} 的版本应与主包一致`);
  }

  // 绝不能出现 postinstall 之类的安装脚本：npm 11+ 默认不执行依赖脚本。
  assert.equal(manifest.scripts, undefined);
});

test('主包清单不会被写入平台限制', () => {
  const manifest = cliManifest('1.2.3', {}, TEST_LICENSE);
  assert.equal(manifest.os, undefined);
  assert.equal(manifest.cpu, undefined);
});

test('平台包清单：os / cpu、许可证与二进制路径', () => {
  for (const platform of PLATFORMS) {
    const manifest = platformManifest(platform, '1.2.3', {}, TEST_LICENSE);

    assert.equal(manifest.name, packageName(platform));
    assert.equal(manifest.version, '1.2.3');
    // os / cpu 正是 npm 在安装期筛选平台包的依据。
    assert.deepEqual(manifest.os, [platform.os]);
    assert.deepEqual(manifest.cpu, [platform.cpu]);
    assert.deepEqual(manifest.files, [`bin/${platform.binName}`]);
    assert.equal(manifest.license, 'MIT');
    assert.equal(manifest.preferUnplugged, true);
    assert.equal(manifest.scripts, undefined);
    // 平台包不设 exports：转发器需要 require.resolve('<pkg>/package.json')
    assert.equal(manifest.exports, undefined);
    // 平台包不设 bin：它不是命令入口
    assert.equal(manifest.bin, undefined);
  }
});

test('仓库根 LICENSE 是 MIT 且署名与包元数据一致', () => {
  const text = fs.readFileSync(new URL('../../LICENSE', import.meta.url), 'utf8');
  assert.ok(text.startsWith('MIT License\n'), 'LICENSE 首行应为 "MIT License"');
  assert.match(text, /^Copyright \(c\) \d{4} badoujun$/m, '版权行应与 package.json 的 author 一致');
  // 标准 MIT 文本的两段关键声明
  assert.match(text, /Permission is hereby granted, free of charge/);
  assert.match(text, /THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND/);
});

test('平台包的 os 取值必须是 Node 的 process.platform 词汇', () => {
  for (const platform of PLATFORMS) {
    assert.ok(
      ['win32', 'linux', 'darwin'].includes(platform.os),
      `os 必须是 Node 词汇，得到 ${platform.os}`
    );
    assert.ok(['x64', 'arm64'].includes(platform.cpu), `cpu 取值异常：${platform.cpu}`);
  }
});

test('版本规范化只接受合法 semver', () => {
  assert.equal(normalizeVersion('1.2.3'), '1.2.3');
  assert.equal(normalizeVersion('v1.2.3'), '1.2.3');
  assert.equal(normalizeVersion('1.2.3-4-gabcdef'), '1.2.3-4.gabcdef');
  assert.equal(normalizeVersion('1.2.3-4-gabcdef-dirty'), '1.2.3-4.gabcdef.dirty');
  assert.equal(normalizeVersion('0.1.0-rc.1'), '0.1.0-rc.1');

  // 裸 SHA（仓库尚无 tag 时 git describe 的输出）不是 semver，必须被拒
  assert.equal(normalizeVersion('fdba91b'), '');
  assert.equal(normalizeVersion('fdba91b-dirty'), '');
  assert.equal(normalizeVersion(''), '');
  assert.equal(normalizeVersion('not-a-version'), '');
});
