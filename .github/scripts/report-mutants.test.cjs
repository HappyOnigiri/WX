'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const reporter = require('./report-mutants.cjs');

const sha = '0123456789abcdef0123456789abcdef01234567';
const source = {
  owner: 'HappyOnigiri',
  repo: 'WX',
  runId: '10',
  attempt: '1',
  event: 'workflow_dispatch',
  ref: 'main',
  apiHeadSha: sha,
  runUrl: 'https://github.com/HappyOnigiri/WX/actions/runs/10',
  commitUrl: `https://github.com/HappyOnigiri/WX/commit/${sha}`,
};

function manifest(runId = '10', attempt = '1', profile = 'internal/config') {
  const survivor = {
    declaration: { path: 'internal/config/duration.go', function: 'parseDuration', line: 42 },
    mutator: 'CONDITIONALS_BOUNDARY',
    line: 43,
    column: 12,
    original: '>=',
    mutated: '>',
  };
  survivor.id = reporter.mutationId(survivor);
  return {
    schema_version: 3,
    profile,
    run_id: runId,
    run_attempt: attempt,
    test_sha: sha,
    command: ['gremlins', 'unleash', `./${profile}`],
    totals: { mutants: 12, killed: 8, lived: 1, not_covered: 2, not_viable: 1, timed_out: 0 },
    survivors: [survivor],
    excluded: [],
  };
}

function emptyManifest(runId = '10', attempt = '1', profile = 'internal/config') {
  const value = manifest(runId, attempt, profile);
  value.totals = { mutants: 0, killed: 0, lived: 0, not_covered: 0, not_viable: 0, timed_out: 0 };
  value.survivors = [];
  return value;
}

function execution(status = 'completed', shard = 'config', profile = 'internal/config') {
  return {
    schema_version: 1, run_id: '10', run_attempt: '1', test_sha: sha,
    shard, profile, stage: 'measurement', status, exit_code: status === 'completed' ? 0 : 1,
    signal: null, duration_seconds: 1.25, detail: status === 'completed' ? '' : 'fixture failure',
  };
}

test('uses the shared deterministic mutation ID vector', () => {
  assert.equal(manifest().survivors[0].id, '8f58df524fcb072e70af7462216f0880cf5bdde384c38b74bb7bbf2f4a2414d7');
});

test('accepts package-scope declarations in schema 3 manifests', () => {
  const survivor = {
    declaration: { path: 'cmd/wx/clean_unmanaged.go', function: '<package>', line: 1 },
    mutator: 'ARITHMETIC_BASE',
    line: 33,
    column: 24,
    original: '+',
    mutated: '-',
  };
  survivor.id = reporter.mutationId(survivor);
  const value = manifest();
  value.survivors = [survivor];
  assert.equal(reporter.validateManifest(value).survivors[0].declaration.function, '<package>');
  const groups = reporter.aggregateManifests([{ artifactName: 'mutation-config-10-1', manifest: value }], source);
  assert.equal(groups[0].title, '[mutation] cmd/wx/clean_unmanaged.go: <package>');
});

test('accepts a bounded file-shard command with many exclusions', () => {
  const value = manifest();
  value.command = ['gremlins', 'unleash', './internal/daemon'];
  for (let index = 0; index < 120; index += 1) {
    value.command.push('--exclude-files', `^source_${index}\\.go$`);
  }
  assert.equal(reporter.validateManifest(value).command.length, 243);
});

test('rejects an unbounded manifest command', () => {
  const value = manifest();
  value.command = Array.from({ length: 1001 }, () => 'argument');
  assert.throws(() => reporter.validateManifest(value), /invalid command/u);
});

test('groups survivors and renders mutation evidence', () => {
  const groups = reporter.aggregateManifests([{ artifactName: 'mutation-config-10-1', manifest: manifest() }], source);
  assert.equal(groups.length, 1);
  assert.equal(groups[0].title, '[mutation] internal/config/duration.go: parseDuration');
  const body = reporter.buildIssueBody(groups[0], source);
  assert.match(body, /CONDITIONALS_BOUNDARY/);
  assert.match(body, /`&gt;=` → `&gt;`/);
  assert.match(body, /test commit/);
});

