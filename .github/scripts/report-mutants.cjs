'use strict';

const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');

const SHA = /^[0-9a-f]{40}$/iu;
const MUTATION_ID = /^[0-9a-f]{64}$/u;
const MUTATION_ID_VERSION = 'wx-mutation-id-v1';
const MUTATION_MANIFEST_SCHEMA_VERSION = 3;
const MAX_TEXT = 12000;
const TOTAL_KEYS = Object.freeze(['mutants', 'killed', 'lived', 'not_covered', 'not_viable', 'timed_out']);
const MUTATION_LABEL = 'mutation';
const MUTATION_LABEL_DESCRIPTION = 'Mutation Hunt automatic detection record';

function fail(message) {
  throw new Error(`invalid mutation report: ${message}`);
}

function text(value, name, limit = MAX_TEXT) {
  if (typeof value !== 'string' || value.length > limit || /[\u0000-\u0008\u000b\u000c\u000e-\u001f]/u.test(value)) {
    fail(`${name} is not a bounded text value`);
  }
  return value;
}

function relativePath(value, name) {
  text(value, name, 1000);
  const normalized = value.replaceAll('\\', '/');
  if (!normalized || normalized.startsWith('/') || normalized === '..' || normalized.includes('../') || normalized.includes('\u0000')) {
    fail(`${name} is not a repository-relative path`);
  }
  return normalized;
}

function nonNegativeInteger(value, name) {
  if (!Number.isInteger(value) || value < 0 || value > 100000000) fail(`${name} is not a bounded non-negative integer`);
  return value;
}

function validateDeclaration(value, name) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) fail(`${name} is not an object`);
  relativePath(value.path, `${name} path`);
  const functionName = text(value.function, `${name} function`, 500);
  if (!functionName || /[\r\n]/u.test(functionName)) fail(`${name} has an invalid function`);
  if (!Number.isInteger(value.line) || value.line < 1) fail(`${name} has an invalid line`);
  return value;
}

function mutationId(survivor) {
  const declaration = survivor.declaration;
  const canonical = [
    MUTATION_ID_VERSION,
    declaration.path,
    declaration.function,
    survivor.mutator,
    String(survivor.line),
    String(survivor.column),
    survivor.original,
    survivor.mutated,
  ].join('\n');
  return crypto.createHash('sha256').update(canonical, 'utf8').digest('hex');
}

function sameMutation(a, b) {
  return a.id === b.id &&
    a.declaration.path === b.declaration.path &&
    a.declaration.function === b.declaration.function &&
    a.mutator === b.mutator &&
    a.line === b.line && a.column === b.column &&
    a.original === b.original && a.mutated === b.mutated;
}

