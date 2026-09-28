#!/usr/bin/env node
// AgoraModel Mock 上游服务（T0.4）
//
// 用途：本地模拟 OpenAI / Anthropic 两种协议的上游，用于验证网关的透传、头改写、
//      SSE 心跳保活、超时与错误处理；并提供 /__requests 便于自动化断言。
//
// 启动：node --no-deprecation tools/mock-upstream/server.mjs   （默认端口 9999）
//       PORT=9999 node --no-deprecation tools/mock-upstream/server.mjs
//
// 加上 --no-deprecation 是为了抑制 Node 22+ 的 DEP0040 (punycode) 与
// DEP0169 (url.parse) 警告：mock 用 `new URL(req.url, ...)` 解析请求时，
// 内部 URL 实现会间接触发这两条 deprecation，但脚本本身无相关问题。
//
// 行为开关（URL query 参数，优先级高于环境变量）：
//   status=401|429|500   直接返回该状态码（JSON 错误体）
//   bad=1                返回非 JSON 文本（模拟 Cloudflare 拦截页）
//   slow=5               首字节前延迟 N 秒（测试首字节/整体超时）
//   silence=20           流式过程中静默 N 秒（测试 SSE 心跳保活）
//   chunks=5             流式分块数量（默认 5）
//   gap=200              流式分块间隔毫秒（默认 200）
//   big=1                非流式返回约 2MB 超大响应
//
// 断言端点：
//   GET    /__requests   返回最近 100 条请求摘要（含收到的认证头、model、body 大小）
//   DELETE /__requests   清空记录

import http from 'node:http';

const PORT = Number(process.env.PORT || 9999);
const MODELS = ['mock-gpt-4o', 'mock-claude-sonnet-4-5', 'mock-deepseek-v3'];

/** 最近请求摘要，供 /__requests 断言使用 */
const requests = [];

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function qp(req, key) {
  return new URL(req.url, 'http://localhost').searchParams.get(key);
}
// param 优先读 x-mock-<key> 请求头（便于经网关透传注入故障），其次读 query 参数
function param(req, key) {
  const header = req.headers['x-mock-' + key];
  if (header !== undefined) return Array.isArray(header) ? header[0] : header;
  return qp(req, key);
}

function num(req, key, def) {
  const v = param(req, key);
  return v === null || v === undefined ? def : Number(v);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function modelOf(buf) {
  try {
    return JSON.parse(buf.toString('utf8')).model ?? null;
  } catch {
    return null;
  }
}

function record(req, body) {
  const entry = {
    ts: new Date().toISOString(),
    method: req.method,
    url: req.url,
    received: {
      authorization: req.headers['authorization'] ?? null,
      'x-api-key': req.headers['x-api-key'] ?? null,
      'anthropic-version': req.headers['anthropic-version'] ?? null,
      'anthropic-beta': req.headers['anthropic-beta'] ?? null,
      'accept-encoding': req.headers['accept-encoding'] ?? null,
      host: req.headers['host'] ?? null,
      'user-agent': req.headers['user-agent'] ?? null,
    },
    bodyBytes: body.length,
    model: modelOf(body),
    bodyPreview: body.toString('utf8').slice(0, 512),
    xHeaders: Object.fromEntries(Object.entries(req.headers).filter(([k]) => k.startsWith('x-'))),
  };
  requests.push(entry);
  if (requests.length > 100) requests.shift();
  console.log(`[mock] ${entry.method} ${entry.url} model=${entry.model} bytes=${entry.bodyBytes}`);
  return entry;
}

function sendJSON(res, status, obj) {
  const payload = JSON.stringify(obj);
  res.writeHead(status, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': Buffer.byteLength(payload),
  });
  res.end(payload);
}

function sseHeaders(res) {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream; charset=utf-8',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
  });
}

// ---------------- OpenAI 协议 ----------------

async function openaiStream(req, res, model) {
  const chunks = num(req, 'chunks', 5);
  const gap = num(req, 'gap', 200);
  const silence = num(req, 'silence', 0);
  sseHeaders(res);
  for (let i = 0; i < chunks; i++) {
    if (res.destroyed || res.writableEnded) {
      console.log('[mock] 客户端已断开，停止推送流式分块');
      return;
    }
    const chunk = {
      id: 'chatcmpl-mock',
      object: 'chat.completion.chunk',
      created: Math.floor(Date.now() / 1000),
      model,
      choices: [
        {
          index: 0,
          delta: i === 0 ? { role: 'assistant', content: `chunk-${i + 1} ` } : { content: `chunk-${i + 1} ` },
          finish_reason: null,
        },
      ],
    };
    res.write(`data: ${JSON.stringify(chunk)}\n\n`);
    if (silence > 0 && i === 0) {
      console.log(`[mock] 静默 ${silence}s（用于 SSE 心跳测试）`);
      await sleep(silence * 1000);
    }
    await sleep(gap);
  }
  res.write(
    `data: ${JSON.stringify({
      id: 'chatcmpl-mock',
      object: 'chat.completion.chunk',
      model,
      choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
    })}\n\n`,
  );
  res.write('data: [DONE]\n\n');
  res.end();
}