test('deduplicates one mutation across profiles while retaining observations', () => {
  const first = manifest('10', '1', 'internal/sessions');
  const second = manifest('10', '1', 'internal/sessions/scanner');
  second.survivors[0].declaration.path = first.survivors[0].declaration.path;
  second.survivors[0].id = reporter.mutationId(second.survivors[0]);
  assert.equal(second.survivors[0].id, first.survivors[0].id);
  const groups = reporter.aggregateManifests([
    { artifactName: 'mutation-sessions-10-1', manifest: first },
    { artifactName: 'mutation-scanner-10-1', manifest: second },
  ], source);
  assert.equal(groups.length, 1);
  assert.equal(groups[0].items.length, 1);
  assert.equal(groups[0].items[0].observations.length, 2);
  const body = reporter.buildIssueBody(groups[0], source);
  assert.equal(body.match(/mutation ID:/gu).length, 1);
  assert.equal(body.match(/`&gt;=` → `&gt;`/gu).length, 1);
  assert.equal(body.match(/profile:/gu).length, 2);
  const expectedObservations = [
    '  observations:',
    '  - profile: `internal/sessions`',
    '  - job: not recorded',
    `  - test commit: \`${sha}\``,
    '  - command: `gremlins unleash ./internal/sessions`',
    '  - totals: mutants 12; killed 8; lived 1; not covered 2; timed out 0; not viable 1',
    '  - profile: `internal/sessions/scanner`',
    '  - job: not recorded',
    `  - test commit: \`${sha}\``,
    '  - command: `gremlins unleash ./internal/sessions/scanner`',
    '  - totals: mutants 12; killed 8; lived 1; not covered 2; timed out 0; not viable 1',
  ].join('\n');
  assert.ok(body.includes(expectedObservations), body);
  assert.doesNotMatch(body, /,\s+- job:/u);
});

test('rejects conflicting details for a shared mutation ID', () => {
  const first = manifest();
  const second = manifest('10', '1', 'internal/config/other');
  second.survivors[0].id = first.survivors[0].id;
  second.survivors[0].mutated = '>=';
  assert.throws(() => reporter.aggregateManifests([
    { artifactName: 'one', manifest: first },
    { artifactName: 'two', manifest: second },
  ], source), /invalid mutation report: .*mutation ID/);
});

test('creates once, suppresses the same marker, and reopens closed issues', async () => {
  const issues = [];
  const comments = new Map();
  const calls = [];
  const github = { rest: { issues: {
    getLabel: async () => ({ data: { name: 'mutation' } }),
    addLabels: async ({ issue_number: number }) => ({ data: [{ name: 'mutation' }], issue_number: number }),
    listForRepo: async () => ({ data: issues }),
    listComments: async ({ issue_number: number }) => ({ data: comments.get(number) || [] }),
    create: async (request) => {
      const issue = { number: issues.length + 1, title: request.title, body: request.body, state: 'open', labels: [{ name: 'mutation' }] };
      issues.push(issue);
      calls.push(['create', issue.number]);
      return { data: issue };
    },
    createComment: async ({ issue_number: number, body }) => {
      const list = comments.get(number) || [];
      list.push({ body });
      comments.set(number, list);
      calls.push(['comment', number]);
      return { data: {} };
    },
    update: async ({ state }) => {
      issues[0].state = state;
      calls.push(['update', state]);
      return { data: issues[0] };
    },
  } } };
  const group = reporter.aggregateManifests([{ artifactName: 'mutation-config-10-1', manifest: manifest() }], source)[0];
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group, source }), 'created');
  assert.deepEqual(issues[0].labels.map((label) => label.name), ['mutation']);
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group, source }), 'already-recorded');
  issues[0].body = '';
  issues[0].state = 'closed';
  const laterSource = { ...source, runId: '11', runUrl: 'https://github.com/HappyOnigiri/WX/actions/runs/11' };
  const later = reporter.aggregateManifests([{ artifactName: 'mutation-config-11-1', manifest: manifest('11') }], laterSource)[0];
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group: later, source: laterSource }), 'reopened-commented');
  assert.deepEqual(issues[0].labels.map((label) => label.name), ['mutation']);
  assert.deepEqual(calls, [['create', 1], ['update', 'open'], ['comment', 1]]);
});

