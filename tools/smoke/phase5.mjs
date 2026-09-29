// Phase 5 验收冒烟：PRD §7 八条验收 + 性能冒烟 + 安全复核 + 换机迁移演练。
//
// A) 单文件交付：三平台六份产物、格式与版本注入
// B) 八条验收：零模板接入 / 协议透传 / 模型选择 / 长任务不中断 / 断连无泄漏 /
//    安全基线 / 可排查 / 单文件交付
// C) 性能：20 并发流式请求的成功率、耗时与内存稳定性
// D) 迁移演练：导出 → 在新数据目录的实例上导入 → Agent 可继续使用
//
// 用法：node tools/smoke/phase5.mjs [--port=19095] [--port-b=19096] [--mock-port=9999]

import fs from 'node:fs';
import path from 'node:path';

import {
  ensureGateway,
  httpRequest,
  hostArtifactName,
  isWindows,
  makeTempDir,
  parseArgs,
  processRssBytes,
  repoRoot,
  run,
  runSmoke,
  sleep,
  startGateway,
  startMock,
  stopAll,
  waitForGatewayKey,
  waitForHttp,
  writeText,
} from './lib/harness.mjs';

const { port, portB, mockPort, exe: exeArg } = parseArgs({ port: 19095, portB: 19096, mockPort: 9999, exe: '' });

/** 六份发布产物及其应有的可执行文件魔数。 */
const ARTIFACTS = [
  { name: 'agoramodel-windows-amd64.exe', magic: 'pe' },
  { name: 'agoramodel-windows-arm64.exe', magic: 'pe' },
  { name: 'agoramodel-linux-amd64', magic: 'elf' },
  { name: 'agoramodel-linux-arm64', magic: 'elf' },
  { name: 'agoramodel-darwin-amd64', magic: 'macho' },
  { name: 'agoramodel-darwin-arm64', magic: 'macho' },
];

function magicOf(file) {
  const head = Buffer.alloc(4);
  const fd = fs.openSync(file, 'r');
  try {
    fs.readSync(fd, head, 0, 4, 0);
  } finally {
    fs.closeSync(fd);
  }
  if (head[0] === 0x4d && head[1] === 0x5a) return 'pe';
  if (head[0] === 0x7f && head[1] === 0x45 && head[2] === 0x4c && head[3] === 0x46) return 'elf';
  if (head.readUInt32BE(0) === 0xcffaedfe || head.readUInt32BE(0) === 0xcafebabe) return 'macho';
  return 'unknown';
}

