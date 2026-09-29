// Phase 1 端到端冒烟：启动 mock 上游与网关，逐项断言核心行为。
//
// 覆盖：健康检查、认证与错误体风格、路由与错误码、非流式与流式透传、
//       上游凭证替换与头透传、SSE 心跳保活、上游超时、上游不可达、请求体上限、
//       extra_body / extra_headers 合并、客户端断连取消。
//
// 用法：node tools/smoke/phase1.mjs [--gateway-port=19090] [--mock-port=9999]

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

const { gatewayPort, mockPort, exe: exeArg } = parseArgs({ gatewayPort: 19090, mockPort: 9999, exe: '' });

runSmoke(async (rec) => {
  const workDir = makeTempDir('agoramodel-p1');
  const dataDir = path.join(workDir, 'data');
  const cfgFile = path.join(workDir, 'config.json');

  console.log(exeArg ? `== 复用已构建的二进制：${exeArg} ==` : '== 构建开发二进制 ==');
  const exe = ensureGateway({ workDir, name: 'agoramodel-p1', exe: exeArg });

  // Phase 1 的白名单极紧（max_body_bytes=1024、sse_idle_seconds=1）以便快速复现边界。
  // gateway.api_key 自 Phase 2 起不参与认证：网关 Key 由数据库生成。
  writeText(
    cfgFile,
    `{
  "gateway": {
    "api_key": "",
    "listen": "127.0.0.1",
    "port": ${gatewayPort},
    "sse_idle_seconds": 1,
    "max_body_bytes": 1024
  },
  "providers": [
    { "id": "mock", "name": "mock", "openai_base_url": "http://127.0.0.1:${mockPort}/v1",
      "api_key": "sk-mock-provider-key", "models": ["mock-gpt-4o", "mock-claude-sonnet-4-5"], "allow_internal": true, "timeout_seconds": 120 },
    { "id": "mock-slow", "name": "mock-slow", "openai_base_url": "http://127.0.0.1:${mockPort}/v1", "api_key": "sk-slow",
      "models": ["slow-model"], "allow_internal": true, "timeout_seconds": 1 },
    { "id": "dead", "name": "dead", "openai_base_url": "http://127.0.0.1:9998/v1", "api_key": "sk-dead",
      "models": ["dead-model"], "allow_internal": true, "timeout_seconds": 5 },
    { "id": "mock-extra", "name": "mock-extra", "openai_base_url": "http://127.0.0.1:${mockPort}/v1", "api_key": "sk-extra",
      "models": ["extra-model"], "allow_internal": true, "timeout_seconds": 60,
      "extra_headers": { "x-tenant": "agora" }, "extra_body": { "temperature": 0.1, "top_p": 0.9 } }
  ]
}
`,
  );

  const bodyOpenAI = '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}';
  const bodyOpenAIStream = '{"model":"mock-gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}';
  const bodyNoModel = '{"messages":[]}';
  const bodyUnknown = '{"model":"nope-1","messages":[]}';
  const bodySlow = '{"model":"slow-model","messages":[]}';
  const bodyDead = '{"model":"dead-model","messages":[]}';
  const bodyExtra = '{"model":"extra-model","messages":[]}';
  const bodyBig = `{"model":"mock-gpt-4o","pad":"${'x'.repeat(2048)}"}`;

  const base = `http://127.0.0.1:${gatewayPort}`;
  const mockBase = `http://127.0.0.1:${mockPort}`;
  const jsonHeader = { 'content-type': 'application/json' };

  const mock = await startMock({ workDir, port: mockPort });
  const gw = startGateway({ exe, workDir, name: 'gateway', config: cfgFile, dataDir, port: gatewayPort });

  try {
    const key = await waitForGatewayKey(gw);
    if (!key) throw new Error(`未能从启动日志取得网关 Key：\n${gw.logText}`);
    console.log(`  网关 Key 已取得：${key.slice(0, 7)}***`);
    const openAIHeaders = { ...jsonHeader, authorization: `Bearer ${key}` };

    if (!(await waitForHttp(`${base}/healthz`, 20000))) {
      throw new Error(`网关未就绪：\n${gw.logText}\n${gw.errText}`);
    }

    console.log('== 断言 ==');

    let res = await httpRequest(`${base}/healthz`, { timeoutMs: 5000 });
    rec.assertThat('healthz 返回 200', res.code === 200, `code=${res.code}`);

    res = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: jsonHeader, body: bodyOpenAI });
    rec.assertThat('无凭证（OpenAI 入站）→ 401', res.code === 401, `code=${res.code}`);
    rec.assertThat('OpenAI 风格错误体含 error.code=invalid_api_key', res.body.includes('"code":"invalid_api_key"'));

    res = await httpRequest(`${base}/v1/messages`, { method: 'POST', headers: jsonHeader, body: bodyOpenAI });
    rec.assertThat('Anthropic 端点已下线 → 404', res.code === 404, `code=${res.code}`);

    res = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: openAIHeaders, body: bodyNoModel });
    rec.assertThat('缺少 model → 400', res.code === 400, `code=${res.code}`);

    res = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: openAIHeaders, body: bodyUnknown });
    rec.assertThat(
      '未知模型 → 404 model_not_found',
      res.code === 404 && res.body.includes('model_not_found'),
      `code=${res.code}`,
    );

    res = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: openAIHeaders, body: bodyOpenAI });
    rec.assertThat(
      '非流式透传 → 200 且内容来自上游',
      res.code === 200 && res.body.includes('mock 非流式回复'),
      `code=${res.code}`,
    );

    let upstream = await httpRequest(`${mockBase}/__requests`, { timeoutMs: 5000 });
    let last = upstream.json?.requests?.at(-1);
    rec.assertThat(
      '上游收到替换后的供应商凭证',
      last?.received?.authorization === 'Bearer sk-mock-provider-key',
      `auth=${last?.received?.authorization}`,
    );

    res = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...openAIHeaders, 'x-mock-chunks': '3', 'x-mock-gap': '40' },
      body: bodyOpenAIStream,
    });
    rec.assertThat('OpenAI 流式透传 → 含 [DONE]', res.body.includes('[DONE]'));

    res = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...openAIHeaders, 'anthropic-version': '2023-06-01', 'x-mock-chunks': '3', 'x-mock-gap': '30' },
      body: bodyOpenAIStream,
    });
    rec.assertThat('流式透传（携带业务头）→ 含 [DONE]', res.body.includes('[DONE]'));

    upstream = await httpRequest(`${mockBase}/__requests`, { timeoutMs: 5000 });
    last = upstream.json?.requests?.at(-1);
    rec.assertThat('业务头 anthropic-version 被透传到上游', last?.received?.['anthropic-version'] === '2023-06-01');
    rec.assertThat('x-api-key 不再出现于上游请求', last?.received?.['x-api-key'] === null);

    res = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...openAIHeaders, 'x-mock-silence': '3', 'x-mock-chunks': '2' },
      body: bodyOpenAIStream,
      timeoutMs: 15000,
    });
    rec.assertThat('SSE 空闲注入心跳（keep-alive）', res.body.includes(': keep-alive'));

    res = await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...openAIHeaders, 'x-mock-slow': '5' },
      body: bodySlow,
      timeoutMs: 30000,
    });
    rec.assertThat(
      '上游超时 → 504 upstream_timeout',
      res.code === 504 && res.body.includes('upstream_timeout'),
      `code=${res.code}`,
    );

    res = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: openAIHeaders, body: bodyDead });
    rec.assertThat(
      '上游不可达 → 502 upstream_unreachable',
      res.code === 502 && res.body.includes('upstream_unreachable'),
      `code=${res.code}`,
    );

    res = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: openAIHeaders, body: bodyBig });
    rec.assertThat(
      '请求体超限 → 413 payload_too_large',
      res.code === 413 && res.body.includes('payload_too_large'),
      `code=${res.code}`,
    );

    res = await httpRequest(`${base}/v1/chat/completions`, { method: 'POST', headers: openAIHeaders, body: bodyExtra });
    upstream = await httpRequest(`${mockBase}/__requests`, { timeoutMs: 5000 });
    last = upstream.json?.requests?.at(-1);
    rec.assertThat('extra_headers 注入到上游', last?.xHeaders?.['x-tenant'] === 'agora');
    rec.assertThat(
      'extra_body 合并进请求体',
      Boolean(last?.bodyPreview?.includes('"temperature":0.1') && last?.bodyPreview?.includes('"top_p":0.9')),
    );

    // 客户端 1 秒后主动断开（对应 curl --max-time 1），随后确认 gateway/上游都没被拖垮。
    await httpRequest(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: { ...openAIHeaders, 'x-mock-silence': '3', 'x-mock-chunks': '2' },
      body: bodyOpenAIStream,
      abortAfterMs: 1000,
    });
    await new Promise((resolve) => setTimeout(resolve, 3000));

    const alive = await httpRequest(`${mockBase}/v1/models`, { timeoutMs: 5000 });
    rec.assertThat('mock 上游在断连后仍存活', alive.code === 200, `code=${alive.code}`);
  } finally {
    await stopAll();
  }

  // 进程已停止、日志落盘后再断言断连取消行为（对应 PowerShell 版本在 finally 之后的断言）。
  rec.assertThat('客户端断开 → 上游请求被取消（日志含 context canceled）', gw.logText.includes('context canceled'));

  console.log(`  临时目录：${workDir}`);
}, { label: '冒烟结果' });