function openaiJSON(req, res, model) {
  if (num(req, 'big', 0) === 1) {
    return sendJSON(res, 200, {
      id: 'chatcmpl-mock-big',
      object: 'chat.completion',
      model,
      choices: [{ index: 0, message: { role: 'assistant', content: 'x'.repeat(2 * 1024 * 1024) }, finish_reason: 'stop' }],
    });
  }
  sendJSON(res, 200, {
    id: 'chatcmpl-mock',
    object: 'chat.completion',
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, message: { role: 'assistant', content: 'mock 非流式回复' }, finish_reason: 'stop' }],
    usage: { prompt_tokens: 12, completion_tokens: 8, total_tokens: 20 },
  });
}

// ---------------- Anthropic 协议 ----------------

async function anthropicStream(req, res, model) {
  const chunks = num(req, 'chunks', 5);
  const gap = num(req, 'gap', 200);
  const silence = num(req, 'silence', 0);
  sseHeaders(res);
  const send = (event, data) => res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
  send('message_start', {
    type: 'message_start',
    message: {
      id: 'msg_mock',
      type: 'message',
      role: 'assistant',
      model,
      content: [],
      stop_reason: null,
      usage: { input_tokens: 12, output_tokens: 0 },
    },
  });
  send('content_block_start', { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } });
  for (let i = 0; i < chunks; i++) {
    if (res.destroyed || res.writableEnded) {
      console.log('[mock] 客户端已断开，停止推送流式分块');
      return;
    }
    send('content_block_delta', {
      type: 'content_block_delta',
      index: 0,
      delta: { type: 'text_delta', text: `chunk-${i + 1} ` },
    });
    if (silence > 0 && i === 0) {
      console.log(`[mock] 静默 ${silence}s（用于 SSE 心跳测试）`);
      await sleep(silence * 1000);
    }
    await sleep(gap);
  }
  send('content_block_stop', { type: 'content_block_stop', index: 0 });
  send('message_delta', { type: 'message_delta', delta: { stop_reason: 'end_turn' }, usage: { output_tokens: chunks * 3 } });
  send('message_stop', { type: 'message_stop' });
  res.end();
}

function anthropicJSON(req, res, model) {
  sendJSON(res, 200, {
    id: 'msg_mock',
    type: 'message',
    role: 'assistant',
    model,
    content: [{ type: 'text', text: 'mock 非流式回复' }],
    stop_reason: 'end_turn',
    usage: { input_tokens: 12, output_tokens: 8 },
  });
}

// ---------------- 服务器 ----------------

const server = http.createServer(async (req, res) => {
  const path = new URL(req.url, 'http://localhost').pathname;

  if (path === '/__requests') {
    if (req.method === 'DELETE') {
      requests.length = 0;
      return sendJSON(res, 200, { ok: true, cleared: true });
    }
    return sendJSON(res, 200, { count: requests.length, requests });
  }

  // 客户端断开时不要让未捕获的 'error' 事件杀掉进程
  req.on('error', (err) => console.log(`[mock] 请求流错误: {err.code || err.message}`));
  res.on('error', (err) => console.log(`[mock] 响应流错误（可能客户端断开）: {err.code || err.message}`));

  const body = await readBody(req);
  record(req, body);

  // 故障注入：在任何协议处理之前生效
  const status = num(req, 'status', 0);
  if (status >= 400) {
    return sendJSON(res, status, { error: { message: `mock 注入错误 ${status}`, type: 'mock_error' } });
  }
  if (num(req, 'bad', 0) === 1) {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    return res.end('<html><body>mock 非 JSON 响应（模拟拦截页）</body></html>');
  }
  const slow = num(req, 'slow', 0);
  if (slow > 0) {
    console.log(`[mock] 首字节前延迟 ${slow}s`);
    await sleep(slow * 1000);
  }

  if (path === '/v1/models' && req.method === 'GET') {
    return sendJSON(res, 200, {
      object: 'list',
      data: MODELS.map((id) => ({ id, object: 'model', owned_by: 'mock' })),
    });
  }

  if (path === '/v1/chat/completions' && req.method === 'POST') {
    let stream = false;
    try {
      stream = JSON.parse(body.toString('utf8')).stream === true;
    } catch {
      /* 非法 JSON 时按非流式处理 */
    }
    const model = modelOf(body) ?? 'mock-gpt-4o';
    return stream ? openaiStream(req, res, model) : openaiJSON(req, res, model);
  }

  if (path === '/v1/messages' && req.method === 'POST') {
    let stream = false;
    try {
      stream = JSON.parse(body.toString('utf8')).stream === true;
    } catch {
      /* 同上 */
    }
    const model = modelOf(body) ?? 'mock-claude-sonnet-4-5';
    return stream ? anthropicStream(req, res, model) : anthropicJSON(req, res, model);
  }

  sendJSON(res, 404, { error: { message: `mock 未实现的路径: ${req.method} ${path}`, type: 'not_found' } });
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`[mock] 上游已启动 http://127.0.0.1:${PORT}`);
  console.log(`[mock] 模型列表: ${MODELS.join(', ')}`);
});
