import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { createPackageTarball, isExecutableEntry, readTarballEntries } from '../scripts/tarball.mjs';

function makeTempDir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'agoramodel-tar-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

/** 造一个最小的包目录：package.json + bin/<binary>。 */
function makePackageDir(t, binaryName = 'agoramodel') {
  const dir = makeTempDir(t);
  fs.writeFileSync(path.join(dir, 'package.json'), '{"name":"x","version":"1.0.0"}\n');
  fs.mkdirSync(path.join(dir, 'bin'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'bin', binaryName), 'BINARY-CONTENT');
  return dir;
}

test('isExecutableEntry 只认 bin/ 下的非 .exe', () => {
  assert.equal(isExecutableEntry('package/bin/agoramodel'), true);
  assert.equal(isExecutableEntry('package/bin/agoramodel.js'), true);
  // Windows 二进制不需要可执行位，保持 644（与 npm 自身行为一致）
  assert.equal(isExecutableEntry('package/bin/agoramodel.exe'), false);
  assert.equal(isExecutableEntry('package/package.json'), false);
  assert.equal(isExecutableEntry('package/README.md'), false);
  assert.equal(isExecutableEntry('package/bin/README.md'), true); // 用了前缀匹配，bin/ 下一律可执行
});

test('打包：条目名带 package/ 前缀，内容与大小正确', (t) => {
  const dir = makePackageDir(t);
  const tgz = path.join(makeTempDir(t), 'out.tgz');

  const result = createPackageTarball(dir, tgz);
  assert.equal(result.files.length, 2);
  assert.ok(result.tarballBytes > 0);

  const entries = readTarballEntries(tgz);
  assert.deepEqual(
    entries.map((entry) => entry.name).sort(),
    ['package/bin/agoramodel', 'package/package.json']
  );
  for (const entry of entries) {
    assert.equal(entry.typeflag, '0', `${entry.name} 应为普通文件条目`);
  }
  assert.equal(
    entries.find((entry) => entry.name === 'package/bin/agoramodel').size,
    Buffer.byteLength('BINARY-CONTENT')
  );
});

test('打包：bin/ 下非 .exe 写入 0755，其余保持 0644', (t) => {
  const dir = makePackageDir(t, 'agoramodel');
  fs.writeFileSync(path.join(dir, 'README.md'), 'readme\n');
  const tgz = path.join(makeTempDir(t), 'out.tgz');
  createPackageTarball(dir, tgz);

  const modeOf = (name) =>
    readTarballEntries(tgz).find((entry) => entry.name === name)?.mode;

  // Windows 文件系统无法表达权限位，这个断言正是「从 Windows 打的包在 Linux 上也能执行」的保证
  assert.equal(modeOf('package/bin/agoramodel'), 0o755);
  assert.equal(modeOf('package/package.json'), 0o644);
  assert.equal(modeOf('package/README.md'), 0o644);
});

test('打包：.exe 不写入可执行位', (t) => {
  const dir = makePackageDir(t, 'agoramodel.exe');
  const tgz = path.join(makeTempDir(t), 'out.tgz');
  createPackageTarball(dir, tgz);

  const entry = readTarballEntries(tgz).find((item) => item.name === 'package/bin/agoramodel.exe');
  assert.equal(entry.mode, 0o644);
});

test('打包：递归包含子目录且顺序稳定', (t) => {
  const dir = makePackageDir(t);
  fs.mkdirSync(path.join(dir, 'docs'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'docs', 'a.md'), 'a');
  const tgz = path.join(makeTempDir(t), 'out.tgz');
  createPackageTarball(dir, tgz);

  const names = readTarballEntries(tgz).map((entry) => entry.name);
  assert.deepEqual(names, [...names].sort(), '条目顺序应为排序后的稳定顺序');
  assert.ok(names.includes('package/docs/a.md'));
});

test('打包：空目录直接报错', (t) => {
  const empty = makeTempDir(t);
  assert.throws(
    () => createPackageTarball(empty, path.join(makeTempDir(t), 'out.tgz')),
    /目录为空/
  );
});

test('打包：产出的 tgz 能被系统 tar 读取（独立校验 tar 头与 checksum）', (t) => {
  const dir = makePackageDir(t);
  const tgz = path.join(makeTempDir(t), 'out.tgz');
  createPackageTarball(dir, tgz);

  const result = spawnSync('tar', ['-tzf', tgz], { encoding: 'utf8' });
  if (result.error) {
    t.skip(`系统没有 tar：${result.error.message}`);
    return;
  }
  assert.equal(result.status, 0, `tar 读取失败：${result.stderr}`);
  assert.match(result.stdout, /package\/bin\/agoramodel/);
  assert.match(result.stdout, /package\/package\.json/);
});

test('打包：系统 tar 解出的权限与写入一致', (t) => {
  const dir = makePackageDir(t);
  const tgz = path.join(makeTempDir(t), 'out.tgz');
  createPackageTarball(dir, tgz);

  const result = spawnSync('tar', ['-tvzf', tgz], { encoding: 'utf8' });
  if (result.error || result.status !== 0) {
    t.skip('系统 tar 不可用或无权限查看');
    return;
  }
  // 形如：-rwxr-xr-x  0 0 0 14 ... package/bin/agoramodel
  const binaryLine = result.stdout
    .split(/\r?\n/)
    .find((line) => line.includes('package/bin/agoramodel'));
  assert.ok(binaryLine, `未在 tar 输出中找到二进制条目：\n${result.stdout}`);
  assert.ok(
    binaryLine.startsWith('-rwxr-xr-x'),
    `bin/ 下的二进制应带可执行位，实际为：${binaryLine}`
  );
});