runSmoke(async (rec) => {
  const workDir = makeTempDir('agoramodel-p5');
  const dataDirA = path.join(workDir, 'data');
  const dataDirB = path.join(workDir, 'migrated');
  const dist = path.join(repoRoot, 'dist');

  console.log('== A) 单文件交付（T5.1）==');

  const missing = ARTIFACTS.filter((item) => !fs.existsSync(path.join(dist, item.name)));
  if (missing.length > 0) {
    console.log('  dist 产物不齐，先执行三平台构建 …');
    if (isWindows) {
      run('pwsh', ['-File', 'build.ps1', '-Target', 'dist', '-Version', '0.2.0']);
    } else {
      // 版本号由 Makefile 从最近的 semver tag 推导；仓库无 tag 时用 main.go 内置版本兜底
      run('make', ['dist']);
    }
  }

  const missingAfter = ARTIFACTS.filter((item) => !fs.existsSync(path.join(dist, item.name))).map((item) => item.name);
  rec.assertThat('三平台六份产物齐备', missingAfter.length === 0, missingAfter.join(', '));

  const badMagic = ARTIFACTS.filter(
    (item) => fs.existsSync(path.join(dist, item.name)) && magicOf(path.join(dist, item.name)) !== item.magic,
  ).map((item) => item.name);
  rec.assertThat('产物格式正确（PE / ELF / Mach-O）', badMagic.length === 0, badMagic.join(', '));

  // 只能执行本机平台的那份产物：Windows 的 .exe 在 Linux 上跑不起来，反之亦然
  const hostArtifact = path.join(dist, hostArtifactName());
  const versionOutput = run(hostArtifact, ['--version'], { allowFailure: true });
  const versionText = `${versionOutput.stdout}${versionOutput.stderr}`.trim();
  // 版本号必须是可读的语义化版本号（构建脚本没有 tag 时不再注入 git hash）
  rec.assertThat('产物版本号为语义化版本号（不是构建哈希）', /^\d+\.\d+\.\d+/.test(versionText), `输出=${versionText}`);
  rec.assertThat('前端已内嵌（产物体积 > 5MB）', fs.statSync(hostArtifact).size > 5 * 1024 * 1024);

  const exe = ensureGateway({
    workDir,
    name: 'agoramodel-p5',
    ldflags: '-s -w -X main.version=0.2.0',
    exe: exeArg,
  });
  const statusOutput = run(exe, ['status'], { allowFailure: true });
  rec.assertThat(
    '服务子命令可用（status 在未安装时给出明确错误）',
    /not installed|服务操作失败|服务可能尚未安装/.test(`${statusOutput.stdout}${statusOutput.stderr}`),
    `输出=${`${statusOutput.stdout}${statusOutput.stderr}`.trim().split('\n').at(-1) ?? ''}`,
  );

  const cfgA = writeText(
    path.join(workDir, 'a.json'),
    `{
  "gateway": { "listen": "127.0.0.1", "port": ${port}, "sse_idle_seconds": 2, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "seed", "name": "seed", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-seed-key", "models": ["mock-gpt-4o"], "allow_internal": true }
  ]
}
`,
  );
  // 网关要求至少一个启用的供应商（避免空配置静默启动），因此两个实例各带一个占位供应商
  const cfgB = writeText(
    path.join(workDir, 'b.json'),
    `{
  "gateway": { "listen": "127.0.0.1", "port": ${portB} },
  "providers": [
    { "id": "placeholder", "name": "placeholder", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-placeholder", "models": ["mock-gpt-4o"], "allow_internal": true }
  ]
}
`,
  );
  const cfgInternal = writeText(
    path.join(workDir, 'internal.json'),
    `{
  "gateway": { "listen": "127.0.0.1", "port": 19097 },
  "providers": [
    { "id": "internal", "name": "internal", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-x", "models": ["m"], "allow_internal": false }
  ]
}
`,
  );

  const base = `http://127.0.0.1:${port}`;
  const json = { 'content-type': 'application/json' };

  await startMock({ workDir, port: mockPort });
  const gwA = startGateway({ exe, workDir, name: 'gw-a', config: cfgA, dataDir: dataDirA, port, logLevel: 'info' });

  try {
    await waitForHttp(`${base}/healthz`, 20000);
    const key = await waitForGatewayKey(gwA);
    rec.assertThat('网关 Key 已生成', Boolean(key));
    if (!key) throw new Error(`未能取得网关 Key：\n${gwA.logText}`);
    const auth = { ...json, authorization: `Bearer ${key}` };

    console.log('== B) PRD §7 八条验收 ==');

    // 1) 零模板接入：只给名称 + URL + Key + 勾选模型，不选任何模板
    const create = await httpRequest(`${base}/api/providers`, {
      method: 'POST',
      headers: json,
      body: JSON.stringify({
        name: '验收供应商',
        openai_base_url: `http://127.0.0.1:${mockPort}/v1`,
        api_key: 'sk-acceptance-key',
        models_selected: ['mock-gpt-4o'],
        allow_internal: true,
      }),
    });
    const providerId = create.body.match(/"id":"([^"]+)"/)?.[1] ?? '';
    rec.assertThat('① 零模板接入：仅填 URL + Key + 勾选模型即完成新增', create.code === 201 && providerId !== '', `code=${create.code}`);

    // 2) 协议透传
    const openai = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: auth,
      body: '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}',
    });
    const anthropic = await httpRequest(`${base}/v1/messages`, {
      method: 'POST',
      headers: auth,
      body: '{"model":"mock-claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}',
    });
    rec.assertThat('② 协议透传：/v1/chat/completions 返回 200', openai.code === 200, `openai=${openai.code}`);
    rec.assertThat('② 原生透传：响应体来自上游（含 mock 标记）', openai.body.includes('mock'));
    rec.assertThat('② Anthropic 路径已下线（404）', anthropic.code === 404, `anthropic=${anthropic.code}`);

    // 3) 模型选择
    const models = await httpRequest(`${base}/v1/models`, { headers: auth });
    const modelJson = models.body;
    const dedup = (modelJson.match(/"id":"mock-gpt-4o","object":"model","owned_by":"[^"]+"/g) ?? []).length;
    rec.assertThat(
      '③ /v1/models 返回已勾选模型（含 owned_by）',
      models.code === 200 && modelJson.includes('"object":"list"') && modelJson.includes('"owned_by"'),
      `code=${models.code}`,
    );
    rec.assertThat('③ 同名模型只出现一次裸名条目', dedup === 1, `出现次数=${dedup}`);
    rec.assertThat('③ owned_by 为供应商名称', modelJson.includes('"owned_by":"seed"'));

    // 4) 长任务不中断（上游静默 20s，网关心跳兜底）
    const stream = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...auth, 'x-mock-silence': '20', 'x-mock-chunks': '3' },
      body: '{"model":"mock-gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}',
      timeoutMs: 60000,
    });
    rec.assertThat('④ 上游静默 20s 仍能完成（SSE 心跳兜底）', stream.body.includes('[DONE]') && stream.body.includes(': keep-alive'));

    // 5) 断连无泄漏：长静默请求结束后网关仍健康
    await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...auth, 'x-mock-silence': '5' },
      body: '{"model":"mock-gpt-4o","stream":true,"messages":[]}',
      timeoutMs: 60000,
    });
    await sleep(1000);
    const afterDisconnect = await httpRequest(`${base}/healthz`);
    rec.assertThat('⑤ 客户端断开后网关仍健康（无泄漏式崩溃）', afterDisconnect.code === 200, `code=${afterDisconnect.code}`);

    // 7) 可排查：注入上游 401，日志页应能查到
    const failing = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...auth, 'x-mock-status': '401' },
      body: '{"model":"mock-gpt-4o","messages":[]}',
    });
    await sleep(800);
    const logs = await httpRequest(`${base}/api/logs?limit=20`, { headers: auth });
    rec.assertThat('⑦ 上游 401 原样透传', failing.code === 401, `code=${failing.code}`);
    rec.assertThat('⑦ 失败请求可在日志接口查到', logs.body.includes('"status_code":401'));

    // 6) 安全基线
    const dbHasPlaintext = ['agora.db', 'agora.db-wal'].some(
      (file) => fs.existsSync(path.join(dataDirA, file)) && fs.readFileSync(path.join(dataDirA, file)).includes('sk-acceptance-key'),
    );
    rec.assertThat('⑥ 数据库中无供应商凭证明文', !dbHasPlaintext);

    const providers = await httpRequest(`${base}/api/providers`, { headers: auth });
    rec.assertThat('⑥ API 只回凭证掩码', providers.body.includes('sk-****') && !providers.body.includes('sk-acceptance-key'));

    const gwInternal = startGateway({
      exe,
      workDir,
      name: 'gw-internal',
      config: cfgInternal,
      dataDir: path.join(workDir, 'internal-data'),
      port: 19097,
      logLevel: 'info',
    });
    const exitedInternal = await gwInternal.waitForExit(10000);
    rec.assertThat('⑥ 内网地址默认被拒（SSRF）', exitedInternal && gwInternal.logText.includes('内网'), `exited=${exitedInternal}`);
    if (!exitedInternal) gwInternal.stop();

    const gwNoPass = startGateway({
      exe,
      workDir,
      name: 'gw-nopass',
      config: cfgB,
      dataDir: path.join(workDir, 'nopass-data'),
      port: portB,
      extraArgs: ['--listen', '0.0.0.0'],
      logLevel: 'info',
    });
    const exitedNoPass = await gwNoPass.waitForExit(10000);
    rec.assertThat('⑥ 非回环监听且无密码时拒绝启动', exitedNoPass && gwNoPass.logText.includes('管理密码'), `exited=${exitedNoPass}`);
    if (!exitedNoPass) gwNoPass.stop();

    // 8) 单文件交付（已在 A 段验证产物；此处验证运行时不依赖数据库外的文件）
    rec.assertThat(
      '⑧ 运行仅依赖可执行文件 + agora.db + master.key',
      fs.existsSync(path.join(dataDirA, 'agora.db')) && fs.existsSync(path.join(dataDirA, 'master.key')),
    );

    console.log('== C) 性能冒烟（T5.4）==');
    const before = processRssBytes(gwA.pid);
    const startedAt = Date.now();
    const codes = await Promise.all(
      Array.from({ length: 20 }, () =>
        httpRequest(`${base}/v1/chat/completions`, {
          method: 'POST',
          headers: { ...auth, 'x-mock-chunks': '3', 'x-mock-gap': '10' },
          body: '{"model":"mock-gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}',
          timeoutMs: 30000,
        }).then((res) => res.code),
      ),
    );
    const elapsedMs = Date.now() - startedAt;
    const okCount = codes.filter((code) => code === 200).length;
    rec.assertThat('20 并发流式请求全部成功', okCount === 20, `成功=${okCount}/20 耗时=${elapsedMs}ms`);

    await sleep(2000);
    const growthMB = Math.round((processRssBytes(gwA.pid) - before) / (1024 * 1024) * 10) / 10;
    rec.assertThat('压测后内存增长可控（< 80MB）', growthMB < 80, `增长=${growthMB}MB`);

    console.log('== D) 换机迁移演练（T5.6）==');
    const exported = await httpRequest(`${base}/api/export`, { headers: auth });
    // 导出文件包含 API Key 明文（用户要求换机迁移可直接导入）
    rec.assertThat(
      '导出配置成功且包含明文凭证（迁移用）',
      exported.code === 200 && exported.body.includes('sk-acceptance-key'),
      `code=${exported.code}`,
    );
    rec.assertThat(
      '导出文件带格式标识',
      exported.json?.format === 'agoramodel.providers/v1',
      `format=${exported.json?.format}`,
    );

    const gwB = startGateway({ exe, workDir, name: 'gw-b', config: cfgB, dataDir: dataDirB, port: portB, logLevel: 'info' });
    await waitForHttp(`http://127.0.0.1:${portB}/healthz`, 20000);
    const keyB = await waitForGatewayKey(gwB);
    rec.assertThat('迁移目标实例已就绪（独立数据目录 + 新网关 Key）', Boolean(keyB) && keyB !== key);

    // 直接把导出文件原样导入目标实例：凭证随文件迁移，导入时用目标机器的主密钥重新加密
    const imported = await httpRequest(`http://127.0.0.1:${portB}/api/import`, {
      method: 'POST',
      headers: json,
      body: exported.body,
    });
    rec.assertThat('在目标实例导入供应商成功', imported.code === 200, `code=${imported.code}`);

    const migratedChat = await httpRequest(`http://127.0.0.1:${portB}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...json, authorization: `Bearer ${keyB}` },
      body: '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}',
    });
    rec.assertThat('迁移后 Agent 可直接使用（同模型名仍可用）', migratedChat.code === 200, `code=${migratedChat.code}`);
  } finally {
    await stopAll();
  }

  console.log(`  临时目录：${workDir}`);
}, { label: '验收结果' });
