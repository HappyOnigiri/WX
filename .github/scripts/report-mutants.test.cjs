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
    schema_version: 2,
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

test('uses the shared deterministic mutation ID vector', () => {
  assert.equal(manifest().survivors[0].id, '8f58df524fcb072e70af7462216f0880cf5bdde384c38b74bb7bbf2f4a2414d7');
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
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group, source }), 'already-recorded');
  issues[0].body = '';
  issues[0].state = 'closed';
  const laterSource = { ...source, runId: '11', runUrl: 'https://github.com/HappyOnigiri/WX/actions/runs/11' };
  const later = reporter.aggregateManifests([{ artifactName: 'mutation-config-11-1', manifest: manifest('11') }], laterSource)[0];
  assert.equal(await reporter.upsertGroup({ github, owner: source.owner, repo: source.repo, group: later, source: laterSource }), 'reopened-commented');
  assert.deepEqual(calls, [['create', 1], ['update', 'open'], ['comment', 1]]);
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
    core: { warning: (message) => warnings.push(message) },
  });
  assert.equal(result.survivorCount, 1);
  assert.equal(result.groups[0].items[0].observations[0].jobUrl, 'https://github.com/HappyOnigiri/WX/actions/runs/10/job/1');
  assert.deepEqual(warnings, ['mutation-config-10-1: 2 mutation(s) not covered']);
});

test('rejects unsafe paths and mismatched run metadata', () => {
  const value = manifest();
  value.survivors[0].declaration.path = '../outside.go';
  assert.throws(() => reporter.validateManifest(value), /repository-relative path/);
  assert.throws(() => reporter.aggregateManifests([{ artifactName: 'mutation-config-10-1', manifest: manifest('11') }], source), /run_id does not match/);
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