function validateManifest(value, artifactName = 'artifact') {
  if (!value || typeof value !== 'object' || Array.isArray(value)) fail(`${artifactName} is not an object`);
  if (value.schema_version !== MUTATION_MANIFEST_SCHEMA_VERSION) fail(`${artifactName} has unsupported schema_version`);
  relativePath(value.profile, `${artifactName} profile`);
  if (value.run_id !== undefined && !/^\d+$/u.test(String(value.run_id))) fail(`${artifactName} has invalid run_id`);
  if (value.run_attempt !== undefined && !/^\d+$/u.test(String(value.run_attempt))) fail(`${artifactName} has invalid run_attempt`);
  if (value.test_sha !== undefined && value.test_sha !== '' && !SHA.test(value.test_sha)) fail(`${artifactName} has invalid test_sha`);
  if (value.duration_seconds !== undefined &&
      (typeof value.duration_seconds !== 'number' || !Number.isFinite(value.duration_seconds) || value.duration_seconds < 0)) {
    fail(`${artifactName} has invalid duration_seconds`);
  }
  if (!Array.isArray(value.command) || value.command.length === 0 || value.command.length > 100) fail(`${artifactName} has invalid command`);
  value.command.forEach((item, index) => text(item, `${artifactName} command ${index}`, 2000));
  if (!value.totals || typeof value.totals !== 'object' || Array.isArray(value.totals)) fail(`${artifactName} has no totals`);
  for (const key of TOTAL_KEYS) nonNegativeInteger(value.totals[key], `${artifactName} totals.${key}`);
  if (!Array.isArray(value.survivors) || value.survivors.length > 10000) fail(`${artifactName} survivors is not an array`);
  for (const [index, item] of value.survivors.entries()) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) fail(`${artifactName} survivor ${index} is not an object`);
    validateDeclaration(item.declaration, `${artifactName} survivor ${index} declaration`);
    text(item.mutator, `${artifactName} survivor ${index} mutator`, 200);
    if (!Number.isInteger(item.line) || item.line < 1 || !Number.isInteger(item.column) || item.column < 1) {
      fail(`${artifactName} survivor ${index} has an invalid location`);
    }
    text(item.original, `${artifactName} survivor ${index} original`, 200);
    text(item.mutated, `${artifactName} survivor ${index} mutated`, 200);
    if (!MUTATION_ID.test(item.id) || mutationId(item) !== item.id) fail(`${artifactName} survivor ${index} has an invalid mutation ID`);
  }
  if (!Array.isArray(value.excluded) || value.excluded.length > 10000) fail(`${artifactName} excluded is not an array`);
  for (const [index, item] of value.excluded.entries()) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) fail(`${artifactName} excluded ${index} is not an object`);
    relativePath(item.path, `${artifactName} excluded ${index} path`);
    const functionName = text(item.function, `${artifactName} excluded ${index} function`, 500);
    if (!functionName || /[\r\n]/u.test(functionName)) fail(`${artifactName} excluded ${index} has an invalid function`);
    if (!MUTATION_ID.test(item.id)) fail(`${artifactName} excluded ${index} has an invalid mutation ID`);
    text(item.reason, `${artifactName} excluded ${index} reason`, 2000);
    const hasDetails = ['mutator', 'line', 'column', 'original', 'mutated', 'status'].some((key) => item[key] !== undefined);
    if (hasDetails) {
      text(item.mutator, `${artifactName} excluded ${index} mutator`, 200);
      if (!Number.isInteger(item.line) || item.line < 1 || !Number.isInteger(item.column) || item.column < 1) {
        fail(`${artifactName} excluded ${index} has an invalid location`);
      }
      text(item.original, `${artifactName} excluded ${index} original`, 200);
      text(item.mutated, `${artifactName} excluded ${index} mutated`, 200);
      text(item.status, `${artifactName} excluded ${index} status`, 100);
      const details = {
        id: item.id,
        declaration: { path: item.path, function: item.function },
        mutator: item.mutator,
        line: item.line,
        column: item.column,
        original: item.original,
        mutated: item.mutated,
      };
      if (mutationId(details) !== item.id) fail(`${artifactName} excluded ${index} has inconsistent mutation details`);
    }
  }
  return value;
}

function marker({ owner, repo, runId, attempt, declaration }) {
  const raw = [owner, repo, String(runId), String(attempt), declaration.path, declaration.function].join('\n');
  return `<!-- wx-mutation: ${crypto.createHash('sha256').update(raw).digest('hex')} -->`;
}

function sanitize(value, limit = 4000) {
  return String(value ?? '').replace(/[\u0000-\u001f]/gu, ' ').replaceAll('`', '｀').replaceAll('@', '＠').replaceAll('<', '&lt;').replaceAll('>', '&gt;').slice(0, limit);
}

function sanitizeText(value, limit = 4000) {
  return sanitize(value, limit).replace(/[[\]!]/gu, '\\$&');
}

function issueTitle(declaration) {
  return `[mutation] ${declaration.path}: ${declaration.function}`;
}

function jobLink(url) {
  return url ? sanitizeText(url, 2000) : 'not recorded';
}

function observationExcerpt(manifest, survivor, jobUrl = '') {
  const totals = manifest.totals;
  return [
    `- profile: \`${sanitize(manifest.profile, 500)}\``,
    `- job: ${jobLink(jobUrl)}`,
    `- test commit: \`${sanitize(manifest.test_sha || 'unknown', 100)}\``,
    `- command: \`${sanitize(manifest.command.join(' '), 2000)}\``,
    `- totals: mutants ${totals.mutants}; killed ${totals.killed}; lived ${totals.lived}; not covered ${totals.not_covered}; timed out ${totals.timed_out}; not viable ${totals.not_viable}`,
  ].join('\n');
}

