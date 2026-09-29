// Phase 3 端到端冒烟：模型拉取与勾选、/v1/models 聚合、供应商名命名空间路由、模型别名。
//
// 覆盖：启动时未勾选模型不对外暴露、按需拉取上游 /models 作为候选、勾选后立即生效、
//       勾选之外的上游模型不出现、模型别名即对外模型名（转发时换回上游名）、
//       供应商名称命名空间路由、拉取失败不影响其他供应商、未授权访问 /v1/models。
//
// 用法：node tools/smoke/phase3.mjs [--port=19092] [--mock-port=9999]

import path from 'node:path';

import {
  ensureGateway,
  httpRequest,
  makeTempDir,
  parseArgs,
  runSmoke,
  startGateway,
  startMock,
  stopAll,
  waitForGatewayKey,
  waitForHttp,
  writeText,
} from './lib/harness.mjs';

const { port, mockPort, exe: exeArg } = parseArgs({ port: 19092, mockPort: 9999, exe: '' });

runSmoke(async (rec) => {
  const workDir = makeTempDir('agoramodel-p3');
  const dataDir = path.join(workDir, 'data');

  console.log(exeArg ? `== 复用已构建的二进制：${exeArg} ==` : '== 构建开发二进制 ==');
  const exe = ensureGateway({ workDir, name: 'agoramodel-p3', exe: exeArg });

  // mock：启动时未勾选任何模型，随后通过 API 拉取候选并勾选
  // dead：指向未监听端口，用于验证拉取失败隔离
  const cfg = writeText(
    path.join(workDir, 'config.json'),
    `{
  "gateway": { "listen": "127.0.0.1", "port": ${port}, "sse_idle_seconds": 15, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "mock", "name": "mock 供应商", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-mock-provider-key", "models": [], "allow_internal": true },
    { "id": "dead", "name": "dead 供应商", "openai_base_url": "http://127.0.0.1:9998/v1",
      "api_key": "sk-dead", "models": [], "allow_internal": true }
  ]
}
`,
  );

  const selectBody = JSON.stringify({
    name: 'mock 供应商',
    models_selected: ['mock-gpt-4o', 'mock-claude-sonnet-4-5'],
    model_aliases: { 'mock-gpt-4o': '我的模型' },
    allow_internal: true,
    enabled: true,
  });
  const aliasModel = '{"model":"我的模型","messages":[{"role":"user","content":"hi"}]}';
  const nsModel = '{"model":"mock 供应商/mock-claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}';
  const unknownNS = '{"model":"nope/nope-model","messages":[{"role":"user","content":"hi"}]}';

  const base = `http://127.0.0.1:${port}`;
  const mockBase = `http://127.0.0.1:${mockPort}`;

  await startMock({ workDir, port: mockPort });
  const gw = startGateway({ exe, workDir, name: 'gateway', config: cfg, dataDir, port });

  try {
    await waitForHttp(`${base}/healthz`, 20000);
    const key = await waitForGatewayKey(gw);
    if (!key) throw new Error(`未能取得网关 Key：\n${gw.logText}`);
    const json = { 'content-type': 'application/json' };
    const auth = { ...json, authorization: `Bearer ${key}` };

    console.log('== 断言 ==');

    let models = await httpRequest(`${base}/v1/models`, { headers: auth });
    rec.assertThat('GET /v1/models 返回 200', models.code === 200, `code=${models.code}`);
    const modelIds = (list) => (list.json?.data ?? []).map((item) => item.id);
    rec.assertThat('未勾选模型时 /v1/models 为空', modelIds(models).length === 0, `count=${modelIds(models).length}`);

    const unauth = await httpRequest(`${base}/v1/models`);
    rec.assertThat('未授权访问 /v1/models → 401', unauth.code === 401, `code=${unauth.code}`);

    const fetched = await httpRequest(`${base}/api/providers/mock/fetch-models`, { method: 'POST', headers: auth });
    rec.assertThat(
      '拉取模型接口返回候选（3 个）',
      fetched.code === 200 && (fetched.json?.candidate_models ?? []).length === 3,
      `code=${fetched.code}`,
    );
    rec.assertThat('拉取候选不改变已勾选模型', (fetched.json?.models_selected ?? []).length === 0);

    const fetchDead = await httpRequest(`${base}/api/providers/dead/fetch-models`, { method: 'POST', headers: auth });
    rec.assertThat('不可达供应商拉取失败 → 502', fetchDead.code === 502, `code=${fetchDead.code}`);

    const providers = await httpRequest(`${base}/api/providers`, { headers: auth });
    const items = providers.json?.items ?? [];
    const deadRec = items.find((item) => item.id === 'dead');
    const mockRec = items.find((item) => item.id === 'mock');
    rec.assertThat(
      '失败原因写入 last_fetch_error',
      Boolean(deadRec?.last_fetch_error),
      `err=${deadRec?.last_fetch_error}`,
    );
    rec.assertThat('失败供应商不影响其他供应商', !mockRec?.last_fetch_error, `err=${mockRec?.last_fetch_error}`);

    const select = await httpRequest(`${base}/api/providers/mock`, { method: 'PUT', headers: auth, body: selectBody });
    rec.assertThat('勾选模型并设置别名 → 200', select.code === 200, `code=${select.code}`);

    models = await httpRequest(`${base}/v1/models`, { headers: auth });
    const ids = modelIds(models);
    rec.assertThat(
      '勾选的模型出现在 /v1/models',
      ids.includes('我的模型') && ids.includes('mock-claude-sonnet-4-5'),
      `ids=${ids.join(',')}`,
    );
    rec.assertThat('未勾选的上游模型不出现', !ids.includes('mock-deepseek-v3'));
    rec.assertThat('配置别名后不再暴露上游原名', !ids.includes('mock-gpt-4o'));
    rec.assertThat(
      '列出命名空间形式（供应商名/模型名）',
      ids.includes('mock 供应商/我的模型') && ids.includes('mock 供应商/mock-claude-sonnet-4-5'),
      `ids=${ids.join(',')}`,
    );
    rec.assertThat('不包含失败供应商的模型', !ids.includes('dead 供应商/mock-gpt-4o'));

    const aliasEntry = (models.json?.data ?? []).find((item) => item.id === '我的模型');
    rec.assertThat('裸名条目 owned_by 为供应商名称', aliasEntry?.owned_by === 'mock 供应商', `owned_by=${aliasEntry?.owned_by}`);

    const alias = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: auth, body: aliasModel });
    rec.assertThat('用别名请求可直接透传（Agent 立即可用）', alias.code === 200 && alias.body.includes('mock'), `code=${alias.code}`);

    let mockRequests = await httpRequest(`${mockBase}/__requests`, { timeoutMs: 5000 });
    let lastAlias = (mockRequests.json?.requests ?? []).filter((item) => item.model === 'mock-gpt-4o').at(-1);
    rec.assertThat('别名已换回上游真实模型名', Boolean(lastAlias), `上游收到 model=${lastAlias?.model}`);

    const ns = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: auth, body: nsModel });
    rec.assertThat('命名空间路由可用（供应商名/模型名）', ns.code === 200 && ns.body.includes('mock'), `code=${ns.code}`);

    mockRequests = await httpRequest(`${mockBase}/__requests`, { timeoutMs: 5000 });
    const lastNS = (mockRequests.json?.requests ?? []).filter((item) => item.model === 'mock-claude-sonnet-4-5').at(-1);
    rec.assertThat('命名空间前缀已从转发给上游的 model 中剥离', Boolean(lastNS), `上游收到 model=${lastNS?.model}`);

    const unknown = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: auth, body: unknownNS });
    rec.assertThat('未知命名空间前缀退回普通模型名 → 404', unknown.code === 404, `code=${unknown.code}`);
  } finally {
    await stopAll();
  }

  console.log(`  临时目录：${workDir}`);
}, { label: '冒烟结果' });
