// Phase 4 端到端冒烟：Web UI 静态资源 + 控制面 API 全链路 + 登录模式。
//
// 覆盖：内嵌前端可访问（index.html / assets 缓存头 / SPA fallback）、/api 与 /v1 未知路径不被前端路由吞掉、
//       健康检查与会话状态、模拟 Web UI 的操作序列（新增供应商 → 拉取并勾选模型 → 连接测试 →
//       用新模型直接透传 → 日志可查 → 编辑保持凭证 → 删除）、以及设置 ADMIN_PASSWORD 后的登录流程。
//
// 用法：node tools/smoke/phase4.mjs [--port=19093] [--auth-port=19094] [--mock-port=9999]

import fs from 'node:fs';
import path from 'node:path';

import {
  ensureGateway,
  httpRequest,
  isWindows,
  makeTempDir,
  parseArgs,
  repoRoot,
  run,
  runSmoke,
  startGateway,
  startMock,
  stopAll,
  waitForGatewayKey,
  waitForHttp,
  writeText,
} from './lib/harness.mjs';

const { port, authPort, mockPort, exe: exeArg } = parseArgs({ port: 19093, authPort: 19094, mockPort: 9999, exe: '' });

/** 从响应头里取会话 Cookie（对应 PowerShell 版本对 -D 头文件的提取）。 */
function sessionCookie(headersText) {
  return headersText.match(/agoramodel_session=([^;\r\n]+)/i)?.[1] ?? '';
}