test('creates the mutation label with defaults after a 404 lookup', async () => {
  const requests = [];
  const github = { rest: { issues: {
    getLabel: async () => { const error = new Error('missing'); error.status = 404; throw error; },
    createLabel: async (request) => { requests.push(request); return { data: { name: 'mutation' } }; },
  } } };
  await reporter.ensureMutationLabel({ github, owner: source.owner, repo: source.repo });
  assert.deepEqual(requests, [{
    owner: source.owner,
    repo: source.repo,
    name: 'mutation',
    description: 'Mutation Hunt automatic detection record',
    color: '1d76db',
  }]);
});

test('adds mutation to an existing issue without removing other labels', async () => {
  const issue = {
    number: 7,
    title: '[mutation] internal/config/duration.go: parseDuration',
    body: '',
    state: 'open',
    labels: [{ name: 'bug' }],
  };
  const added = [];
  let removed = 0;
  const github = { rest: { issues: {
    getLabel: async () => ({ data: { name: 'mutation' } }),
    listForRepo: async () => ({ data: [issue] }),
    listComments: async () => ({ data: [] }),
    addLabels: async ({ issue_number, labels }) => {
      added.push({ issue_number, labels });
      issue.labels.push(...labels.map((name) => ({ name })));
      return { data: issue.labels };
    },
    removeLabels: async () => { removed += 1; throw new Error('labels must not be removed'); },
    createComment: async () => ({ data: {} }),
    update: async () => ({ data: issue }),
  } } };
  const group = reporter.aggregateManifests([{ artifactName: 'mutation-config-10-1', manifest: manifest() }], source)[0];
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group, source }), 'commented');
  assert.deepEqual(added, [{ issue_number: 7, labels: ['mutation'] }]);
  assert.equal(removed, 0);
  assert.deepEqual(issue.labels.map((label) => label.name), ['bug', 'mutation']);
});

test('backfills only unlabeled mutation issues', async () => {
  const issues = [
    { number: 1, title: '[mutation] internal/config/duration.go: parseDuration', labels: [] },
    { number: 2, title: '[mutation] internal/config/other.go: parseOther', labels: [{ name: 'mutation' }] },
    { number: 3, title: '[bug] unrelated issue', labels: [] },
    { number: 4, title: '[mutation] internal/config/pr.go: parsePR', labels: [], pull_request: { url: 'pull' } },
  ];
  const added = [];
  const github = { rest: { issues: {
    getLabel: async () => ({ data: { name: 'mutation' } }),
    addLabels: async ({ issue_number, labels }) => {
      added.push({ issue_number, labels });
      const issue = issues.find((item) => item.number === issue_number);
      issue.labels.push(...labels.map((name) => ({ name })));
      return { data: issue.labels };
    },
  } } };
  assert.equal(await reporter.backfillMutationLabels({ github, owner: source.owner, repo: source.repo, issues }), 1);
  assert.deepEqual(added, [{ issue_number: 1, labels: ['mutation'] }]);
  assert.deepEqual(issues[1].labels.map((label) => label.name), ['mutation']);
  assert.deepEqual(issues[2].labels, []);
  assert.deepEqual(issues[3].labels, []);
});

test('propagates label lookup, creation, and attachment errors', async () => {
  const lookupError = new Error('lookup failed');
  await assert.rejects(
    reporter.ensureMutationLabel({
      github: { rest: { issues: { getLabel: async () => { throw lookupError; } } } },
      owner: source.owner,
      repo: source.repo,
    }),
    lookupError,
  );

  const createError = new Error('create failed');
  const missing = new Error('missing');
  missing.status = 404;
  await assert.rejects(
    reporter.ensureMutationLabel({
      github: { rest: { issues: {
        getLabel: async () => { throw missing; },
        createLabel: async () => { throw createError; },
      } } },
      owner: source.owner,
      repo: source.repo,
    }),
    createError,
  );

  const attachError = new Error('attach failed');
  const issue = {
    number: 8,
    title: '[mutation] internal/config/duration.go: parseDuration',
    body: '',
    state: 'open',
    labels: [],
  };
  const group = reporter.aggregateManifests([{ artifactName: 'mutation-config-10-1', manifest: manifest() }], source)[0];
  await assert.rejects(
    reporter.upsertGroup({
      github: { rest: { issues: {
        getLabel: async () => ({ data: { name: 'mutation' } }),
        listForRepo: async () => ({ data: [issue] }),
        addLabels: async () => { throw attachError; },
      } } },
      owner: source.owner,
      repo: source.repo,
      group,
      source,
    }),
    attachError,
  );
});

