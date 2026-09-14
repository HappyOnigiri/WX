'use strict';

const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');

const SHA = /^[0-9a-f]{40}$/iu;
const MAX_TEXT = 12000;
const TOTAL_KEYS = Object.freeze(['mutants', 'killed', 'lived', 'not_covered', 'not_viable', 'timed_out']);

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

function validateManifest(value, artifactName = 'artifact') {
  if (!value || typeof value !== 'object' || Array.isArray(value)) fail(`${artifactName} is not an object`);
  if (value.schema_version !== 1) fail(`${artifactName} has unsupported schema_version`);
  relativePath(value.profile, `${artifactName} profile`);
  if (value.run_id !== undefined && !/^\d+$/u.test(String(value.run_id))) fail(`${artifactName} has invalid run_id`);
  if (value.run_attempt !== undefined && !/^\d+$/u.test(String(value.run_attempt))) fail(`${artifactName} has invalid run_attempt`);
  if (value.test_sha !== undefined && value.test_sha !== '' && !SHA.test(value.test_sha)) fail(`${artifactName} has invalid test_sha`);
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
  }
  if (!Array.isArray(value.excluded) || value.excluded.length > 10000) fail(`${artifactName} excluded is not an array`);
  for (const [index, item] of value.excluded.entries()) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) fail(`${artifactName} excluded ${index} is not an object`);
    relativePath(item.path, `${artifactName} excluded ${index} path`);
    text(item.function, `${artifactName} excluded ${index} function`, 500);
    text(item.reason, `${artifactName} excluded ${index} reason`, 2000);
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

function excerpt(manifest, survivor, jobUrl = '') {
  const totals = manifest.totals;
  return [
    `- profile: \`${sanitize(manifest.profile, 500)}\``,
    `- mutator: \`${sanitize(survivor.mutator, 200)}\``,
    `- mutation: \`${sanitize(survivor.original, 100)}\` → \`${sanitize(survivor.mutated, 100)}\``,
    `- location: ${sanitizeText(`${survivor.line}:${survivor.column}`, 100)}`,
    `- job: ${jobLink(jobUrl)}`,
    `- test commit: \`${sanitize(manifest.test_sha || 'unknown', 100)}\``,
    `- command: \`${sanitize(manifest.command.join(' '), 2000)}\``,
    `- totals: mutants ${totals.mutants}; killed ${totals.killed}; lived ${totals.lived}; not covered ${totals.not_covered}; timed out ${totals.timed_out}; not viable ${totals.not_viable}`,
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
  for (const item of group.items) {
    lines.push('', excerpt(item.manifest, item.survivor, item.jobUrl));
  }
  return lines.join('\n').slice(0, 60000);
}

function buildIssueComment(group, source) {
  return [
    group.marker,
    `Mutation survivor observed in [Mutation Hunt run ${source.runId}](${source.runUrl}) (attempt ${source.attempt}).`,
    '',
    ...group.items.map((item) => excerpt(item.manifest, item.survivor, item.jobUrl)),
  ].join('\n').slice(0, 60000);
}

function artifactId(name) {
  const match = /^mutation-(.+)-\d+-\d+$/u.exec(String(name || ''));
  return match?.[1] || '';
}

function aggregateManifests(manifests, source) {
  const groups = new Map();
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
      group.items.push({ manifest, survivor, jobUrl: item.jobUrl || '' });
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

async function upsertGroup({ github, owner, repo, group, source }) {
  const issues = await listIssues(github, owner, repo);
  const matching = issues.filter((item) => item.title === group.title).sort((a, b) => a.number - b.number);
  const issue = matching[0];
  if (matching.length > 1 && source.summary) source.summary(`duplicate issue titles for ${group.title}: ${matching.slice(1).map((item) => item.number).join(', ')}`);
  if (!issue) {
    await github.rest.issues.create({ owner, repo, title: group.title, body: buildIssueBody(group, source) });
    return 'created';
  }
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
  for (const report of reports) report.jobUrl = report.jobUrl || jobUrls.get(artifactId(report.artifactName)) || source.runUrl;
  const groups = aggregateManifests(reports, source);
  for (const report of reports) {
    const value = report.manifest;
    if (value.totals.timed_out > 0 && source.summary) source.summary(`${report.artifactName}: ${value.totals.timed_out} mutation(s) timed out`);
    if (value.totals.not_covered > 0 && source.summary) source.summary(`${report.artifactName}: ${value.totals.not_covered} mutation(s) not covered`);
  }
  const results = [];
  for (const group of groups) results.push({ title: group.title, action: await upsertGroup({ github, owner, repo, group, source }) });
  const survivorCount = groups.reduce((total, group) => total + group.items.length, 0);
  const summary = `mutation reports: ${reports.length} artifact(s), ${survivorCount} survivor(s), ${results.filter((item) => item.action === 'created').length} issue(s) created, ${results.filter((item) => item.action === 'commented' || item.action === 'reopened-commented').length} commented`;
  if (options.core?.summary) await options.core.summary.addHeading('Mutation hunt reports').addRaw(`${summary}\n`).write();
  return { source, reports, results, groups, survivorCount };
}

module.exports = {
  aggregateManifests,
  buildIssueBody,
  buildIssueComment,
  collectManifests,
  issueTitle,
  marker,
  run,
  upsertGroup,
  validateManifest,
};
