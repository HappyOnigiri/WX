'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const childProcess = require('node:child_process');
const reporter = require('./report-flaky-tests.cjs');

function writeZip(t, files) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'wx-ci-report-test-'));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const source = path.join(directory, 'source');
  for (const [name, body] of Object.entries(files)) {
    const target = path.join(source, name);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, typeof body === 'number' ? '' : body);
    if (typeof body === 'number') fs.truncateSync(target, body);
  }
  const zipPath = path.join(directory, 'artifact.zip');
  childProcess.execFileSync('zip', ['-qr', zipPath, '.'], { cwd: source });
  return zipPath;
}

function manifest(runId, attempt, profile = 'coverage') {
  return {
    schema_version: 1,
    profile,
    recovered: true,
    run_id: String(runId),
    run_attempt: String(attempt),
    test_sha: '0123456789abcdef0123456789abcdef01234567',
    initial: { command: ['go', 'test', '-json'], shuffle: '123' },
    retries: [{ command: ['go', 'test', '-run=^TestFlaky$'], shuffle: '123' }],
    recoveries: [{
      package: 'example.test',
      declaration: { path: 'internal/example/flaky_test.go', function: 'TestFlaky', line: 12 },
      failed_tests: ['TestFlaky/sub test'],
      retry_index: 1,
    }],
  };
}

const source = {
  owner: 'HappyOnigiri',
  repo: 'WX',
  runId: '10',
  attempt: '1',
  event: 'push',
  ref: 'main',
  testSha: '0123456789abcdef0123456789abcdef01234567',
  runUrl: 'https://github.com/HappyOnigiri/WX/actions/runs/10',
  commitUrl: 'https://github.com/HappyOnigiri/WX/commit/0123456789abcdef0123456789abcdef01234567',
};

test('groups coverage and race evidence under one issue key', () => {
  const groups = reporter.aggregateManifests([
    { artifactName: 'coverage', manifest: manifest('10', '1', 'coverage') },
    { artifactName: 'race', manifest: manifest('10', '1', 'race-daemon') },
  ], source);
  assert.equal(groups.length, 1);
  assert.equal(groups[0].items.length, 2);
  assert.match(reporter.buildIssueBody(groups[0], source), /race-daemon/);
});

test('creates once and comments on a later occurrence of the same issue', async () => {
  const issues = [];
  const comments = new Map();
  const calls = [];
  const github = { rest: { issues: {
    listForRepo: async () => ({ data: issues }),
    listComments: async ({ issue_number: number }) => ({ data: comments.get(number) || [] }),
    create: async (request) => { const issue = { number: issues.length + 1, title: request.title, body: request.body, state: 'open' }; issues.push(issue); calls.push(['create', issue.number]); return { data: issue }; },
    createComment: async ({ issue_number: number, body }) => { const list = comments.get(number) || []; list.push({ body }); comments.set(number, list); calls.push(['comment', number]); return { data: {} }; },
    update: async () => ({ data: {} }),
  } } };
  const group1 = reporter.aggregateManifests([{ artifactName: 'coverage', manifest: manifest('10', '1') }], source)[0];
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group: group1, source }), 'created');
  const source2 = { ...source, runId: '11', runUrl: 'https://github.com/HappyOnigiri/WX/actions/runs/11' };
  const group2 = reporter.aggregateManifests([{ artifactName: 'coverage', manifest: manifest('11', '1') }], source2)[0];
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group: group2, source: source2 }), 'commented');
  assert.deepEqual(calls, [['create', 1], ['comment', 1]]);
});