test('uses job URLs and warnings in the run orchestration', async () => {
  const issues = [];
  const warnings = [];
  const github = { rest: {
    actions: {
      listJobsForWorkflowRun: async () => ({ data: { jobs: [{ name: 'hunt (config)', run_attempt: 1, html_url: 'https://github.com/HappyOnigiri/WX/actions/runs/10/job/1' }] } }),
    },
    issues: {
      getLabel: async () => ({ data: { name: 'mutation' } }),
      addLabels: async () => ({ data: [{ name: 'mutation' }] }),
      listForRepo: async () => ({ data: issues }),
      create: async (request) => { const issue = { number: 1, title: request.title, body: request.body, state: 'open', labels: [{ name: 'mutation' }] }; issues.push(issue); return { data: issue }; },
      listComments: async () => ({ data: [] }),
      createComment: async () => ({ data: {} }),
      update: async () => ({ data: {} }),
    },
  } };
  const result = await reporter.run({
    github,
    owner: source.owner,
    repo: source.repo,
    sourceRunId: source.runId,
    sourceAttempt: source.attempt,
    sourceRun: { event: source.event, head_branch: source.ref, head_sha: sha, html_url: source.runUrl },
    reports: [{ artifactName: 'mutation-config-10-1', manifest: manifest() }],
    expectedShards: [{ id: 'config', profiles: ['internal/config'] }],
    core: { warning: (message) => warnings.push(message) },
  });
  assert.equal(result.survivorCount, 1);
  assert.equal(result.groups[0].items[0].observations[0].jobUrl, 'https://github.com/HappyOnigiri/WX/actions/runs/10/job/1');
  assert.deepEqual(warnings, ['mutation-config-10-1: 2 mutation(s) not covered']);
});

test('validates and aggregates without writing issues when filing is disabled', async () => {
  const calls = [];
  const github = { rest: {
    actions: { listJobsForWorkflowRun: async () => ({ data: { jobs: [] } }) },
    issues: {
      getLabel: async () => { calls.push('getLabel'); throw new Error('issue API must not be called'); },
      listForRepo: async () => { calls.push('listForRepo'); throw new Error('issue API must not be called'); },
      listComments: async () => { calls.push('listComments'); throw new Error('issue API must not be called'); },
      create: async () => { calls.push('create'); throw new Error('issue API must not be called'); },
      createComment: async () => { calls.push('createComment'); throw new Error('issue API must not be called'); },
      addLabels: async () => { calls.push('addLabels'); throw new Error('issue API must not be called'); },
    },
  } };
  const result = await reporter.run({
    github,
    owner: source.owner,
    repo: source.repo,
    sourceRunId: source.runId,
    sourceAttempt: source.attempt,
    sourceRun: { event: source.event, head_branch: source.ref, head_sha: sha, html_url: source.runUrl },
    reports: [{ artifactName: 'mutation-config-10-1', manifest: manifest() }],
    expectedShards: [{ id: 'config', profiles: ['internal/config'] }],
    fileIssues: false,
  });
  assert.equal(result.fileIssues, false);
  assert.equal(result.groups.length, 1);
  assert.equal(result.survivorCount, 1);
  assert.deepEqual(result.results, [{ title: '[mutation] internal/config/duration.go: parseDuration', action: 'not-filed' }]);
  assert.deepEqual(result.notFiled, ['[mutation] internal/config/duration.go: parseDuration']);
  assert.deepEqual(calls, []);
});

