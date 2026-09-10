'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const childProcess = require('node:child_process');

const PROFILES = new Set(['coverage', 'race-daemon', 'race-rest']);
const SHA = /^[0-9a-f]{40}$/i;
const MAX_TEXT = 12000;

function fail(message) {
  throw new Error(`invalid CI test report: ${message}`);
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
  if (!normalized || normalized.startsWith('/') || normalized.includes('../') || normalized === '..' || normalized.includes('\u0000')) {
    fail(`${name} is not a repository-relative path`);
  }
  return normalized;
}

function validateManifest(value, artifactName = 'artifact') {
  if (!value || typeof value !== 'object' || Array.isArray(value)) fail(`${artifactName} is not an object`);
  if (value.schema_version !== 1) fail(`${artifactName} has unsupported schema_version`);
  if (!PROFILES.has(value.profile)) fail(`${artifactName} has unsupported profile`);
  if (value.test_sha && !SHA.test(value.test_sha)) fail(`${artifactName} has invalid test_sha`);
  if (value.api_head_sha && !SHA.test(value.api_head_sha)) fail(`${artifactName} has invalid api_head_sha`);
  if (value.run_id && !/^\d+$/.test(String(value.run_id))) fail(`${artifactName} has invalid run_id`);
  if (value.run_attempt && !/^\d+$/.test(String(value.run_attempt))) fail(`${artifactName} has invalid run_attempt`);
  if (value.recoveries === undefined) value.recoveries = [];
  if (!Array.isArray(value.recoveries)) fail(`${artifactName} recoveries is not an array`);
  if (value.recoveries.length > 1000) fail(`${artifactName} has too many recoveries`);
  for (const [index, item] of value.recoveries.entries()) {
    if (!item || typeof item !== 'object') fail(`${artifactName} recovery ${index} is not an object`);
    const declaration = item.declaration;
    if (!declaration || typeof declaration !== 'object') fail(`${artifactName} recovery ${index} has no declaration`);
    relativePath(declaration.path, `${artifactName} recovery ${index} path`);
    const fn = text(declaration.function, `${artifactName} recovery ${index} function`, 500);
    if (!/^(Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]+$/.test(fn)) fail(`${artifactName} recovery ${index} has invalid function`);
    text(item.package, `${artifactName} recovery ${index} package`, 500);
    if (!Array.isArray(item.failed_tests) || item.failed_tests.length === 0 || item.failed_tests.length > 100) {
      fail(`${artifactName} recovery ${index} has invalid failed_tests`);
    }
    for (const testName of item.failed_tests) text(testName, `${artifactName} recovery ${index} failed test`, 1000);
  }
  if (value.recovered !== true && value.recoveries.length !== 0) fail(`${artifactName} marks recoveries without recovered=true`);
  return value;
}

function marker({ owner, repo, runId, attempt, declaration }) {
  const raw = [owner, repo, String(runId), String(attempt), declaration.path, declaration.function].join('\n');
  return `<!-- wx-flaky: ${crypto.createHash('sha256').update(raw).digest('hex')} -->`;
}

function sanitize(value, limit = 4000) {
  return String(value ?? '').replace(/[\u0000-\u001f]/gu, ' ').replaceAll('`', '\\`').replaceAll('@', '＠').replaceAll('<', '&lt;').replaceAll('>', '&gt;').slice(0, limit);
}

function issueTitle(declaration) {
  return `[flaky] ${declaration.path}: ${declaration.function}`;
}

function excerpt(manifest, recovery, jobUrl = '') {
  const failed = recovery.failed_tests.join(', ');
  const initial = manifest.initial || {};
  const retry = (manifest.retries || [])[recovery.retry_index - 1] || {};
  return [
    `- profile: \`${manifest.profile}\``,
    jobUrl ? `- job: ${jobUrl}` : '- job: not recorded',
    `- package: \`${sanitize(recovery.package, 500)}\``,
    `- failed tests: \`${sanitize(failed, 1000)}\``,
    `- initial command: \`${sanitize((initial.command || []).join(' '), 2000)}\``,
    `- retry command: \`${sanitize((retry.command || []).join(' '), 2000)}\``,
    `- duration: initial ${sanitize(initial.duration_ms ?? 'unknown', 100)} ms; retry ${sanitize(retry.duration_ms ?? 'unknown', 100)} ms`,
    `- result: initial ${sanitize(initial.status || initial.exit || 'unknown', 100)}; retry ${sanitize(retry.status || retry.exit || 'unknown', 100)}`,
    `- shuffle seed: \`${sanitize(retry.shuffle || initial.shuffle || 'not recorded', 200)}\``,
    '',
    'Initial failure excerpt:',
    '```text',
    sanitize(initial.log_excerpt || 'not recorded', 4000),
    '```',
    '',
    'Retry output excerpt:',
    '```text',
    sanitize(retry.log_excerpt || 'not recorded', 4000),
    '```',
    `- evidence: artifact \`${sanitize(manifest.artifact_name || 'ci-test-report', 300)}\``,
    '',
    'The test passed on the one additional run. This records an observation, not a diagnosis of the test or product.',
  ].join('\n');
}

