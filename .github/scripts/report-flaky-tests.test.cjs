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
  repo: 'WorktreeX',
  runId: '10',
  attempt: '1',
  event: 'push',
  ref: 'main',
  testSha: '0123456789abcdef0123456789abcdef01234567',
  runUrl: 'https://github.com/HappyOnigiri/WorktreeX/actions/runs/10',
  commitUrl: 'https://github.com/HappyOnigiri/WorktreeX/commit/0123456789abcdef0123456789abcdef01234567',
};

test('groups coverage and race evidence under one issue key', () => {
  const groups = reporter.aggregateManifests([
    { artifactName: 'coverage', manifest: manifest('10', '1', 'coverage') },
    { artifactName: 'race', manifest: manifest('10', '1', 'race-daemon') },
    { artifactName: 'race-state', manifest: manifest('10', '1', 'race-state') },
  ], source);
  assert.equal(groups.length, 1);
  assert.equal(groups[0].items.length, 3);
  assert.match(reporter.buildIssueBody(groups[0], source), /race-daemon/);
  assert.match(reporter.buildIssueBody(groups[0], source), /race-state/);
});

test('associates a race-state report with its job URL', async () => {
  const github = { rest: {
    actions: {
      getWorkflowRun: async () => ({ data: { name: 'CI', path: '.github/workflows/ci.yml', run_attempt: 1, event: 'push', head_branch: 'main', head_sha: source.testSha, html_url: source.runUrl } }),
      listJobsForWorkflowRun: async () => ({ data: { jobs: [{
        name: 'race (state)',
        html_url: 'https://github.com/HappyOnigiri/WorktreeX/actions/runs/10/job/state',
        run_attempt: 1,
        conclusion: 'success',
        steps: [{ name: 'Upload CI test report', conclusion: 'success' }],
      }] } }),
      listWorkflowRunArtifacts: async () => ({ data: { artifacts: [{ id: 1, name: 'ci-tests-race-state-10-1', expired: false, workflow_run: { id: 10 } }] } }),
    },
    issues: {
      listForRepo: async () => ({ data: [] }),
      create: async () => ({ data: {} }),
    },
  } };
  const result = await reporter.run({
    github,
    owner: source.owner,
    repo: source.repo,
    sourceRunId: source.runId,
    sourceAttempt: source.attempt,
    reports: [{ artifactName: 'ci-tests-race-state-10-1', manifest: manifest('10', '1', 'race-state') }],
  });
  assert.equal(result.groups[0].items[0].jobUrl, 'https://github.com/HappyOnigiri/WorktreeX/actions/runs/10/job/state');
});

test('rejects a race-state artifact whose manifest names another profile', () => {
  assert.throws(() => reporter.aggregateManifests([
    { artifactName: 'ci-tests-race-state-10-1', manifest: manifest('10', '1', 'race-rest') },
  ], source), /profile does not match its artifact name/);
});

test('collects every weighted race shard as a separate profile', async () => {
  const shards = ['race-daemon-0', 'race-daemon-1', 'race-daemon-2', 'race-rest-0', 'race-rest-1'];
  const jobs = shards.map((profile) => ({
    name: `race (${profile.replace(/^race-/, '')})`,
    html_url: `https://github.com/HappyOnigiri/WorktreeX/actions/runs/10/job/${profile}`,
    run_attempt: 1,
    conclusion: 'success',
    steps: [{ name: 'Upload CI test report', conclusion: 'success' }],
  }));
  const artifacts = shards.map((profile, id) => ({
    id: id + 1,
    name: `ci-tests-${profile}-10-1`,
    expired: false,
    workflow_run: { id: 10 },
  }));
  const github = { rest: {
    actions: {
      getWorkflowRun: async () => ({ data: { name: 'CI', path: '.github/workflows/ci.yml', run_attempt: 1, event: 'push', head_branch: 'main', head_sha: source.testSha, html_url: source.runUrl } }),
      listJobsForWorkflowRun: async () => ({ data: { jobs } }),
      listWorkflowRunArtifacts: async () => ({ data: { artifacts } }),
    },
    issues: {
      listForRepo: async () => ({ data: [] }),
      create: async () => ({ data: {} }),
    },
  } };
  const reports = shards.map((profile) => ({
    artifactName: `ci-tests-${profile}-10-1`,
    manifest: manifest('10', '1', profile),
  }));
  const result = await reporter.run({
    github,
    owner: source.owner,
    repo: source.repo,
    sourceRunId: source.runId,
    sourceAttempt: source.attempt,
    reports,
  });
  assert.equal(result.groups.length, 1);
  assert.deepEqual(result.groups[0].items.map((item) => item.manifest.profile), shards);
  assert.deepEqual(result.groups[0].items.map((item) => item.jobUrl), jobs.map((job) => job.html_url));
});