test('rejects unsafe paths and mismatched run metadata', () => {
  const value = manifest();
  value.survivors[0].declaration.path = '../outside.go';
  assert.throws(() => reporter.validateManifest(value), /repository-relative path/);
  assert.throws(() => reporter.aggregateManifests([{ artifactName: 'mutation-config-10-1', manifest: manifest('11') }], source), /run_id does not match/);
});

test('validates optional measured duration', () => {
  const value = manifest();
  value.duration_seconds = 1.5;
  assert.equal(reporter.validateManifest(value).duration_seconds, 1.5);
  for (const duration of [-1, NaN, Infinity, -Infinity, '1']) {
    const invalid = manifest();
    invalid.duration_seconds = duration;
    assert.throws(() => reporter.validateManifest(invalid), /duration_seconds/);
  }
});

test('sanitizes markdown metacharacters in artifact values', () => {
  const value = manifest();
  value.command = ['evil`![](https://attacker.example/pixel.png)'];
  value.survivors[0].original = '`@<';
  value.survivors[0].id = reporter.mutationId(value.survivors[0]);
  const group = reporter.aggregateManifests([{ artifactName: 'mutation-config-10-1', manifest: value }], source)[0];
  const body = reporter.buildIssueBody(group, source);
  assert.ok(!body.includes('evil`'), body);
  assert.match(body, /evil｀!\[\]\(https:\/\/attacker\.example\/pixel\.png\)/);
  assert.match(body, /`｀＠&lt;`/);
});

