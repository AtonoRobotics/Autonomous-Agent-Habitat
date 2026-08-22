#!/usr/bin/env node
// AMH control-plane UI server: the first-party user-surface extension
// against daemon/api's admin routes (daemon/api/controlplane.go).
//
// Per the v10 spec's governing decision 1 ("user surfaces attach as
// replaceable extensions"), this is NOT baked into the Go daemon — it is
// installed and activated through the extension registry itself
// (daemon/extensions), exactly like any other extension, and could be
// replaced or removed the same way. See extension.json for the manifest
// that registers it, and install.sh for how an operator does that.
//
// This process does two things, both deliberately minimal (no npm
// dependency, no build step — Node 18+'s built-in http and global fetch
// are enough):
//   1. Serves the static single-page app in public/.
//   2. Proxies /api/* to the daemon's admin API (AMH_API_BASE_URL),
//      forwarding the browser's Authorization header untouched. The
//      proxy exists only to keep the browser same-origin (avoiding CORS
//      entirely) — it never sees, stores, or needs to know any
//      credential itself; the operator/agent token lives only in the
//      browser's own localStorage and travels in the Authorization
//      header the browser sets.
'use strict';

const http = require('http');
const fs = require('fs');
const path = require('path');

const PORT = process.env.PORT ? parseInt(process.env.PORT, 10) : 8091;
const DAEMON_API_BASE_URL = process.env.AMH_API_BASE_URL || 'http://127.0.0.1:8090';
const PUBLIC_DIR = path.join(__dirname, 'public');

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
};

const server = http.createServer((req, res) => {
  if (req.url.startsWith('/api/')) {
    proxyToDaemon(req, res).catch((err) => {
      res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'control-plane-ui: proxy to daemon failed: ' + String(err) }));
    });
    return;
  }
  serveStatic(req, res);
});

function serveStatic(req, res) {
  const urlPath = (req.url === '/' ? '/index.html' : req.url).split('?')[0];
  const filePath = path.normalize(path.join(PUBLIC_DIR, urlPath));
  if (!filePath.startsWith(PUBLIC_DIR)) {
    res.writeHead(403);
    res.end('forbidden');
    return;
  }
  fs.readFile(filePath, (err, data) => {
    if (err) {
      res.writeHead(404);
      res.end('not found');
      return;
    }
    res.writeHead(200, { 'Content-Type': MIME[path.extname(filePath)] || 'application/octet-stream' });
    res.end(data);
  });
}

async function proxyToDaemon(req, res) {
  const targetUrl = DAEMON_API_BASE_URL + req.url.slice('/api'.length);

  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  const body = chunks.length ? Buffer.concat(chunks) : undefined;

  const headers = {};
  if (req.headers['authorization']) headers['authorization'] = req.headers['authorization'];
  if (req.headers['content-type']) headers['content-type'] = req.headers['content-type'];

  const upstream = await fetch(targetUrl, {
    method: req.method,
    headers,
    body: body && req.method !== 'GET' && req.method !== 'HEAD' ? body : undefined,
  });
  const respBody = Buffer.from(await upstream.arrayBuffer());
  res.writeHead(upstream.status, { 'Content-Type': upstream.headers.get('content-type') || 'application/json' });
  res.end(respBody);
}

server.listen(PORT, '127.0.0.1', () => {
  console.log(`amh-control-plane-ui listening on http://127.0.0.1:${PORT} (proxying ${DAEMON_API_BASE_URL})`);
});