test('reports a missing weighted race shard independently', async () => {
  const shards = ['race-daemon-0', 'race-daemon-1', 'race-daemon-2', 'race-rest-0', 'race-rest-1'];
  const jobs = shards.map((profile) => ({
    name: `race (${profile.replace(/^race-/, '')})`,
    run_attempt: 1,
    conclusion: 'success',
    steps: [{ name: 'Upload CI test report', conclusion: 'success' }],
  }));
  const present = shards.slice(0, 3);
  const artifacts = present.map((profile, id) => ({
    id: id + 1,
    name: `ci-tests-${profile}-10-1`,
    expired: false,
    workflow_run: { id: 10 },
  }));
  const warnings = [];
  const github = { rest: {
    actions: {
      getWorkflowRun: async () => ({ data: { name: 'CI', path: '.github/workflows/ci.yml', run_attempt: 1, event: 'push', head_branch: 'main', head_sha: source.testSha, html_url: source.runUrl } }),
      listJobsForWorkflowRun: async () => ({ data: { jobs } }),
      listWorkflowRunArtifacts: async () => ({ data: { artifacts } }),
    },
    issues: {
      listForRepo: async () => ({ data: [] }),
      create: async () => ({ data: {} }),
    },
  } };
  const reports = present.map((profile) => ({
    artifactName: `ci-tests-${profile}-10-1`,
    manifest: manifest('10', '1', profile),
  }));
  await assert.rejects(reporter.run({
    github,
    owner: source.owner,
    repo: source.repo,
    sourceRunId: source.runId,
    sourceAttempt: source.attempt,
    reports,
    core: { warning: (message) => warnings.push(message) },
  }), /missing report artifact for race-rest-0, race-rest-1/);
  assert.deepEqual(warnings, ['missing report artifact for race-rest-0', 'missing report artifact for race-rest-1']);
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
  const source2 = { ...source, runId: '11', runUrl: 'https://github.com/HappyOnigiri/WorktreeX/actions/runs/11' };
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
  const sourceRun = (id) => ({ name: 'CI', path: '.github/workflows/ci.yml', run_attempt: 1, event: 'push', head_branch: 'main', head_sha: '0123456789abcdef0123456789abcdef01234567', html_url: `https://github.com/HappyOnigiri/WorktreeX/actions/runs/${id}` });
  const github = { rest: {
    actions: {
      getWorkflowRun: async ({ run_id: id }) => { currentRun = String(id); return { data: sourceRun(id) }; },
      listJobsForWorkflowRun: async () => ({ data: { jobs: [{ name: 'coverage-tests', run_attempt: 1, conclusion: 'failure', steps: [{ name: 'Upload CI test report', conclusion: 'success' }] }] } }),
      listWorkflowRunArtifacts: async () => ({ data: { artifacts: [
        { id: 1, name: `ci-tests-coverage-${currentRun}-1`, expired: false, workflow_run: { id: Number(currentRun) } },
        // Flake Hunt用のマーカーが同じrunにあっても、CIの起票契約には影響しない。
        { id: 2, name: `flake-hunt-no-issues-${currentRun}-1`, expired: false, workflow_run: { id: Number(currentRun) } },
      ] } }),
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
  const first = await reporter.run({ github, owner: 'HappyOnigiri', repo: 'WorktreeX', sourceRunId: '10', sourceAttempt: '1', reports });
  assert.equal(first.results[0].action, 'created');
  const second = await reporter.run({ github, owner: 'HappyOnigiri', repo: 'WorktreeX', sourceRunId: '11', sourceAttempt: '1', reports: [{ artifactName: 'ci-tests-coverage-11-1', manifest: manifest('11', '1') }] });
  assert.equal(second.results[0].action, 'commented');
});

test('files the recoveries it has before failing on a missing artifact', async () => {
  const issues = [];
  const warnings = [];
  const github = { rest: {
    actions: {
      getWorkflowRun: async () => ({ data: { name: 'CI', path: '.github/workflows/ci.yml', run_attempt: 1, event: 'push', head_branch: 'main', head_sha: '0123456789abcdef0123456789abcdef01234567', html_url: 'https://github.com/HappyOnigiri/WorktreeX/actions/runs/10' } }),
      // coverage-testsはBuild CI test runnerで落ち、if-no-files-foundの警告だけでuploadが成功した状態。
      listJobsForWorkflowRun: async () => ({ data: { jobs: [
        { name: 'coverage-tests', run_attempt: 1, conclusion: 'failure', steps: [{ name: 'Upload CI test report', conclusion: 'success' }] },
        { name: 'race (daemon)', run_attempt: 1, conclusion: 'success', steps: [{ name: 'Upload CI test report', conclusion: 'success' }] },
        { name: 'race (state)', run_attempt: 1, conclusion: 'success', steps: [{ name: 'Upload CI test report', conclusion: 'success' }] },
      ] } }),
      listWorkflowRunArtifacts: async () => ({ data: { artifacts: [
        { id: 1, name: 'ci-tests-race-daemon-10-1', expired: false, workflow_run: { id: 10 } },
        { id: 2, name: 'ci-tests-race-state-10-1', expired: false, workflow_run: { id: 10 } },
      ] } }),
    },
    issues: {
      listForRepo: async () => ({ data: issues }),
      listComments: async () => ({ data: [] }),
      create: async (request) => { const issue = { number: issues.length + 1, title: request.title, body: request.body, state: 'open' }; issues.push(issue); return { data: issue }; },
      createComment: async () => ({ data: {} }),
      update: async () => ({ data: {} }),
    },
  } };
  const reports = [{ artifactName: 'ci-tests-race-daemon-10-1', manifest: manifest('10', '1', 'race-daemon') }];
  await assert.rejects(reporter.run({ github, owner: 'HappyOnigiri', repo: 'WorktreeX', sourceRunId: '10', sourceAttempt: '1', reports, core: { warning: (message) => warnings.push(message) } }), /missing report artifact for coverage/);
  assert.equal(issues.length, 1);
  assert.deepEqual(warnings, ['missing report artifact for coverage']);
});

test('rejects a path escape in an artifact manifest', () => {
  const value = manifest('10', '1');
  value.recoveries[0].declaration.path = '../outside.go';
  assert.throws(() => reporter.validateManifest(value), /repository-relative path/);
});

test('keeps artifact content from breaking out of markdown', () => {
  const value = manifest('10', '1');
  value.recoveries[0].package = 'evil`![](https://attacker.example/pixel.png)';
  value.initial.status = '![](https://attacker.example/prose.png)';
  const body = reporter.buildIssueBody(reporter.aggregateManifests([{ artifactName: 'coverage', manifest: value }], source)[0], source);
  // code span内の画像記法は解釈されないので、spanが閉じないことだけを確かめる。
  assert.ok(!body.includes('evil`'), body);
  assert.match(body, /^- package: `evil｀!\[\]\(https:\/\/attacker\.example\/pixel\.png\)`$/mu);
  // 地の文では記法そのものを無効化する。
  assert.ok(body.includes('- result: initial \\!\\[\\](https://attacker.example/prose.png); retry unknown'), body);
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

function huntManifest(huntId, overrides = {}) {
  return {
    schema_version: 1,
    kind: 'flake-hunt',
    hunt_id: huntId,
    command: ['go', 'test', '-json', '-race', '-shuffle=on', '-count=10', './...'],
    count: '10',
    rounds: 4,
    failed_rounds: 1,
    anomaly_rounds: 0,
    run_id: '10',
    run_attempt: '1',
    test_sha: '0123456789abcdef0123456789abcdef01234567',
    round_records: [
      { round: 1, status: 'passed', shuffle: '111' },
      { round: 2, status: 'failed', shuffle: '222' },
    ],
    tests: [{
      package: 'example.test',
      declaration: { path: 'internal/example/flaky_test.go', function: 'TestFlaky', line: 12 },
      subtests: ['TestFlaky', 'TestFlaky/sub test'],
      pass_count: 39,
      fail_count: 1,
      skip_count: 0,
      log_excerpt: '    flaky_test.go:12: boom',
    }],
    ...overrides,
  };
}

const huntSource = { ...source, kind: 'hunt', runUrl: 'https://github.com/HappyOnigiri/WorktreeX/actions/runs/10' };

function huntGithub({ jobs = [], artifacts = [], issues = [], comments = new Map(), calls = [] } = {}) {
  return { rest: {
    actions: {
      getWorkflowRun: async () => ({ data: { name: 'Flake Hunt', path: '.github/workflows/flake-hunt.yml', run_attempt: 1, event: 'workflow_dispatch', head_branch: 'main', head_sha: source.testSha, html_url: huntSource.runUrl } }),
      listJobsForWorkflowRun: async () => ({ data: { jobs } }),
      listWorkflowRunArtifacts: async () => ({ data: { artifacts } }),
    },
    issues: {
      listForRepo: async () => ({ data: issues }),
      listComments: async ({ issue_number: number }) => ({ data: comments.get(number) || [] }),
      create: async (request) => { const issue = { number: issues.length + 1, title: request.title, body: request.body, state: 'open' }; issues.push(issue); calls.push(['create', request.title]); return { data: issue }; },
      createComment: async ({ issue_number: number, body }) => { const list = comments.get(number) || []; list.push({ body }); comments.set(number, list); calls.push(['comment', number]); return { data: {} }; },
      update: async ({ state }) => { calls.push(['update', state]); return { data: {} }; },
    },
  } };
}

test('files a hunt observation under the same issue title as CI', () => {
  const groups = reporter.aggregateHuntManifests([
    { artifactName: 'flake-hunt-hunt-1-10-1', huntId: 'hunt-1', manifest: huntManifest('hunt-1') },
    { artifactName: 'flake-hunt-hunt-2-10-1', huntId: 'hunt-2', manifest: huntManifest('hunt-2') },
  ], huntSource);
  assert.equal(groups.length, 1);
  const ciGroups = reporter.aggregateManifests([{ artifactName: 'coverage', manifest: manifest('10', '1') }], source);
  assert.equal(groups[0].title, ciGroups[0].title);
  assert.equal(groups[0].title, '[flaky] internal/example/flaky_test.go: TestFlaky');
  // markerはpath・functionとrunから決まるので、同じrunのCIとhuntで同じ値になる。
  assert.equal(groups[0].marker, ciGroups[0].marker);
  const body = reporter.buildIssueBody(groups[0], huntSource);
  assert.match(body, /Flake Hunt repeated this test profile/);
  assert.match(body, /78 pass, 2 fail, 0 skip across 2 container\(s\)/);
  assert.match(body, /containers: 2; rounds: 8 \(failed 2, anomalous 0\)/);
  assert.match(body, /hunt-1#2=222/);
  assert.match(body, /boom/);
});

test('reports only tests that both passed and failed across rounds', () => {
  const deterministic = huntManifest('hunt-1');
  deterministic.tests[0].pass_count = 0;
  deterministic.tests[0].fail_count = 4;
  assert.equal(reporter.aggregateHuntManifests([{ artifactName: 'flake-hunt-hunt-1-10-1', manifest: deterministic }], huntSource).length, 0);
  const neverFailed = huntManifest('hunt-1');
  neverFailed.tests[0].fail_count = 0;
  assert.equal(reporter.aggregateHuntManifests([{ artifactName: 'flake-hunt-hunt-1-10-1', manifest: neverFailed }], huntSource).length, 0);
});

// コンテナ横断で合算してから判定するので、片方だけを見れば決定的に見える失敗も救える。
test('combines containers before deciding whether a test is flaky', () => {
  const failing = huntManifest('hunt-1');
  failing.tests[0].pass_count = 0;
  failing.tests[0].fail_count = 2;
  const passing = huntManifest('hunt-2');
  passing.tests[0].fail_count = 0;
  passing.tests[0].pass_count = 40;
  const groups = reporter.aggregateHuntManifests([
    { artifactName: 'flake-hunt-hunt-1-10-1', manifest: failing },
    { artifactName: 'flake-hunt-hunt-2-10-1', manifest: passing },
  ], huntSource);
  assert.equal(groups.length, 1);
  assert.equal(groups[0].pass, 40);
  assert.equal(groups[0].fail, 2);
});

test('does not file an issue from a hunt that only ran one round', () => {
  const single = huntManifest('hunt-1', { rounds: 1, failed_rounds: 1 });
  assert.equal(reporter.aggregateHuntManifests([{ artifactName: 'flake-hunt-hunt-1-10-1', manifest: single }], huntSource).length, 0);
});

test('rejects a hunt manifest whose hunt_id does not match its artifact name', () => {
  assert.throws(() => reporter.aggregateHuntManifests([
    { artifactName: 'flake-hunt-hunt-1-10-1', huntId: 'hunt-1', manifest: huntManifest('hunt-2') },
  ], huntSource), /invalid flake hunt report: .*hunt_id does not match/);
});

test('rejects a path escape in a hunt manifest', () => {
  const value = huntManifest('hunt-1');
  value.tests[0].declaration.path = '../outside.go';
  assert.throws(() => reporter.validateHuntManifest(value), /invalid flake hunt report: .*repository-relative path/);
});

test('rejects a hunt manifest with unbounded counts', () => {
  const value = huntManifest('hunt-1');
  value.tests[0].pass_count = -1;
  assert.throws(() => reporter.validateHuntManifest(value), /is not a bounded count/);
  assert.throws(() => reporter.validateHuntManifest(huntManifest('hunt-1', { kind: 'ci' })), /is not a flake hunt report/);
});

test('warns instead of failing when a hunt container reports nothing', async () => {
  const warnings = [];
  const github = huntGithub({
    jobs: [
      { name: 'hunt-1', run_attempt: 1, conclusion: 'failure', steps: [{ name: 'Upload flake hunt report', conclusion: 'success' }] },
      { name: 'hunt-2', run_attempt: 1, conclusion: 'failure', steps: [{ name: 'Upload flake hunt report', conclusion: 'success' }] },
    ],
    artifacts: [{ id: 1, name: 'flake-hunt-hunt-1-10-1', expired: false, workflow_run: { id: 10 } }],
  });
  const result = await reporter.run({
    github, owner: source.owner, repo: source.repo, sourceRunId: '10', sourceAttempt: '1',
    core: { warning: (message) => warnings.push(message) },
    reports: [{ artifactName: 'flake-hunt-hunt-1-10-1', huntId: 'hunt-1', manifest: huntManifest('hunt-1') }],
  });
  assert.deepEqual(warnings, ['missing flake hunt report for hunt-2']);
  assert.deepEqual(result.results.map((item) => item.action), ['created']);
});

test('does not file hunt issues when the source run has a no-issues marker', async () => {
  const calls = [];
  const github = huntGithub({
    artifacts: [{ id: 99, name: 'flake-hunt-no-issues-10-1', expired: false, workflow_run: { id: 10 } }],
    calls,
  });
  const result = await reporter.run({
    github, owner: source.owner, repo: source.repo, sourceRunId: '10', sourceAttempt: '1',
    reports: [{ artifactName: 'flake-hunt-hunt-1-10-1', huntId: 'hunt-1', manifest: huntManifest('hunt-1') }],
  });
  assert.equal(result.fileIssues, false);
  assert.deepEqual(result.results.map((item) => item.action), ['not-filed']);
  assert.deepEqual(result.notFiled, ['[flaky] internal/example/flaky_test.go: TestFlaky']);
  assert.deepEqual(calls, []);
});

test('skips a hunt manifest written by an unknown schema version', async () => {
  const warnings = [];
  const github = huntGithub({});
  const result = await reporter.run({
    github, owner: source.owner, repo: source.repo, sourceRunId: '10', sourceAttempt: '1',
    core: { warning: (message) => warnings.push(message) },
    reports: [{ artifactName: 'flake-hunt-hunt-1-10-1', huntId: 'hunt-1', manifest: huntManifest('hunt-1', { schema_version: 99 }) }],
  });
  assert.deepEqual(warnings, ['skipping flake-hunt-hunt-1-10-1: unsupported schema_version']);
  assert.equal(result.results.length, 0);
});

test('comments on the issue a CI run already opened for the same test', async () => {
  const calls = [];
  const issues = [{ number: 7, title: '[flaky] internal/example/flaky_test.go: TestFlaky', body: 'opened by CI', state: 'closed' }];
  const github = huntGithub({ issues, calls });
  const result = await reporter.run({
    github, owner: source.owner, repo: source.repo, sourceRunId: '10', sourceAttempt: '1',
    reports: [{ artifactName: 'flake-hunt-hunt-1-10-1', huntId: 'hunt-1', manifest: huntManifest('hunt-1') }],
  });
  assert.deepEqual(result.results.map((item) => item.action), ['reopened-commented']);
  assert.deepEqual(calls, [['update', 'open'], ['comment', 7]]);
});

test('records a hunt observation only once per run', async () => {
  const comments = new Map();
  const issues = [];
  const github = huntGithub({ issues, comments });
  const options = {
    github, owner: source.owner, repo: source.repo, sourceRunId: '10', sourceAttempt: '1',
    reports: [{ artifactName: 'flake-hunt-hunt-1-10-1', huntId: 'hunt-1', manifest: huntManifest('hunt-1') }],
  };
  assert.deepEqual((await reporter.run(options)).results.map((item) => item.action), ['created']);
  assert.deepEqual((await reporter.run(options)).results.map((item) => item.action), ['already-recorded']);
});

test('caps how many issues one run opens', async () => {
  const tests = [];
  for (let index = 0; index < 25; index += 1) {
    tests.push({
      package: 'example.test',
      declaration: { path: `internal/example/flaky${String(index).padStart(2, '0')}_test.go`, function: 'TestFlaky', line: 12 },
      subtests: ['TestFlaky'],
      pass_count: 3,
      fail_count: 1,
      skip_count: 0,
    });
  }
  const github = huntGithub({});
  const result = await reporter.run({
    github, owner: source.owner, repo: source.repo, sourceRunId: '10', sourceAttempt: '1',
    reports: [{ artifactName: 'flake-hunt-hunt-1-10-1', huntId: 'hunt-1', manifest: huntManifest('hunt-1', { tests }) }],
  });
  assert.equal(result.results.length, 20);
  assert.equal(result.skipped.length, 5);
});

test('rejects a workflow that is not in the contract table', async () => {
  const github = { rest: { actions: {
    getWorkflowRun: async () => ({ data: { name: 'Flake Hunt', path: '.github/workflows/nightly.yml' } }),
  } } };
  await assert.rejects(reporter.run({ github, owner: source.owner, repo: source.repo, sourceRunId: '10', sourceAttempt: '1' }), /not a supported workflow/);
});

test('keeps hunt artifact content from breaking out of markdown', () => {
  const value = huntManifest('hunt-1');
  value.tests[0].package = 'evil`![](https://attacker.example/pixel.png)';
  const body = reporter.buildIssueBody(reporter.aggregateHuntManifests([{ artifactName: 'flake-hunt-hunt-1-10-1', manifest: value }], huntSource)[0], huntSource);
  assert.ok(!body.includes('evil`'), body);
  assert.match(body, /^- package: `evil｀!\[\]\(https:\/\/attacker\.example\/pixel\.png\)`$/mu);
});
