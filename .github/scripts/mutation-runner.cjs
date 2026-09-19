'use strict';

const childProcess = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const EXECUTION_SCHEMA_VERSION = 1;

function parseArguments(argv) {
  const options = { timeoutSeconds: 9600, graceSeconds: 10 };
  let index = 0;
  for (; index < argv.length; index += 1) {
    if (argv[index] === '--') break;
    const name = argv[index];
    if (!name.startsWith('--') || index + 1 >= argv.length) throw new Error(`invalid argument ${name}`);
    const value = argv[++index];
    const key = name.slice(2).replace(/-([a-z])/gu, (_, letter) => letter.toUpperCase());
    options[key] = value;
  }
  options.command = argv.slice(index + 1);
  options.timeoutSeconds = Number(options.timeoutSeconds);
  options.graceSeconds = Number(options.graceSeconds);
  for (const key of ['output', 'executionResult', 'diagnosticsDir', 'shard', 'profile', 'runId', 'attempt', 'sha']) {
    if (!options[key]) throw new Error(`--${key.replace(/[A-Z]/gu, (letter) => `-${letter.toLowerCase()}`)} is required`);
  }
  if (options.command.length === 0) throw new Error('command is required after --');
  if (!Number.isFinite(options.timeoutSeconds) || options.timeoutSeconds <= 0) throw new Error('timeout-seconds must be positive');
  if (!Number.isFinite(options.graceSeconds) || options.graceSeconds < 0) throw new Error('grace-seconds must be non-negative');
  return options;
}

function safeCommand(command, args, options = {}) {
  try {
    return childProcess.execFileSync(command, args, { encoding: 'utf8', timeout: 5000, ...options }).trim();
  } catch {
    return 'unavailable';
  }
}

function heartbeat(child, startedAt, heartbeatPath) {
  const processInfo = safeCommand('ps', ['-o', 'pid=,ppid=,pgid=', '-p', String(child.pid)]);
  const processCount = safeCommand('sh', ['-c', 'ps -e -o pid= | wc -l']);
  const disk = safeCommand('df', ['-Pk', '.']).split('\n').at(-1) || 'unavailable';
  const line = `[mutation heartbeat] elapsed=${Math.floor((Date.now() - startedAt) / 1000)}s free_memory_bytes=${os.freemem()} process_count=${processCount} target_pid_ppid_pgid="${processInfo}" disk=${disk}\n`;
  process.stdout.write(line);
  fs.appendFileSync(heartbeatPath, line, { mode: 0o600 });
}

function killGroup(pid, signal) {
  try {
    process.kill(-pid, signal);
  } catch (error) {
    if (error?.code !== 'ESRCH') throw error;
  }
}

function validateResult(resultPath) {
  if (!fs.existsSync(resultPath)) return 'result file is missing';
  const data = fs.readFileSync(resultPath);
  if (data.length === 0) return 'result file is empty';
  try {
    const value = JSON.parse(data.toString('utf8'));
    if (!value || typeof value !== 'object' || Array.isArray(value)) return 'result JSON is not an object';
  } catch (error) {
    return `result JSON is invalid: ${error.message}`;
  }
  return '';
}

async function run(options) {
  fs.mkdirSync(options.diagnosticsDir, { recursive: true });
  fs.mkdirSync(path.dirname(options.executionResult), { recursive: true });
  const stdoutPath = path.join(options.diagnosticsDir, 'gremlins.stdout.log');
  const stderrPath = path.join(options.diagnosticsDir, 'gremlins.stderr.log');
  const heartbeatPath = path.join(options.diagnosticsDir, 'heartbeat.log');
  const stdout = fs.createWriteStream(stdoutPath, { flags: 'w', mode: 0o600 });
  const stderr = fs.createWriteStream(stderrPath, { flags: 'w', mode: 0o600 });
  const startedAt = Date.now();
  const child = childProcess.spawn(options.command[0], options.command.slice(1), {
    detached: true,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  child.stdout.pipe(stdout);
  child.stdout.pipe(process.stdout);
  child.stderr.pipe(stderr);
  child.stderr.pipe(process.stderr);
  let timedOut = false;
  let escalation = Promise.resolve();
  const interval = setInterval(() => heartbeat(child, startedAt, heartbeatPath), 60000);
  heartbeat(child, startedAt, heartbeatPath);
  const timeout = setTimeout(() => {
    timedOut = true;
    process.stderr.write(`[mutation runner] measurement deadline exceeded; terminating process group ${child.pid}\n`);
    killGroup(child.pid, 'SIGTERM');
    escalation = new Promise((resolve) => setTimeout(() => {
      killGroup(child.pid, 'SIGKILL');
      resolve();
    }, options.graceSeconds * 1000));
  }, options.timeoutSeconds * 1000);
  const completion = await new Promise((resolve) => {
    child.once('error', (error) => resolve({ code: null, signal: null, error }));
    child.once('exit', (code, signal) => resolve({ code, signal, error: null }));
  });
  clearInterval(interval);
  clearTimeout(timeout);
  await escalation;
  await Promise.all([new Promise((resolve) => stdout.end(resolve)), new Promise((resolve) => stderr.end(resolve))]);
  let status = 'completed';
  let detail = '';
  if (timedOut) {
    status = 'measurement_timed_out';
  } else if (completion.error || completion.code !== 0) {
    status = 'gremlins_failed';
    detail = completion.error?.message || `exit ${completion.code ?? 'none'} signal ${completion.signal || 'none'}`;
  } else {
    detail = validateResult(options.output);
    if (detail === 'result file is missing' && String(options.allowMissingResult) === 'true') detail = '';
    if (detail) status = 'result_invalid';
  }
  const finishedAt = Date.now();
  const execution = {
    schema_version: EXECUTION_SCHEMA_VERSION,
    run_id: String(options.runId),
    run_attempt: String(options.attempt),
    test_sha: String(options.sha),
    shard: String(options.shard),
    profile: String(options.profile),
    stage: 'measurement',
    status,
    exit_code: completion.code,
    signal: completion.signal,
    duration_seconds: (finishedAt - startedAt) / 1000,
    detail,
  };
  fs.writeFileSync(options.executionResult, `${JSON.stringify(execution, null, 2)}\n`, { mode: 0o600 });
  return execution;
}

async function main() {
  const result = await run(parseArguments(process.argv.slice(2)));
  if (result.status !== 'completed') process.exitCode = 1;
}

if (require.main === module) main().catch((error) => {
  process.stderr.write(`${error.stack || error.message}\n`);
  process.exitCode = 1;
});

module.exports = { EXECUTION_SCHEMA_VERSION, parseArguments, run, validateResult };