function mutationExcerpt(item) {
  const survivor = item.survivor;
  return [
    `- mutation ID: \`${sanitize(item.id, 100)}\``,
    `- mutator: \`${sanitize(survivor.mutator, 200)}\``,
    `- mutation: \`${sanitize(survivor.original, 100)}\` → \`${sanitize(survivor.mutated, 100)}\``,
    `- location: ${sanitizeText(`${survivor.line}:${survivor.column}`, 100)}`,
    '',
    '  observations:',
    ...item.observations.flatMap((observation) => observationExcerpt(observation.manifest, observation.survivor, observation.jobUrl)
      .split('\n').map((line) => `  ${line}`)),
  ].join('\n');
}

function buildIssueBody(group, source) {
  const declaration = group.items[0].survivor.declaration;
  const lines = [
    group.marker,
    `## ${sanitizeText(issueTitle(declaration), 1000)}`,
    '',
    `Source workflow: [Mutation Hunt run ${source.runId}](${source.runUrl}) (attempt ${source.attempt})`,
    `Event: \`${sanitize(source.event, 200)}\`; ref: \`${sanitize(source.ref, 500)}\``,
    `API head SHA: [${sanitize(source.apiHeadSha || 'unknown', 100)}](${source.commitUrl || '#'})`,
    source.prUrl ? `PR: ${sanitizeText(source.prUrl, 2000)}` : 'PR: none',
    '',
    'The mutation survived the selected test suite:',
  ];
  for (const item of group.items) lines.push('', mutationExcerpt(item));
  return lines.join('\n').slice(0, 60000);
}

function buildIssueComment(group, source) {
  return [
    group.marker,
    `Mutation survivor observed in [Mutation Hunt run ${source.runId}](${source.runUrl}) (attempt ${source.attempt}).`,
    '',
    ...group.items.map((item) => mutationExcerpt(item)),
  ].join('\n').slice(0, 60000);
}

function artifactId(name) {
  const match = /^mutation-(.+)-\d+-\d+$/u.exec(String(name || ''));
  return match?.[1] || '';
}

