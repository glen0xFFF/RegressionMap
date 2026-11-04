// A stand-in for the RegressionMap API, used by the CI script tests.
//
// POST /api/analyses answers `post` (default: 202 with the analysis id).
// GET /api/analyses/<ID> answers the entries of `gets` in turn and repeats the
// last one. Each answer is { status, json } or { status, raw }, plus optional
// headers. The Content-Type is a bare application/json, as the Go server sends
// it. Every request and every TCP connection is recorded.
//
// This module only exports functions: importing it starts nothing.
import http from 'node:http';

export const ID = 'a-test-1';

export async function startServer({ post, gets = [], host = '127.0.0.1' } = {}) {
  const requests = [];
  const state = { connections: 0, gets: 0 };
  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      const body = Buffer.concat(chunks).toString('utf8');
      requests.push({ method: req.method, url: req.url, headers: req.headers, body, at: Date.now() });
      let answer;
      if (req.method === 'POST' && req.url === '/api/analyses') {
        answer = post || { status: 202, json: { id: ID, status: 'running', repo: 'my-repo' } };
      } else if (req.method === 'GET' && req.url === '/api/analyses/' + ID && gets.length > 0) {
        answer = gets[Math.min(state.gets, gets.length - 1)];
        state.gets++;
      } else {
        answer = { status: 404, json: { error: { code: 'not_found', message: 'no route' } } };
      }
      const text = answer.raw !== undefined ? answer.raw : JSON.stringify(answer.json);
      res.writeHead(answer.status, { 'Content-Type': 'application/json', ...(answer.headers || {}) });
      res.end(text);
    });
  });
  server.on('connection', () => {
    state.connections++;
  });
  // A TLS handshake or other garbage: drop the connection without an answer.
  server.on('clientError', (_err, socket) => socket.destroy());
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, host, resolve);
  });
  const { port } = server.address();
  const shown = host.includes(':') ? `[${host}]` : host;
  return {
    port,
    url: `http://${shown}:${port}`,
    requests,
    state,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
}

// A loopback port that nothing listens on (bound once, then released).
export async function freePort() {
  const server = http.createServer();
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address();
  await new Promise((resolve) => server.close(resolve));
  return port;
}