test('collects manifests from downloaded artifact directories', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wx-mutation-report-'));
  try {
    const directory = path.join(root, 'mutation-config-10-1');
    fs.mkdirSync(directory, { recursive: true });
    fs.writeFileSync(path.join(directory, 'manifest.json'), JSON.stringify(manifest()), { mode: 0o600 });
    const reports = reporter.collectManifests(root);
    assert.equal(reports.length, 1);
    assert.equal(reports[0].artifactName, 'mutation-config-10-1');
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('rejects incomplete, duplicate, and unexpected mutation shards', () => {
  const expected = [
    { id: 'config', profiles: ['internal/config'] },
    { id: 'state', profiles: ['internal/state'] },
  ];
  assert.throws(() => reporter.validateShardCompleteness([
    { artifactName: 'mutation-config-10-1', manifest: emptyManifest() },
  ], expected, source), /expected mutation shard state is missing/);
  assert.throws(() => reporter.validateShardCompleteness([
    { artifactName: 'mutation-config-10-1', manifest: emptyManifest() },
    { artifactName: 'mutation-config-10-1', manifest: emptyManifest() },
  ], [{ id: 'config', profiles: ['internal/config'] }], source), /duplicate profile/);
  assert.throws(() => reporter.validateShardCompleteness([
    { artifactName: 'mutation-other-10-1', manifest: emptyManifest() },
  ], [{ id: 'config', profiles: ['internal/config'] }], source), /not an expected mutation shard/);
  assert.throws(() => reporter.validateShardCompleteness([
    { artifactName: 'mutation-config-10-2', manifest: emptyManifest('10', '1') },
  ], [{ id: 'config', profiles: ['internal/config'] }], source), /expected run ID and attempt/);
});

test('accepts multiple file shards that report the same package profile', () => {
  const expected = [
    { id: 'daemon-1', profiles: ['internal/daemon'] },
    { id: 'daemon-2', profiles: ['internal/daemon'] },
  ];
  assert.doesNotThrow(() => reporter.validateShardCompleteness([
    { artifactName: 'mutation-daemon-1-10-1', manifest: emptyManifest('10', '1', 'internal/daemon') },
    { artifactName: 'mutation-daemon-2-10-1', manifest: emptyManifest('10', '1', 'internal/daemon') },
  ], expected, source));
});

test('still rejects a missing planned shard when filing is disabled', async () => {
  let writes = 0;
  const github = { rest: {
    actions: { listJobsForWorkflowRun: async () => ({ data: { jobs: [] } }) },
    issues: {
      getLabel: async () => { writes += 1; return { data: { name: 'mutation' } }; },
      create: async () => { writes += 1; return { data: { number: 1 } }; },
      addLabels: async () => { writes += 1; return { data: [] }; },
      update: async () => { writes += 1; return { data: {} }; },
      createComment: async () => { writes += 1; return { data: {} }; },
      listForRepo: async () => ({ data: [] }),
      listComments: async () => ({ data: [] }),
    },
  } };
  await assert.rejects(reporter.run({
    github,
    owner: source.owner,
    repo: source.repo,
    sourceRunId: source.runId,
    sourceAttempt: source.attempt,
    sourceRun: { event: source.event, head_branch: source.ref, head_sha: sha, html_url: source.runUrl },
    reports: [{ artifactName: 'mutation-config-10-1', manifest: emptyManifest() }],
    expectedShards: [
      { id: 'config', profiles: ['internal/config'] },
      { id: 'state', profiles: ['internal/state'] },
    ],
    fileIssues: false,
  }), /expected mutation shard state is missing/);
  assert.equal(writes, 0);
});

test('accepts all planned empty manifests without issue writes', async () => {
  let writes = 0;
  const github = { rest: {
    actions: { listJobsForWorkflowRun: async () => ({ data: { jobs: [] } }) },
    issues: {
      getLabel: async () => { writes += 1; return { data: { name: 'mutation' } }; },
      create: async () => { writes += 1; return { data: { number: 1 } }; },
      addLabels: async () => { writes += 1; return { data: [] }; },
      update: async () => { writes += 1; return { data: {} }; },
      createComment: async () => { writes += 1; return { data: {} }; },
      listForRepo: async () => ({ data: [] }),
      listComments: async () => ({ data: [] }),
    },
  } };
  const result = await reporter.run({
    github,
    owner: source.owner,
    repo: source.repo,
    sourceRunId: source.runId,
    sourceAttempt: source.attempt,
    sourceRun: { event: source.event, head_branch: source.ref, head_sha: sha, html_url: source.runUrl },
    reports: [
      { artifactName: 'mutation-config-10-1', manifest: emptyManifest('10', '1', 'internal/config') },
      { artifactName: 'mutation-state-10-1', manifest: emptyManifest('10', '1', 'internal/state') },
    ],
    expectedShards: [
      { id: 'config', profiles: ['internal/config'] },
      { id: 'state', profiles: ['internal/state'] },
    ],
  });
  assert.equal(result.survivorCount, 0);
  assert.deepEqual(result.results, []);
  assert.equal(writes, 0);
});

test('reports every unavailable shard and performs no issue writes', async () => {
  let writes = 0;
  const summaries = [];
  const summary = { addHeading() { return this; }, addRaw(value) { summaries.push(value); return this; }, async write() {} };
  const github = { rest: {
    actions: { listJobsForWorkflowRun: async () => ({ data: { jobs: [] } }) },
    issues: new Proxy({}, { get: () => async () => { writes += 1; } }),
  } };
  await assert.rejects(reporter.run({
    github,
    owner: source.owner,
    repo: source.repo,
    sourceRunId: source.runId,
    sourceAttempt: source.attempt,
    sourceRun: { event: source.event, head_branch: source.ref, head_sha: sha, html_url: source.runUrl },
    reports: [{
      artifactName: 'mutation-config-10-1',
      execution: execution('measurement_timed_out'),
    }],
    expectedShards: [
      { id: 'config', profiles: ['internal/config'] },
      { id: 'state', profiles: ['internal/state'] },
    ],
    core: { summary },
  }), (error) => {
    assert.match(error.message, /measurement_timed_out/u);
    assert.match(error.message, /expected mutation shard state is missing/u);
    return true;
  });
  assert.equal(writes, 0);
  assert.match(summaries.join('\n'), /mutation measurements are incomplete/u);
});

test('collects an execution result even when no manifest was produced', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wx-mutation-execution-'));
  try {
    const directory = path.join(root, 'mutation-config-10-1', 'internal-config');
    fs.mkdirSync(directory, { recursive: true });
    fs.writeFileSync(path.join(directory, 'execution.json'), JSON.stringify(execution('gremlins_failed')), { mode: 0o600 });
    const reports = reporter.collectManifests(root);
    assert.equal(reports.length, 1);
    assert.equal(reports[0].execution.status, 'gremlins_failed');
    assert.equal(reports[0].manifest, undefined);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
