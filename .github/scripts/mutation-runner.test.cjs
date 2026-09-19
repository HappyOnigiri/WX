'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const runner = require('./mutation-runner.cjs');

function options(root, script, timeoutSeconds = 5) {
  return {
    timeoutSeconds,
    graceSeconds: 0.05,
    output: path.join(root, 'gremlins.json'),
    executionResult: path.join(root, 'execution.json'),
    diagnosticsDir: path.join(root, 'diagnostics'),
    shard: 'daemon-file', profile: 'internal/daemon', runId: '10', attempt: '2', sha: 'a'.repeat(40),
    command: [process.execPath, '-e', script, path.join(root, 'gremlins.json')],
  };
}

test('records completed, failed, missing, and invalid results', async () => {
  for (const [name, script, want] of [
    ['completed', 'require("fs").writeFileSync(process.argv[1], "{}")', 'completed'],
    ['failed', 'process.exit(7)', 'gremlins_failed'],
    ['missing', '', 'result_invalid'],
    ['invalid', 'require("fs").writeFileSync(process.argv[1], "{")', 'result_invalid'],
  ]) {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), `wx-mutation-${name}-`));
    try {
      const result = await runner.run(options(root, script));
      assert.equal(result.status, want);
      assert.equal(JSON.parse(fs.readFileSync(path.join(root, 'execution.json'))).status, want);
      assert.ok(fs.existsSync(path.join(root, 'diagnostics', 'gremlins.stdout.log')));
      assert.ok(fs.existsSync(path.join(root, 'diagnostics', 'gremlins.stderr.log')));
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  }
});

test('times out the complete process group without leaving a child', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wx-mutation-timeout-'));
  const pidPath = path.join(root, 'child.pid');
  const script = `const cp=require('node:child_process');const fs=require('node:fs');const c=cp.spawn(process.execPath,['-e','process.on("SIGTERM",()=>{});setInterval(()=>{},1000)']);fs.writeFileSync(${JSON.stringify(pidPath)},String(c.pid));setInterval(()=>{},1000)`;
  try {
    const result = await runner.run(options(root, script, 0.1));
    assert.equal(result.status, 'measurement_timed_out');
    const pid = Number(fs.readFileSync(pidPath, 'utf8'));
    await new Promise((resolve) => setTimeout(resolve, 100));
    assert.throws(() => process.kill(pid, 0), /ESRCH/u);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('accepts a missing result only when a zero-mutation dry-run authorized it', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wx-mutation-zero-'));
  try {
    const input = options(root, '');
    input.allowMissingResult = 'true';
    const result = await runner.run(input);
    assert.equal(result.status, 'completed');
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