function buildIssueBody(group, source) {
  const first = group.items[0];
  const declaration = first.recovery.declaration;
  const lines = [
    group.marker,
    `## ${sanitize(issueTitle(declaration), 1000)}`,
    '',
    `Source workflow: [CI run ${source.runId}](${source.runUrl}) (attempt ${source.attempt})`,
    `Event: \`${sanitize(source.event, 200)}\`; ref: \`${sanitize(source.ref, 500)}\``,
    `API head SHA: [${source.apiHeadSha || 'unknown'}](${source.commitUrl || '#'})`,
    source.prUrl ? `PR: ${source.prUrl}` : 'PR: none',
    '',
    'The following CI test failure recovered on exactly one additional run:',
  ];
  for (const item of group.items) {
    lines.push('', `### ${sanitize(item.manifest.profile, 100)}`, `Test commit: \`${sanitize(item.manifest.test_sha || 'unknown', 100)}\``, excerpt(item.manifest, item.recovery, item.jobUrl));
  }
  return lines.join('\n').slice(0, 60000);
}

function buildIssueComment(group, source) {
  return [group.marker, `Reproduction observed in [CI run ${source.runId}](${source.runUrl}) (attempt ${source.attempt}).`, '', ...group.items.map((item) => `### ${sanitize(item.manifest.profile, 100)}\n${excerpt(item.manifest, item.recovery, item.jobUrl)}`)].join('\n').slice(0, 60000);
}