function expectedProfiles(value, name) {
  let profiles = value;
  if (typeof profiles === 'string') profiles = profiles.trim().split(/\s+/u).filter(Boolean);
  if (!Array.isArray(profiles) || profiles.length === 0) fail(`${name} has no expected profiles`);
  const normalized = profiles.map((profile) => {
    if (typeof profile !== 'string') fail(`${name} has an invalid profile`);
    const value = profile.replaceAll('\\', '/').replace(/^\.\//u, '').replace(/\/+$/u, '');
    if (!value || value === '.') fail(`${name} has an invalid profile`);
    return value;
  });
  if (new Set(normalized).size !== normalized.length) fail(`${name} has duplicate profiles`);
  return normalized;
}

function normalizeExpectedShards(value) {
  let shards = value;
  if (typeof shards === 'string') {
    try {
      shards = JSON.parse(shards);
    } catch (error) {
      fail(`expected shards are not valid JSON: ${error.message}`);
    }
  }
  if (!Array.isArray(shards) || shards.length === 0) fail('expected shards are empty');
  return shards.map((item, index) => {
    const name = `expected shard ${index}`;
    if (typeof item === 'string') {
      text(item, `${name} ID`, 500);
      if (!item) fail(`${name} has an empty ID`);
      return { id: item, profiles: null };
    }
    if (!item || typeof item !== 'object' || Array.isArray(item)) fail(`${name} is not an object`);
    const id = text(item.id, `${name} ID`, 500);
    if (!id) fail(`${name} has an empty ID`);
    const profiles = expectedProfiles(item.profiles ?? item.profile ?? item.packages, name);
    return { id, profiles };
  });
}

function validateShardCompleteness(reports, expectedShards, source) {
  const expected = normalizeExpectedShards(expectedShards);
  const expectedById = new Map();
  for (const shard of expected) {
    if (expectedById.has(shard.id)) fail(`expected shard ${shard.id} is duplicated`);
    expectedById.set(shard.id, shard);
  }
  if (!Array.isArray(reports) || reports.length === 0) fail('no mutation manifests were downloaded');
  const observed = new Map();
  const suffix = `-${source.runId}-${source.attempt}`;
  for (const report of reports) {
    const artifactName = text(report?.artifactName, 'manifest artifact name', 1000);
    const prefix = 'mutation-';
    if (!artifactName.startsWith(prefix) || !artifactName.endsWith(suffix)) {
      fail(`${artifactName} does not contain the expected run ID and attempt`);
    }
    const id = artifactName.slice(prefix.length, artifactName.length - suffix.length);
    if (!id || !expectedById.has(id)) fail(`${artifactName} is not an expected mutation shard`);
    const manifest = validateManifest(report.manifest, artifactName);
    if (manifest.run_id === undefined || String(manifest.run_id) !== String(source.runId)) {
      fail(`${artifactName} run_id does not match source run`);
    }
    if (manifest.run_attempt === undefined || String(manifest.run_attempt) !== String(source.attempt)) {
      fail(`${artifactName} run_attempt does not match source attempt`);
    }
    const shard = expectedById.get(id);
    if (shard.profiles && !shard.profiles.includes(manifest.profile)) {
      fail(`${artifactName} contains unexpected profile ${manifest.profile}`);
    }
    let profiles = observed.get(id);
    if (!profiles) {
      profiles = new Set();
      observed.set(id, profiles);
    }
    if (profiles.has(manifest.profile)) fail(`${artifactName} contains a duplicate profile ${manifest.profile}`);
    profiles.add(manifest.profile);
  }
  for (const shard of expected) {
    const profiles = observed.get(shard.id);
    if (!profiles) fail(`expected mutation shard ${shard.id} is missing`);
    if (shard.profiles) {
      const missing = shard.profiles.filter((profile) => !profiles.has(profile));
      const unexpected = [...profiles].filter((profile) => !shard.profiles.includes(profile));
      if (missing.length > 0 || unexpected.length > 0) {
        fail(`mutation shard ${shard.id} profiles are incomplete (missing: ${missing.join(', ') || 'none'}; unexpected: ${unexpected.join(', ') || 'none'})`);
      }
    }
  }
  return { expected, observed };
}

function aggregateManifests(manifests, source) {
  const groups = new Map();
  const mutations = new Map();
  for (const item of manifests) {
    const manifest = validateManifest(item.manifest, item.artifactName || 'artifact');
    if (manifest.run_id !== undefined && String(manifest.run_id) !== String(source.runId)) fail(`${item.artifactName} run_id does not match source run`);
    if (manifest.run_attempt !== undefined && String(manifest.run_attempt) !== String(source.attempt)) fail(`${item.artifactName} run_attempt does not match source attempt`);
    manifest.artifact_name = item.artifactName || '';
    for (const survivor of manifest.survivors) {
      const key = `${survivor.declaration.path}\n${survivor.declaration.function}`;
      let group = groups.get(key);
      if (!group) {
        group = {
          key,
          marker: marker({ ...source, declaration: survivor.declaration }),
          title: issueTitle(survivor.declaration),
          items: [],
        };
        groups.set(key, group);
      }
      let mutation = mutations.get(survivor.id);
      if (!mutation) {
        mutation = { id: survivor.id, survivor, observations: [] };
        mutations.set(survivor.id, mutation);
        group.items.push(mutation);
      } else if (!sameMutation(mutation.survivor, survivor)) {
        fail(`mutation ID ${survivor.id} has conflicting mutation details`);
      }
      mutation.observations.push({ manifest, survivor, jobUrl: item.jobUrl || '' });
    }
  }
  return [...groups.values()].sort((a, b) => a.title.localeCompare(b.title));
}

async function pages(fetchPage) {
  const all = [];
  for (let page = 1; page <= 100; page += 1) {
    const result = await fetchPage(page);
    const values = result.data?.items ?? result.data?.artifacts ?? result.data?.comments ?? result.data?.jobs ?? result.data ?? [];
    all.push(...values);
    if (values.length < 100) break;
  }
  return all;
}

async function listIssues(github, owner, repo) {
  return pages((page) => github.rest.issues.listForRepo({ owner, repo, state: 'all', per_page: 100, page })).then((issues) => issues.filter((item) => !item.pull_request));
}

async function listComments(github, owner, repo, issueNumber) {
  return pages((page) => github.rest.issues.listComments({ owner, repo, issue_number: issueNumber, per_page: 100, page }));
}

function isNotFound(error) {
  return error?.status === 404 || error?.response?.status === 404;
}

function labelNames(labels) {
  return (Array.isArray(labels) ? labels : []).map((label) => typeof label === 'string' ? label : label?.name).filter(Boolean);
}

async function ensureMutationLabel({ github, owner, repo }) {
  const api = github?.rest?.issues;
  if (!api) throw new Error('GitHub issues API is required for mutation labels');
  if (api.getLabel) {
    try {
      const result = await api.getLabel({ owner, repo, name: MUTATION_LABEL });
      if (result?.data?.name !== MUTATION_LABEL) fail('mutation label lookup returned an unexpected label');
      return;
    } catch (error) {
      if (!isNotFound(error)) throw error;
    }
  } else if (api.listLabelsForRepo) {
    const labels = await pages((page) => api.listLabelsForRepo({ owner, repo, per_page: 100, page }));
    if (labels.some((label) => label?.name === MUTATION_LABEL)) return;
  } else {
    throw new Error('GitHub issues label API is required for mutation labels');
  }
  if (!api.createLabel) throw new Error('GitHub issues createLabel API is required for mutation labels');
  const result = await api.createLabel({
    owner,
    repo,
    name: MUTATION_LABEL,
    description: MUTATION_LABEL_DESCRIPTION,
    color: '1d76db',
  });
  const created = result?.data;
  if (!created || created.name !== MUTATION_LABEL) fail('mutation label creation returned an unexpected label');
}

async function addMutationLabel({ github, owner, repo, issue }) {
  const api = github?.rest?.issues;
  if (!api?.addLabels) throw new Error('GitHub issues addLabels API is required for mutation labels');
  const result = await api.addLabels({ owner, repo, issue_number: issue.number, labels: [MUTATION_LABEL] });
  if (Array.isArray(result?.data) && !labelNames(result.data).includes(MUTATION_LABEL)) {
    throw new Error(`mutation label was not applied to issue #${issue.number}`);
  }
}

async function ensureIssueMutationLabel({ github, owner, repo, issue }) {
  if (labelNames(issue.labels).includes(MUTATION_LABEL)) return;
  await addMutationLabel({ github, owner, repo, issue });
}

async function backfillMutationLabels({ github, owner, repo, issues }) {
  await ensureMutationLabel({ github, owner, repo });
  const candidates = issues || await listIssues(github, owner, repo);
  let updated = 0;
  for (const issue of candidates) {
    if (issue.pull_request || typeof issue.title !== 'string' || !issue.title.startsWith('[mutation] ')) continue;
    if (!labelNames(issue.labels).includes(MUTATION_LABEL)) {
      await ensureIssueMutationLabel({ github, owner, repo, issue });
      updated += 1;
    }
  }
  return updated;
}

async function upsertGroup({ github, owner, repo, group, source, labelReady = false }) {
  if (!labelReady) await ensureMutationLabel({ github, owner, repo });
  const issues = await listIssues(github, owner, repo);
  const matching = issues.filter((item) => item.title === group.title).sort((a, b) => a.number - b.number);
  const issue = matching[0];
  if (matching.length > 1 && source.summary) source.summary(`duplicate issue titles for ${group.title}: ${matching.slice(1).map((item) => item.number).join(', ')}`);
  if (!issue) {
    const created = await github.rest.issues.create({ owner, repo, title: group.title, body: buildIssueBody(group, source), labels: [MUTATION_LABEL] });
    const createdIssue = created?.data;
    if (!createdIssue?.number) throw new Error('mutation issue creation returned no issue number');
    await addMutationLabel({ github, owner, repo, issue: createdIssue });
    return 'created';
  }
  await ensureIssueMutationLabel({ github, owner, repo, issue });
  const comments = await listComments(github, owner, repo, issue.number);
  const found = [issue.body || '', ...comments.map((item) => item.body || '')].some((body) => body.includes(group.marker));
  if (found) return 'already-recorded';
  const wasClosed = issue.state === 'closed';
  if (wasClosed) await github.rest.issues.update({ owner, repo, issue_number: issue.number, state: 'open' });
  await github.rest.issues.createComment({ owner, repo, issue_number: issue.number, body: buildIssueComment(group, source) });
  return wasClosed ? 'reopened-commented' : 'commented';
}

function collectManifests(reportDir) {
  const reports = [];
  if (!fs.existsSync(reportDir)) return reports;
  let visited = 0;
  function visit(directory, artifactName) {
    for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
      if (++visited > 10000) fail('report directory has too many entries');
      if (entry.isSymbolicLink()) continue;
      const target = path.join(directory, entry.name);
      if (entry.isDirectory()) {
        visit(target, artifactName || entry.name);
      } else if (entry.name === 'manifest.json') {
        const data = JSON.parse(fs.readFileSync(target, 'utf8'));
        reports.push({ artifactName: artifactName || path.basename(reportDir), manifest: data });
      }
    }
  }
  visit(reportDir, '');
  return reports;
}