test('reopens a closed issue before commenting', async () => {
  const issue = { number: 3, title: '[flaky] internal/example/flaky_test.go: TestFlaky', body: '', state: 'closed' };
  const calls = [];
  const github = { rest: { issues: {
    listForRepo: async () => ({ data: [issue] }),
    listComments: async () => ({ data: [] }),
    update: async ({ state }) => { issue.state = state; calls.push(['update', state]); return { data: issue }; },
    createComment: async () => { calls.push(['comment']); return { data: {} }; },
  } } };
  const group = reporter.aggregateManifests([{ artifactName: 'coverage', manifest: manifest('10', '1') }], source)[0];
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group, source }), 'reopened-commented');
  assert.deepEqual(calls, [['update', 'open'], ['comment']]);
});

test('runs the report workflow with mocked Actions and issue APIs', async () => {
  const issues = [];
  const comments = new Map();
  let currentRun = '10';
  const sourceRun = (id) => ({ name: 'CI', path: '.github/workflows/ci.yml', run_attempt: 1, event: 'push', head_branch: 'main', head_sha: '0123456789abcdef0123456789abcdef01234567', html_url: `https://github.com/HappyOnigiri/WX/actions/runs/${id}` });
  const github = { rest: {
    actions: {
      getWorkflowRun: async ({ run_id: id }) => { currentRun = String(id); return { data: sourceRun(id) }; },
      listJobsForWorkflowRun: async () => ({ data: { jobs: [{ name: 'coverage-tests', run_attempt: 1, conclusion: 'failure', steps: [{ name: 'Upload CI test report', conclusion: 'success' }] }] } }),
      listWorkflowRunArtifacts: async () => ({ data: { artifacts: [{ id: 1, name: `ci-tests-coverage-${currentRun}-1`, expired: false, workflow_run: { id: Number(currentRun) } }] } }),
    },
    issues: {
      listForRepo: async () => ({ data: issues }),
      listComments: async ({ issue_number: number }) => ({ data: comments.get(number) || [] }),
      create: async (request) => { const issue = { number: issues.length + 1, title: request.title, body: request.body, state: 'open' }; issues.push(issue); return { data: issue }; },
      createComment: async ({ issue_number: number, body }) => { const list = comments.get(number) || []; list.push({ body }); comments.set(number, list); return { data: {} }; },
      update: async () => ({ data: {} }),
    },
  } };
  const reports = [{ artifactName: 'ci-tests-coverage-10-1', manifest: manifest('10', '1') }];
  const first = await reporter.run({ github, owner: 'HappyOnigiri', repo: 'WX', sourceRunId: '10', sourceAttempt: '1', reports });
  assert.equal(first.results[0].action, 'created');
  const second = await reporter.run({ github, owner: 'HappyOnigiri', repo: 'WX', sourceRunId: '11', sourceAttempt: '1', reports: [{ artifactName: 'ci-tests-coverage-11-1', manifest: manifest('11', '1') }] });
  assert.equal(second.results[0].action, 'commented');
});

test('rejects a path escape in an artifact manifest', () => {
  const value = manifest('10', '1');
  value.recoveries[0].declaration.path = '../outside.go';
  assert.throws(() => reporter.validateManifest(value), /repository-relative path/);
});

test('reads manifest.json out of an artifact zip', (t) => {
  const zipPath = writeZip(t, { 'coverage/manifest.json': JSON.stringify(manifest('10', '1')), 'coverage/initial.log': 'log' });
  assert.equal(reporter.readZipManifest(zipPath, 'ci-tests-coverage-10-1').profile, 'coverage');
});

test('rejects an artifact that expands beyond the size limit before extracting it', (t) => {
  const zipPath = writeZip(t, { 'manifest.json': JSON.stringify(manifest('10', '1')), 'big.bin': 101 * 1024 * 1024 });
  assert.throws(() => reporter.readZipManifest(zipPath, 'ci-tests-coverage-10-1'), /expands beyond the size limit/);
});

test('accepts a successful manifest without recoveries', () => {
  const value = manifest('10', '1');
  delete value.recoveries;
  assert.deepEqual(reporter.validateManifest(value).recoveries, []);
  assert.equal(reporter.aggregateManifests([{ artifactName: 'coverage', manifest: value }], source).length, 0);
});