function aggregateManifests(manifests, source) {
  const groups = new Map();
  for (const item of manifests) {
    const manifest = validateManifest(item.manifest, item.artifactName);
    const profileMatch = /^ci-tests-(coverage|race-daemon|race-rest)-/.exec(item.artifactName || '');
    if (profileMatch && manifest.profile !== profileMatch[1]) fail(`${item.artifactName} profile does not match its artifact name`);
    if (manifest.run_id && String(manifest.run_id) !== String(source.runId)) fail(`${item.artifactName} run_id does not match source run`);
    if (manifest.run_attempt && String(manifest.run_attempt) !== String(source.attempt)) fail(`${item.artifactName} run_attempt does not match source attempt`);
    manifest.artifact_name = item.artifactName;
    for (const recovery of manifest.recoveries) {
      const key = `${recovery.declaration.path}\n${recovery.declaration.function}`;
      let group = groups.get(key);
      if (!group) {
        group = { key, marker: marker({ ...source, declaration: recovery.declaration }), title: issueTitle(recovery.declaration), items: [] };
        groups.set(key, group);
      }
      group.items.push({ manifest, recovery, jobUrl: item.jobUrl || '' });
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

function zipEntries(zipPath) {
  const names = childProcess.execFileSync('unzip', ['-Z1', zipPath], { encoding: 'utf8' }).split('\n').filter(Boolean);
  if (names.length > 10000) fail('artifact has too many entries');
  for (const name of names) {
    const normalized = name.replaceAll('\\', '/');
    if (normalized.startsWith('/') || normalized.includes('../') || normalized === '..' || normalized.includes('\u0000')) fail('artifact contains unsafe archive entry');
  }
  return names;
}

// アーカイブが申告する非圧縮の合計サイズを、展開の前に読む。
function zipUncompressedSize(zipPath, artifactName) {
  const summary = childProcess.execFileSync('unzip', ['-Z', '-t', zipPath], { encoding: 'utf8' });
  const match = /(\d+)\s+bytes uncompressed/.exec(summary);
  if (!match) fail(`${artifactName} has no readable archive summary`);
  return Number(match[1]);
}

// アーティファクトはfork PRからも届くため、展開の前に上限を判定し、manifest.json以外は取り出さない。
// unzip -pはディスクへ書かず、maxBufferが実際の展開量も頭打ちにするので、
// 中央ディレクトリが小さいサイズを申告する高圧縮率のアーカイブでも被害が出ない。
function readZipManifest(zipPath, artifactName) {
  const entries = zipEntries(zipPath);
  const manifestName = entries.find((name) => name === 'manifest.json' || name.endsWith('/manifest.json'));
  if (!manifestName) fail(`${artifactName} has no manifest.json`);
  if (/[*?[\\]/.test(manifestName)) fail(`${artifactName} has an unsupported manifest.json entry name`);
  if (zipUncompressedSize(zipPath, artifactName) > 100 * 1024 * 1024) fail(`${artifactName} expands beyond the size limit`);
  let data;
  try {
    data = childProcess.execFileSync('unzip', ['-p', zipPath, manifestName], { encoding: 'utf8', maxBuffer: 20 * 1024 * 1024 });
  } catch {
    fail(`${artifactName} manifest.json could not be extracted within the size limit`);
  }
  return validateManifest(JSON.parse(data), artifactName);
}

async function collectFromArtifacts({ github, owner, repo, runId, attempt, artifacts }) {
  const result = [];
  for (const artifact of artifacts) {
    const response = await github.rest.actions.downloadArtifact({ owner, repo, artifact_id: artifact.id, archive_format: 'zip' });
    const data = Buffer.isBuffer(response.data) ? response.data : Buffer.from(response.data);
    if (data.length > 50 * 1024 * 1024) fail(`${artifact.name} is too large`);
    const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'wx-ci-artifact-')), `${artifact.id}.zip`);
    fs.writeFileSync(file, data, { mode: 0o600 });
    try {
      const manifest = readZipManifest(file, artifact.name);
      const match = /^ci-tests-(coverage|race-daemon|race-rest)-/.exec(artifact.name);
      if (!match || manifest.profile !== match[1]) fail(`${artifact.name} profile does not match its artifact name`);
      result.push({ artifactName: artifact.name, manifest });
    } finally {
      fs.rmSync(path.dirname(file), { recursive: true, force: true });
    }
  }
  return result;
}

async function run(options) {
  const github = options.github;
  const owner = options.owner || options.context?.repo?.owner;
  const repo = options.repo || options.context?.repo?.repo;
  const runId = String(options.sourceRunId || options.context?.payload?.workflow_run?.id || options.context?.repo?.run_id || '');
  const attempt = String(options.sourceAttempt || options.context?.payload?.workflow_run?.run_attempt || '1');
  if (!github || !owner || !repo || !/^\d+$/.test(runId) || !/^\d+$/.test(attempt)) throw new Error('source run and repository are required');
  const sourceRun = (await github.rest.actions.getWorkflowRun({ owner, repo, run_id: Number(runId) })).data;
  if (sourceRun.name !== 'CI' || (sourceRun.path && sourceRun.path !== '.github/workflows/ci.yml')) throw new Error('source run is not the CI workflow');
  if (sourceRun.status && sourceRun.status !== 'completed') throw new Error('source CI run has not completed');
  if (sourceRun.run_attempt && String(sourceRun.run_attempt) !== attempt) throw new Error('source run attempt does not match requested attempt');
  const source = {
    owner, repo, runId, attempt, event: sourceRun.event || '', ref: sourceRun.head_branch || sourceRun.head_sha || '',
    apiHeadSha: sourceRun.head_sha || '', prUrl: sourceRun.pull_requests?.[0]?.html_url || '', runUrl: sourceRun.html_url || `https://github.com/${owner}/${repo}/actions/runs/${runId}`,
    commitUrl: sourceRun.head_sha ? `https://github.com/${owner}/${repo}/commit/${sourceRun.head_sha}` : '',
    summary: (message) => options.core?.warning?.(message),
  };
  const jobs = await pages((page) => github.rest.actions.listJobsForWorkflowRun({ owner, repo, run_id: Number(runId), filter: 'all', per_page: 100, page }));
  const jobProfiles = new Map([['coverage-tests', 'coverage'], ['race (daemon)', 'race-daemon'], ['race (rest)', 'race-rest']]);
  const expected = new Set();
  const jobUrls = new Map();
  for (const job of jobs) {
    if (String(job.run_attempt || attempt) !== attempt) continue;
    const profile = jobProfiles.get(job.name);
    if (!profile) continue;
    const uploaded = (job.steps || []).some((step) => step.name === 'Upload CI test report' && step.conclusion === 'success');
    if (job.conclusion === 'success' || uploaded) expected.add(profile);
    if (job.html_url) jobUrls.set(profile, job.html_url);
  }
  const artifacts = await pages((page) => github.rest.actions.listWorkflowRunArtifacts({ owner, repo, run_id: Number(runId), per_page: 100, page }));
  const usable = artifacts.filter((artifact) => !artifact.expired && artifact.workflow_run?.id === Number(runId) && new RegExp(`^ci-tests-(coverage|race-daemon|race-rest)-${runId}-${attempt}$`).test(artifact.name));
  for (const profile of expected) {
    if (!usable.some((artifact) => artifact.name.startsWith(`ci-tests-${profile}-`))) throw new Error(`missing report artifact for ${profile}`);
  }
  const reports = options.reports || await collectFromArtifacts({ github, owner, repo, runId, attempt, artifacts: usable });
  for (const report of reports) {
    const profile = /^ci-tests-(coverage|race-daemon|race-rest)-/.exec(report.artifactName || '')?.[1] || report.manifest.profile;
    report.jobUrl = jobUrls.get(profile) || '';
  }
  const groups = aggregateManifests(reports, source);
  const results = [];
  for (const group of groups) results.push({ title: group.title, action: await upsertGroup({ github, owner, repo, group, source }) });
  const summary = `flaky reports: ${results.length} issue(s); ${results.filter((item) => item.action === 'created').length} created, ${results.filter((item) => item.action === 'commented' || item.action === 'reopened-commented').length} commented`;
  if (options.core?.summary) {
    await options.core.summary.addHeading('Flaky test reports').addRaw(`${summary}\n`).write();
  }
  return { source, results, groups };
}

module.exports = {
  aggregateManifests,
  buildIssueBody,
  buildIssueComment,
  issueTitle,
  marker,
  readZipManifest,
  run,
  upsertGroup,
  validateManifest,
};