async function run(options) {
  const github = options.github;
  const context = options.context || {};
  const owner = options.owner || context.repo?.owner;
  const repo = options.repo || context.repo?.repo;
  const runId = String(options.sourceRunId || context.runId || context.payload?.workflow_run?.id || '');
  const attempt = String(options.sourceAttempt || context.runAttempt || context.payload?.workflow_run?.run_attempt || '1');
  if (!owner || !repo || !/^\d+$/u.test(runId) || !/^\d+$/u.test(attempt)) throw new Error('source run and repository are required');
  let sourceRun = options.sourceRun || {};
  if (!options.sourceRun && github?.rest?.actions?.getWorkflowRun) {
    sourceRun = (await github.rest.actions.getWorkflowRun({ owner, repo, run_id: Number(runId) })).data || {};
    if (sourceRun.run_attempt && String(sourceRun.run_attempt) !== attempt) throw new Error('source run attempt does not match requested attempt');
  }
  if (sourceRun.name && sourceRun.name !== 'Mutation Hunt') throw new Error('source run is not the Mutation Hunt workflow');
  if (sourceRun.path && sourceRun.path !== '.github/workflows/mutation-hunt.yml') throw new Error('source run is not the mutation-hunt workflow');
  const source = {
    owner, repo, runId, attempt,
    event: sourceRun.event || context.eventName || '',
    ref: sourceRun.head_branch || sourceRun.head_sha || context.ref || '',
    apiHeadSha: sourceRun.head_sha || '',
    prUrl: sourceRun.pull_requests?.[0]?.html_url || '',
    runUrl: sourceRun.html_url || `${context.serverUrl || 'https://github.com'}/${owner}/${repo}/actions/runs/${runId}`,
    commitUrl: sourceRun.head_sha ? `https://github.com/${owner}/${repo}/commit/${sourceRun.head_sha}` : '',
    summary: (message) => options.core?.warning?.(message),
  };
  const jobUrls = new Map();
  if (github?.rest?.actions?.listJobsForWorkflowRun) {
    const jobs = await pages((page) => github.rest.actions.listJobsForWorkflowRun({ owner, repo, run_id: Number(runId), filter: 'all', per_page: 100, page }));
    for (const job of jobs) {
      if (job.run_attempt && String(job.run_attempt) !== attempt) continue;
      const name = String(job.name || '');
      const id = /^hunt \((.+)\)$/u.exec(name)?.[1] || name;
      if (job.html_url && id) jobUrls.set(id, job.html_url);
    }
  }
  const reports = options.reports || collectManifests(options.reportDir || 'artifacts/mutation');
  validateShardCompleteness(reports, options.expectedShards, source);
  for (const report of reports) report.jobUrl = report.jobUrl || jobUrls.get(artifactId(report.artifactName)) || source.runUrl;
  const groups = aggregateManifests(reports, source);
  for (const report of reports) {
    const value = report.manifest;
    if (value.totals.timed_out > 0 && source.summary) source.summary(`${report.artifactName}: ${value.totals.timed_out} mutation(s) timed out`);
    if (value.totals.not_covered > 0 && source.summary) source.summary(`${report.artifactName}: ${value.totals.not_covered} mutation(s) not covered`);
  }
  if (groups.length > 0) await ensureMutationLabel({ github, owner, repo });
  const results = [];
  for (const group of groups) results.push({ title: group.title, action: await upsertGroup({ github, owner, repo, group, source, labelReady: groups.length > 0 }) });
  const survivorCount = groups.reduce((total, group) => total + group.items.length, 0);
  const summary = `mutation reports: ${reports.length} artifact(s), ${survivorCount} survivor(s), ${results.filter((item) => item.action === 'created').length} issue(s) created, ${results.filter((item) => item.action === 'commented' || item.action === 'reopened-commented').length} commented`;
  if (options.core?.summary) await options.core.summary.addHeading('Mutation hunt reports').addRaw(`${summary}\n`).write();
  return { source, reports, results, groups, survivorCount };
}

module.exports = {
  aggregateManifests,
  backfillMutationLabels,
  buildIssueBody,
  buildIssueComment,
  collectManifests,
  ensureMutationLabel,
  issueTitle,
  marker,
  mutationId,
  MUTATION_MANIFEST_SCHEMA_VERSION,
  run,
  upsertGroup,
  validateShardCompleteness,
  validateManifest,
};