runSmoke(async (rec) => {
  const workDir = makeTempDir('agoramodel-p4');
  const dataDirA = path.join(workDir, 'data');
  const dataDirB = path.join(workDir, 'auth');

  console.log('== 准备：构建前端与开发二进制 ==');
  const webDir = path.join(repoRoot, 'web');
  const indexHtml = path.join(repoRoot, 'internal', 'webui', 'dist', 'index.html');
  if (!fs.existsSync(indexHtml)) {
    console.log('  前端尚未构建，执行 npm ci && npm run build …');
    const npm = isWindows ? 'npm.cmd' : 'npm';
    if (!fs.existsSync(path.join(webDir, 'node_modules'))) {
      run(npm, ['ci'], { cwd: webDir });
    }
    run(npm, ['run', 'build'], { cwd: webDir });
  }
  rec.assertThat('前端构建产物存在（internal/webui/dist/index.html）', fs.existsSync(indexHtml));

  console.log(exeArg ? `  复用已构建的二进制：${exeArg}` : '  构建开发二进制 …');
  const exe = ensureGateway({ workDir, name: 'agoramodel-p4', exe: exeArg });

  const configA = writeText(
    path.join(workDir, 'a.json'),
    `{
  "gateway": { "listen": "127.0.0.1", "port": ${port}, "sse_idle_seconds": 15, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "seed", "name": "seed", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-seed-key", "models": ["mock-gpt-4o"], "allow_internal": true }
  ]
}
`,
  );
  const configB = writeText(
    path.join(workDir, 'b.json'),
    `{
  "gateway": { "listen": "127.0.0.1", "port": ${authPort} },
  "providers": [
    { "id": "seed", "name": "seed", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-seed-key", "models": ["mock-gpt-4o"], "allow_internal": true }
  ]
}
`,
  );

  const base = `http://127.0.0.1:${port}`;
  const authBase = `http://127.0.0.1:${authPort}`;
  const jsonHeader = { 'content-type': 'application/json' };

  await startMock({ workDir, port: mockPort });
  const gwA = startGateway({ exe, workDir, name: 'gw-a', config: configA, dataDir: dataDirA, port });

  try {
    await waitForHttp(`${base}/healthz`, 20000);
    const key = await waitForGatewayKey(gwA);
    rec.assertThat('网关 Key 已生成（供 Agent 使用）', Boolean(key));
    if (!key) throw new Error(`未能取得网关 Key：\n${gwA.logText}`);
    const authHeader = { ...jsonHeader, authorization: `Bearer ${key}` };

    console.log('== A) Web UI 静态资源与 SPA 行为 ==');

    const index = await httpRequest(`${base}/`);
    rec.assertThat('GET / 返回前端页面', index.code === 200 && index.body.includes('<div id="root">'), `code=${index.code}`);
    rec.assertThat('GET / 的 Content-Type 为 text/html', /content-type:\s*text\/html/i.test(index.headersText));
    rec.assertThat('GET / 不被缓存（no-store）', /cache-control:\s*no-store/i.test(index.headersText));

    const spa = await httpRequest(`${base}/logs`);
    rec.assertThat('未知前端路由回退到 index.html（SPA）', spa.code === 200 && spa.body.includes('<div id="root">'), `code=${spa.code}`);

    const assetsDir = path.join(repoRoot, 'internal', 'webui', 'dist', 'assets');
    const assetName = fs.readdirSync(assetsDir).find((name) => name.endsWith('.js'));
    const asset = await httpRequest(`${base}/assets/${assetName}`);
    rec.assertThat('GET /assets/*.js 返回 200', asset.code === 200, `code=${asset.code}`);
    rec.assertThat('静态资源带长期缓存头（immutable）', /immutable/i.test(asset.headersText));

    const apiUnknown = await httpRequest(`${base}/api/nope`);
    rec.assertThat('/api 未知路径返回 404（不被 SPA 吞掉）', apiUnknown.code === 404, `code=${apiUnknown.code}`);
    const v1Unknown = await httpRequest(`${base}/v1/nope`);
    rec.assertThat('/v1 未知路径返回 404（不被 SPA 吞掉）', v1Unknown.code === 404, `code=${v1Unknown.code}`);

    console.log('== B) 控制面会话与健康检查 ==');

    const session = await httpRequest(`${base}/api/auth/session`);
    rec.assertThat(
      '会话：回环 + 未设密码 → 免登录',
      session.body.includes('"login_required":false') && session.body.includes('"authenticated":true'),
    );

    const health = await httpRequest(`${base}/api/health`);
    rec.assertThat('GET /api/health 返回 200 且含版本', health.code === 200 && health.body.includes('"status":"ok"'), `code=${health.code}`);

    console.log('== C) 模拟 Web UI 操作序列 ==');

    const createBody = JSON.stringify({
      name: '界面新增供应商',
      openai_base_url: `http://127.0.0.1:${mockPort}/v1`,
      api_key: 'sk-ui-created-abcdefgh',
      models_selected: [],
      timeout_seconds: 60,
      allow_internal: true,
      enabled: true,
    });
    const created = await httpRequest(`${base}/api/providers`, { method: 'POST', headers: jsonHeader, body: createBody });
    rec.assertThat('POST /api/providers 创建成功（201）', created.code === 201, `code=${created.code}`);
    rec.assertThat(
      '响应只回凭证掩码',
      created.body.includes('"api_key_hint":"sk-****efgh"') && !created.body.includes('sk-ui-created-abcdefgh'),
    );
    const newId = created.body.match(/"id":"([^"]+)"/)?.[1] ?? '';
    rec.assertThat('新建供应商默认没有已启用模型', created.body.includes('"model_count":0'));

    const fetched = await httpRequest(`${base}/api/providers/${newId}/fetch-models`, { method: 'POST', headers: authHeader });
    rec.assertThat(
      '拉取模型接口返回上游候选列表',
      fetched.code === 200 && fetched.body.includes('"candidate_models"'),
      `code=${fetched.code}`,
    );

    const selectBody = JSON.stringify({ name: '界面新增供应商', models_selected: ['mock-deepseek-v3'], allow_internal: true, enabled: true });
    const selected = await httpRequest(`${base}/api/providers/${newId}`, { method: 'PUT', headers: jsonHeader, body: selectBody });
    rec.assertThat('勾选模型后立即生效（model_count=1）', selected.code === 200 && selected.body.includes('"model_count":1'), `code=${selected.code}`);

    const test = await httpRequest(`${base}/api/providers/${newId}/test`, { method: 'POST', headers: authHeader });
    rec.assertThat('POST /api/providers/{id}/test 连接测试通过', test.code === 200 && test.body.includes('"ok":true'), `code=${test.code}`);

    const models = await httpRequest(`${base}/api/models`, { headers: authHeader });
    rec.assertThat('GET /api/models 含勾选的模型', models.body.includes('"model":"mock-deepseek-v3"'));
    rec.assertThat('GET /api/models 标注供应商名称', models.body.includes('"provider_name":"界面新增供应商"'));

    const chat = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: authHeader,
      body: '{"model":"mock-deepseek-v3","messages":[{"role":"user","content":"hi"}]}',
    });
    rec.assertThat('用界面新增的供应商直接完成透传（Agent 立即可用）', chat.code === 200 && chat.body.includes('mock'), `code=${chat.code}`);

    await new Promise((resolve) => setTimeout(resolve, 800));
    const logs = await httpRequest(`${base}/api/logs?limit=20`, { headers: authHeader });
    rec.assertThat('GET /api/logs 可查到刚才的请求', logs.code === 200 && logs.body.includes('mock-deepseek-v3'), `code=${logs.code}`);

    const updated = await httpRequest(`${base}/api/providers/${newId}`, {
      method: 'PUT',
      headers: jsonHeader,
      body: '{"name":"改名后的供应商"}',
    });
    rec.assertThat(
      'PUT 更新名称成功且凭证掩码不变',
      updated.code === 200 && updated.body.includes('改名后的供应商') && updated.body.includes('sk-****efgh'),
      `code=${updated.code}`,
    );

    const settings = await httpRequest(`${base}/api/settings`, { headers: authHeader });
    rec.assertThat('GET /api/settings 返回监听与 Key 掩码', settings.code === 200 && settings.body.includes('"gateway_key_hint":"gw-'));
    rec.assertThat(
      'GET /api/settings 返回可复制的明文网关 Key',
      settings.code === 200 && settings.body.includes(key) && settings.body.includes('"gateway_key_revealable":true'),
    );

    const deleted = await httpRequest(`${base}/api/providers/${newId}`, { method: 'DELETE', headers: authHeader });
    rec.assertThat('DELETE /api/providers/{id} 成功', deleted.code === 200, `code=${deleted.code}`);

    console.log('== D) 登录模式（ADMIN_PASSWORD）==');
    startGateway({
      exe,
      workDir,
      name: 'gw-b',
      config: configB,
      dataDir: dataDirB,
      port: authPort,
      extraArgs: ['--admin-password', 'p4-admin-pass'],
    });

    await waitForHttp(`${authBase}/healthz`, 20000);

    const unauth = await httpRequest(`${authBase}/api/providers`);
    rec.assertThat('设置管理密码后未登录访问 /api → 401', unauth.code === 401, `code=${unauth.code}`);

    const indexAuth = await httpRequest(`${authBase}/`);
    rec.assertThat('登录页仍可加载前端资源（GET / 200）', indexAuth.code === 200, `code=${indexAuth.code}`);

    const login = await httpRequest(`${authBase}/api/auth/login`, {
      method: 'POST',
      headers: jsonHeader,
      body: '{"password":"p4-admin-pass"}',
    });
    rec.assertThat(
      'POST /api/auth/login 成功并下发会话 Cookie',
      login.code === 200 && /agoramodel_session=/i.test(login.headersText),
      `code=${login.code}`,
    );

    const cookie = sessionCookie(login.headersText);
    const authed = await httpRequest(`${authBase}/api/providers`, { headers: { cookie: `agoramodel_session=${cookie}` } });
    rec.assertThat('携带会话 Cookie 后可访问 /api', authed.code === 200, `code=${authed.code}`);

    const wrong = await httpRequest(`${authBase}/api/auth/login`, {
      method: 'POST',
      headers: jsonHeader,
      body: '{"password":"nope"}',
    });
    rec.assertThat('错误密码被拒绝（401）', wrong.code === 401, `code=${wrong.code}`);
  } finally {
    await stopAll();
  }

  console.log(`  临时目录：${workDir}`);
}, { label: '冒烟结果' });
