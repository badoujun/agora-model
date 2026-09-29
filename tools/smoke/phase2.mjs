// Phase 2 端到端冒烟：SQLite 持久化、凭证加密、网关 Key 生命周期、SSRF 校验。
//
// 覆盖：首次启动（生成主密钥 / 导入供应商 / 生成网关 Key）、请求日志落库与脱敏、
//       重启复用（不重复导入、原 Key 仍有效）、网关 Key 重置（旧 Key 立即失效）、
//       allow_internal=false 时内网地址被拒、主密钥丢失时不静默降级。
//
// 用法：node tools/smoke/phase2.mjs [--port=19091] [--mock-port=9999]

import fs from 'node:fs';
import path from 'node:path';

import {
  copyDir,
  dbContains,
  ensureGateway,
  httpRequest,
  makeTempDir,
  parseArgs,
  readText,
  runSmoke,
  sleep,
  startGateway,
  startMock,
  stopAll,
  waitForGatewayKey,
  waitForHttp,
  writeText,
} from './lib/harness.mjs';

const { port, mockPort, exe: exeArg } = parseArgs({ port: 19091, mockPort: 9999, exe: '' });

runSmoke(async (rec) => {
  const workDir = makeTempDir('agoramodel-p2');
  const dataDirA = path.join(workDir, 'data'); // 正常实例
  const dataDirB = path.join(workDir, 'internal'); // SSRF 拒绝场景
  const dataDirC = path.join(workDir, 'lost-key'); // 主密钥丢失场景

  console.log('== 构建开发二进制 ==');
  const exe = ensureGateway({ workDir, name: 'agoramodel-p2', exe: exeArg });

  const cfgOk = writeText(
    path.join(workDir, 'ok.json'),
    `{
  "gateway": { "listen": "127.0.0.1", "port": ${port}, "sse_idle_seconds": 15, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "mock", "name": "mock", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-mock-provider-key", "models": ["mock-gpt-4o"],
      "allow_internal": true }
  ]
}
`,
  );

  const cfgBad = writeText(
    path.join(workDir, 'internal.json'),
    `{
  "gateway": { "listen": "127.0.0.1", "port": ${port} },
  "providers": [
    { "id": "internal", "name": "internal", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-internal", "models": ["m"], "allow_internal": false }
  ]
}
`,
  );

  const body = '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}';
  const base = `http://127.0.0.1:${port}`;
  const jsonHeader = { 'content-type': 'application/json' };

  await startMock({ workDir, port: mockPort });

  try {
    console.log('== 断言 ==');

    // ---------- A) 首次启动 ----------
    const gwA = startGateway({ exe, workDir, name: 'gw-a', config: cfgOk, dataDir: dataDirA, port });
    const key1 = await waitForGatewayKey(gwA);
    rec.assertThat(
      '首次启动打印网关 Key（控制台亦可查看/复制）',
      Boolean(key1),
      `hint=${key1 ? `${key1.slice(0, 7)}***` : ''}`,
    );
    if (!key1) throw new Error(`未能取得网关 Key：\n${gwA.logText}`);

    await waitForHttp(`${base}/healthz`, 20000);

    rec.assertThat(
      '首次启动从引导配置导入供应商',
      gwA.logText.includes('已从引导配置导入供应商') && gwA.logText.includes('count=1'),
    );
    rec.assertThat('首次启动生成主密钥文件', gwA.logText.includes('已生成新的主密钥文件'));

    rec.assertThat('healthz 返回 200', (await httpRequest(`${base}/healthz`)).code === 200);

    const auth = { ...jsonHeader, authorization: `Bearer ${key1}` };
    let res = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: auth, body });
    rec.assertThat('使用数据库中的网关 Key 可完成透传', res.code === 200 && res.body.includes('mock'), `code=${res.code}`);

    res = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...jsonHeader, authorization: 'Bearer gw-nope' },
      body,
    });
    rec.assertThat('错误网关 Key 被拒', res.code === 401, `code=${res.code}`);

    // 控制台可随时查看/复制当前网关 Key（明文以密文入库，sha256 仍用于校验）
    const settingsA = await httpRequest(`${base}/api/settings`);
    rec.assertThat(
      '网关 Key 明文可从 /api/settings 取回（控制台可复制）',
      settingsA.code === 200 && settingsA.body.includes(key1),
      `code=${settingsA.code}`,
    );

    const dbPath = path.join(dataDirA, 'agora.db');
    const masterPath = path.join(dataDirA, 'master.key');
    rec.assertThat(
      '数据目录含 agora.db 与 master.key',
      fs.existsSync(dbPath) && fs.existsSync(masterPath),
    );
    rec.assertThat('master.key 为 64 位 hex', /^[0-9a-f]{64}$/.test(readText(masterPath).trim()));

    await sleep(1500); // 等待异步日志批量落库
    rec.assertThat('请求日志已落库（可按模型检索到）', dbContains(dataDirA, 'mock-gpt-4o'));
    rec.assertThat('数据库中不含供应商凭证明文', !dbContains(dataDirA, 'sk-mock-provider-key'));
    // 网关 Key 明文以 AES 密文入库（控制台可解密查看），因此库里检索不到明文字符串
    rec.assertThat('数据库中不含网关 Key 明文字符串（只存密文与哈希）', !dbContains(dataDirA, key1));

    gwA.stop();
    await sleep(500);

    // ---------- B) 重启复用 ----------
    const gwB = startGateway({ exe, workDir, name: 'gw-b', config: cfgOk, dataDir: dataDirA, port });
    await waitForHttp(`${base}/healthz`, 20000);
    rec.assertThat('重启不再生成网关 Key（读库复用）', !gwB.logText.includes('已生成网关 Key'));
    rec.assertThat('重启不再重复导入供应商', !gwB.logText.includes('已从引导配置导入'));
    rec.assertThat('重启后原网关 Key 仍有效', (await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: auth, body })).code === 200);
    gwB.stop();
    await sleep(500);

    // ---------- C) 网关 Key 重置 ----------
    const gwC = startGateway({ exe, workDir, name: 'gw-c', config: cfgOk, dataDir: dataDirA, port, extraArgs: ['--reset-gateway-key'] });
    const key2 = await waitForGatewayKey(gwC);
    rec.assertThat('重置后打印新 Key 且与旧 Key 不同', Boolean(key2) && key2 !== key1);
    if (!key2) throw new Error(`重置后未能取得网关 Key：\n${gwC.logText}`);

    await waitForHttp(`${base}/healthz`, 20000);
    await sleep(300);
    rec.assertThat(
      '重置后新 Key 可用',
      (await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: { ...jsonHeader, authorization: `Bearer ${key2}` }, body })).code === 200,
    );
    rec.assertThat(
      '重置后旧 Key 立即失效（401）',
      (await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: auth, body })).code === 401,
    );
    const settingsC = await httpRequest(`${base}/api/settings`);
    rec.assertThat(
      '重置后 /api/settings 返回的是新 Key 明文（控制台可继续复制）',
      settingsC.code === 200 && settingsC.body.includes(key2),
    );
    gwC.stop();
    await sleep(500);

    // ---------- D) SSRF 校验 ----------
    const gwD = startGateway({ exe, workDir, name: 'gw-d', config: cfgBad, dataDir: dataDirB, port });
    const exitedD = await gwD.waitForExit(10000);
    rec.assertThat('allow_internal=false 时内网地址被拒并启动失败', exitedD, `exited=${exitedD}`);
    rec.assertThat('SSRF 拒绝信息可读（提示内网地址）', gwD.logText.includes('内网'));
    if (!exitedD) gwD.stop();

    // ---------- E) 主密钥丢失 ----------
    copyDir(dataDirA, dataDirC);
    fs.rmSync(path.join(dataDirC, 'master.key'), { force: true });
    const gwE = startGateway({ exe, workDir, name: 'gw-e', config: cfgOk, dataDir: dataDirC, port });
    const exitedE = await gwE.waitForExit(10000);
    rec.assertThat('主密钥丢失时启动失败（不静默降级）', exitedE && gwE.logText.includes('解密失败'), `exited=${exitedE}`);
    if (!exitedE) gwE.stop();
  } finally {
    await stopAll();
  }

  console.log(`  临时目录：${workDir}`);
}, { label: '冒烟结果' });
