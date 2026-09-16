'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const childProcess = require('node:child_process');

const PROFILE_CONTRACTS = Object.freeze([
  Object.freeze({ profile: 'coverage', job: 'coverage-tests' }),
  Object.freeze({ profile: 'race-daemon-0', job: 'race (daemon-0)' }),
  Object.freeze({ profile: 'race-daemon-1', job: 'race (daemon-1)' }),
  Object.freeze({ profile: 'race-daemon-2', job: 'race (daemon-2)' }),
  Object.freeze({ profile: 'race-rest-0', job: 'race (rest-0)' }),
  Object.freeze({ profile: 'race-rest-1', job: 'race (rest-1)' }),
  // 旧artifactを手動で再処理できるよう、以前のprofileも受け付ける。
  Object.freeze({ profile: 'race-state-0', job: 'race (state-0)' }),
  Object.freeze({ profile: 'race-state-1', job: 'race (state-1)' }),
  Object.freeze({ profile: 'race-daemon', job: 'race (daemon)' }),
  Object.freeze({ profile: 'race-state', job: 'race (state)' }),
  Object.freeze({ profile: 'race-rest', job: 'race (rest)' }),
]);
const PROFILES = new Set(PROFILE_CONTRACTS.map(({ profile }) => profile));
const PROFILE_PATTERN = PROFILE_CONTRACTS
  .map(({ profile }) => profile)
  .sort((a, b) => b.length - a.length)
  .map((profile) => profile.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
  .join('|');
const ARTIFACT_PROFILE = new RegExp(`^ci-tests-(${PROFILE_PATTERN})-`);
const SHA = /^[0-9a-f]{40}$/i;
const MAX_TEXT = 12000;

// 起票の対象にする workflow。表示名とpathの両方が一致したものだけを受け付ける。
// workflows: に並ぶ名前と同じく workflow の name: に依存するので、名前を変えるとここも直す必要がある。
const WORKFLOW_CONTRACTS = Object.freeze([
  Object.freeze({ kind: 'ci', name: 'CI', path: '.github/workflows/ci.yml' }),
  Object.freeze({ kind: 'hunt', name: 'Flake Hunt', path: '.github/workflows/flake-hunt.yml' }),
]);

const HUNT_KIND = 'flake-hunt';
const HUNT_SCHEMA_VERSION = 1;
const HUNT_JOB = /^hunt-\d{1,3}$/;
const HUNT_UPLOAD_STEP = 'Upload flake hunt report';
const CI_UPLOAD_STEP = 'Upload CI test report';
// 1ラウンドしか回らなかったハントは、通常CIの1回の再実行より弱い証拠にしかならない。
const MIN_HUNT_ROUNDS = 2;
// 1つのrunで開く issue の上限。超えた分は step summary にだけ残す。
const MAX_ISSUES_PER_RUN = 20;
const MAX_HUNT_EXCERPTS = 2;
const MAX_HUNT_SEEDS = 10;

function huntArtifactPattern(runId, attempt) {
  return new RegExp(`^flake-hunt-(hunt-\\d{1,3})-${runId}-${attempt}$`);
}

function huntIdFromArtifactName(name, runId, attempt) {
  return huntArtifactPattern(runId, attempt).exec(name || '')?.[1] || '';
}

function profileFromArtifactName(name) {
  return ARTIFACT_PROFILE.exec(name || '')?.[1] || '';
}

function artifactNamePattern(runId, attempt) {
  return new RegExp(`^ci-tests-(${PROFILE_PATTERN})-${runId}-${attempt}$`);
}

// 検証中の対象名。fail から呼ばれる text / relativePath にも同じ文言を載せる。
let reportSubject = 'CI test report';

function fail(message) {
  throw new Error(`invalid ${reportSubject}: ${message}`);
}

// withSubject は同期の検証だけを包む。非同期を渡すと対象名が別の検証へ漏れる。
function withSubject(subject, action) {
  const previous = reportSubject;
  reportSubject = subject;
  try {
    return action();
  } finally {
    reportSubject = previous;
  }
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

// アーティファクト由来の文字列をcode spanとfenced blockへ埋められる形へ均す。
// code span内はバックスラッシュでエスケープが効かず、\` ではspanが閉じてしまうため、
// バックティックは全角へ置き換える。
function sanitize(value, limit = 4000) {
  return String(value ?? '').replace(/[\u0000-\u001f]/gu, ' ').replaceAll('`', '｀').replaceAll('@', '＠').replaceAll('<', '&lt;').replaceAll('>', '&gt;').slice(0, limit);
}

// 地の文へ埋める値に使う。リンク・画像の記法はここでだけ解釈され、
// GitHubのプロキシ越しに外部を取得させるので無効化する。
// code spanやfenced blockではバックスラッシュがそのまま見えるため、同じ処理はしない。
function sanitizeText(value, limit = 4000) {
  return sanitize(value, limit).replace(/[[\]!]/gu, '\\$&');
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
    `- duration: initial ${sanitizeText(initial.duration_ms ?? 'unknown', 100)} ms; retry ${sanitizeText(retry.duration_ms ?? 'unknown', 100)} ms`,
    `- result: initial ${sanitizeText(initial.status || initial.exit || 'unknown', 100)}; retry ${sanitizeText(retry.status || retry.exit || 'unknown', 100)}`,
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

function integer(value, name, limit) {
  if (!Number.isSafeInteger(value) || value < 0 || value > limit) fail(`${name} is not a bounded count`);
  return value;
}

// validateHuntManifest は validateManifest と同じ厳しさで hunt 側の manifest を検査する。
// schema_version はここへ来る前に判定する。未知の版は hard fail ではなく読み飛ばす扱いにするためである。
function validateHuntManifest(value, artifactName = 'artifact') {
  return withSubject('flake hunt report', () => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) fail(`${artifactName} is not an object`);
    if (value.kind !== HUNT_KIND) fail(`${artifactName} is not a flake hunt report`);
    if (!HUNT_JOB.test(String(value.hunt_id ?? ''))) fail(`${artifactName} has an invalid hunt_id`);
    if (value.test_sha && !SHA.test(value.test_sha)) fail(`${artifactName} has invalid test_sha`);
    if (value.api_head_sha && !SHA.test(value.api_head_sha)) fail(`${artifactName} has invalid api_head_sha`);
    if (value.run_id && !/^\d+$/.test(String(value.run_id))) fail(`${artifactName} has invalid run_id`);
    if (value.run_attempt && !/^\d+$/.test(String(value.run_attempt))) fail(`${artifactName} has invalid run_attempt`);
    for (const name of ['rounds', 'failed_rounds', 'anomaly_rounds']) integer(value[name], `${artifactName} ${name}`, 100000);
    if (!Array.isArray(value.command) || value.command.length > 200) fail(`${artifactName} has an invalid command`);
    for (const arg of value.command) text(arg, `${artifactName} command argument`, 2000);
    if (value.run_regexp !== undefined) text(value.run_regexp, `${artifactName} run_regexp`, 2000);
    if (value.count !== undefined) text(String(value.count), `${artifactName} count`, 100);
    if (value.round_records === undefined) value.round_records = [];
    if (!Array.isArray(value.round_records) || value.round_records.length > 100000) fail(`${artifactName} round_records is not a bounded array`);
    for (const [index, round] of value.round_records.entries()) {
      if (!round || typeof round !== 'object') fail(`${artifactName} round ${index} is not an object`);
      integer(round.round, `${artifactName} round ${index} number`, 100000);
      text(round.status ?? '', `${artifactName} round ${index} status`, 100);
      text(String(round.shuffle ?? ''), `${artifactName} round ${index} shuffle`, 100);
    }
    if (value.tests === undefined) value.tests = [];
    if (!Array.isArray(value.tests)) fail(`${artifactName} tests is not an array`);
    if (value.tests.length > 1000) fail(`${artifactName} has too many tests`);
    for (const [index, item] of value.tests.entries()) {
      if (!item || typeof item !== 'object') fail(`${artifactName} test ${index} is not an object`);
      const declaration = item.declaration;
      if (!declaration || typeof declaration !== 'object') fail(`${artifactName} test ${index} has no declaration`);
      relativePath(declaration.path, `${artifactName} test ${index} path`);
      const fn = text(declaration.function, `${artifactName} test ${index} function`, 500);
      if (!/^(Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]+$/.test(fn)) fail(`${artifactName} test ${index} has invalid function`);
      text(item.package, `${artifactName} test ${index} package`, 500);
      for (const name of ['pass_count', 'fail_count', 'skip_count']) integer(item[name], `${artifactName} test ${index} ${name}`, 1000000);
      if (item.subtests === undefined) item.subtests = [];
      if (!Array.isArray(item.subtests) || item.subtests.length > 200) fail(`${artifactName} test ${index} has invalid subtests`);
      for (const name of item.subtests) text(name, `${artifactName} test ${index} subtest`, 1000);
      if (item.log_excerpt !== undefined) text(item.log_excerpt, `${artifactName} test ${index} log_excerpt`, MAX_TEXT);
    }
    if (value.diagnostics === undefined) value.diagnostics = [];
    if (!Array.isArray(value.diagnostics) || value.diagnostics.length > 1000) fail(`${artifactName} diagnostics is not a bounded array`);
    for (const item of value.diagnostics) text(item, `${artifactName} diagnostic`, 2000);
    return value;
  });
}

// aggregateHuntManifests は全コンテナの manifest を合算してから flaky を判定する。
// artifact が欠けると pass の証拠だけが減るので、決定的な失敗を flaky と誤報する方向へは働かない。
// この非対称性があるので、欠落は warning に留めて起票を進めてよい。
function aggregateHuntManifests(manifests, source) {
  const hunt = { containers: 0, rounds: 0, failedRounds: 0, anomalyRounds: 0, commands: new Set(), runRegexp: new Set(), counts: new Set() };
  const groups = new Map();
  for (const item of manifests) {
    const man = validateHuntManifest(item.manifest, item.artifactName);
    const artifactHuntId = item.huntId || '';
    if (artifactHuntId && man.hunt_id !== artifactHuntId) {
      withSubject('flake hunt report', () => fail(`${item.artifactName} hunt_id does not match its artifact name`));
    }
    if (man.run_id && String(man.run_id) !== String(source.runId)) {
      withSubject('flake hunt report', () => fail(`${item.artifactName} run_id does not match source run`));
    }
    if (man.run_attempt && String(man.run_attempt) !== String(source.attempt)) {
      withSubject('flake hunt report', () => fail(`${item.artifactName} run_attempt does not match source attempt`));
    }
    hunt.containers += 1;
    hunt.rounds += man.rounds;
    hunt.failedRounds += man.failed_rounds;
    hunt.anomalyRounds += man.anomaly_rounds;
    hunt.commands.add(man.command.join(' '));
    if (man.run_regexp) hunt.runRegexp.add(man.run_regexp);
    if (man.count) hunt.counts.add(String(man.count));
    const seeds = man.round_records.filter((round) => round.status !== 'passed' && round.shuffle).map((round) => `${man.hunt_id}#${round.round}=${round.shuffle}`);
    for (const tally of man.tests) {
      const key = `${tally.declaration.path}\n${tally.declaration.function}`;
      let group = groups.get(key);
      if (!group) {
        group = {
          kind: 'hunt', key, hunt, title: issueTitle(tally.declaration),
          marker: marker({ ...source, declaration: tally.declaration }),
          declaration: tally.declaration, pass: 0, fail: 0, skip: 0,
          packages: new Set(), subtests: new Set(), containers: new Set(), excerpts: [], seeds: [],
        };
        groups.set(key, group);
      }
      group.pass += tally.pass_count;
      group.fail += tally.fail_count;
      group.skip += tally.skip_count;
      group.packages.add(tally.package);
      group.containers.add(man.hunt_id);
      for (const name of tally.subtests) group.subtests.add(name);
      if (tally.log_excerpt && group.excerpts.length < MAX_HUNT_EXCERPTS) group.excerpts.push({ huntId: man.hunt_id, text: tally.log_excerpt });
      for (const seed of seeds) if (group.seeds.length < MAX_HUNT_SEEDS && !group.seeds.includes(seed)) group.seeds.push(seed);
    }
  }
  // ラウンドをまたいで1回でも成功し、かつ1回以上失敗したテストだけを起票する。
  // 全ラウンド失敗する決定的な失敗と、証拠のうすい1ラウンドだけのハントは対象にしない。
  const flaky = hunt.rounds >= MIN_HUNT_ROUNDS ? [...groups.values()].filter((group) => group.pass > 0 && group.fail > 0) : [];
  return flaky.sort((a, b) => a.title.localeCompare(b.title));
}

// huntDetail はコンテナごとの羅列ではなく集計値を並べる。
// 20コンテナ分を並べると60000文字の切り詰めに掛かり、代表的な失敗ログまで落ちる。
function huntDetail(group) {
  const hunt = group.hunt;
  const lines = [
    `- command: \`${sanitize([...hunt.commands].join(' | '), 2000)}\``,
    `- \`-run\`: \`${sanitize([...hunt.runRegexp].join(' | ') || 'not restricted', 500)}\``,
    `- \`-count\` per round: \`${sanitize([...hunt.counts].join(' | ') || 'not recorded', 100)}\``,
    `- containers: ${hunt.containers}; rounds: ${hunt.rounds} (failed ${hunt.failedRounds}, anomalous ${hunt.anomalyRounds})`,
    `- observed for this test: ${group.pass} pass, ${group.fail} fail, ${group.skip} skip across ${group.containers.size} container(s)`,
    `- package: \`${sanitize([...group.packages].join(', '), 500)}\``,
    `- failing test names: \`${sanitize([...group.subtests].join(', '), 2000)}\``,
    // seedは-shuffle=onのラウンド単位の値で、-count>1の反復ごとの並びまでは決めない。
    `- shuffle seed per round: \`${sanitize(group.seeds.join(', ') || 'not recorded', 1000)}\``,
  ];
  for (const item of group.excerpts) {
    lines.push('', `Failure excerpt (\`${sanitize(item.huntId, 100)}\`):`, '```text', sanitize(item.text, 4000), '```');
  }
  lines.push('', 'The test both passed and failed within this hunt. This records an observation, not a diagnosis of the test or product.');
  return lines.join('\n');
}

function huntHeader(source) {
  return [
    `Source workflow: [Flake Hunt run ${source.runId}](${source.runUrl}) (attempt ${source.attempt})`,
    `Event: \`${sanitize(source.event, 200)}\`; ref: \`${sanitize(source.ref, 500)}\``,
    `API head SHA: [${source.apiHeadSha || 'unknown'}](${source.commitUrl || '#'})`,
  ];
}

function buildHuntIssueBody(group, source) {
  return [
    group.marker,
    `## ${sanitizeText(issueTitle(group.declaration), 1000)}`,
    '',
    ...huntHeader(source),
    '',
    'Flake Hunt repeated this test profile and observed both a pass and a failure:',
    '',
    huntDetail(group),
  ].join('\n').slice(0, 60000);
}

function buildHuntIssueComment(group, source) {
  return [
    group.marker,
    `Reproduced by [Flake Hunt run ${source.runId}](${source.runUrl}) (attempt ${source.attempt}).`,
    '',
    huntDetail(group),
  ].join('\n').slice(0, 60000);
}

function buildIssueBody(group, source) {
  if (group.kind === 'hunt') return buildHuntIssueBody(group, source);
  const first = group.items[0];
  const declaration = first.recovery.declaration;
  const lines = [
    group.marker,
    `## ${sanitizeText(issueTitle(declaration), 1000)}`,
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
  if (group.kind === 'hunt') return buildHuntIssueComment(group, source);
  return [group.marker, `Reproduction observed in [CI run ${source.runId}](${source.runUrl}) (attempt ${source.attempt}).`, '', ...group.items.map((item) => `### ${sanitize(item.manifest.profile, 100)}\n${excerpt(item.manifest, item.recovery, item.jobUrl)}`)].join('\n').slice(0, 60000);
}

function aggregateManifests(manifests, source) {
  const groups = new Map();
  for (const item of manifests) {
    const manifest = validateManifest(item.manifest, item.artifactName);
    const artifactProfile = profileFromArtifactName(item.artifactName);
    if (artifactProfile && manifest.profile !== artifactProfile) fail(`${item.artifactName} profile does not match its artifact name`);
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

// issues を渡すと全ページ取得を1回に巻き上げられる。huntは数十グループになり得るため、
// 作成した issue は同じ配列へ積み、同じrunの後続グループから見えるようにする。
async function upsertGroup({ github, owner, repo, group, source, issues }) {
  const known = issues || await listIssues(github, owner, repo);
  const matching = known.filter((item) => item.title === group.title).sort((a, b) => a.number - b.number);
  const issue = matching[0];
  // concurrencyのqueueが効かずCIとFlake Huntが同時に走ると、同じタイトルのissueが並び得る。
  // 先頭だけを使い、重複は警告として残す。
  if (matching.length > 1 && source.summary) source.summary(`duplicate issue titles for ${group.title}: ${matching.slice(1).map((item) => item.number).join(', ')}`);
  if (!issue) {
    const body = buildIssueBody(group, source);
    const created = await github.rest.issues.create({ owner, repo, title: group.title, body });
    if (issues) issues.push({ number: created?.data?.number ?? Number.MAX_SAFE_INTEGER, title: group.title, body, state: 'open' });
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
function extractManifest(zipPath, artifactName) {
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
  return JSON.parse(data);
}

function readZipManifest(zipPath, artifactName) {
  return validateManifest(extractManifest(zipPath, artifactName), artifactName);
}

function downloadArtifactZip(github, owner, repo, artifact, read) {
  return (async () => {
    const response = await github.rest.actions.downloadArtifact({ owner, repo, artifact_id: artifact.id, archive_format: 'zip' });
    const data = Buffer.isBuffer(response.data) ? response.data : Buffer.from(response.data);
    if (data.length > 50 * 1024 * 1024) fail(`${artifact.name} is too large`);
    const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'wx-ci-artifact-')), `${artifact.id}.zip`);
    fs.writeFileSync(file, data, { mode: 0o600 });
    try {
      return read(file);
    } finally {
      fs.rmSync(path.dirname(file), { recursive: true, force: true });
    }
  })();
}

async function collectFromArtifacts({ github, owner, repo, artifacts }) {
  const result = [];
  for (const artifact of artifacts) {
    const manifest = await downloadArtifactZip(github, owner, repo, artifact, (file) => readZipManifest(file, artifact.name));
    const artifactProfile = profileFromArtifactName(artifact.name);
    if (!artifactProfile || manifest.profile !== artifactProfile) fail(`${artifact.name} profile does not match its artifact name`);
    result.push({ artifactName: artifact.name, manifest });
  }
  return result;
}

async function collectFromHuntArtifacts({ github, owner, repo, runId, attempt, artifacts }) {
  const result = [];
  for (const artifact of artifacts) {
    const manifest = await downloadArtifactZip(github, owner, repo, artifact, (file) => extractManifest(file, artifact.name));
    result.push({ artifactName: artifact.name, huntId: huntIdFromArtifactName(artifact.name, runId, attempt), manifest });
  }
  return result;
}

// collectCI は通常CIのrunから、profileごとのmanifestを集める。
// 期待するartifactはPROFILE_CONTRACTSの静的な表から決まる。
async function collectCI({ github, owner, repo, runId, attempt, source, options }) {
  const jobs = await pages((page) => github.rest.actions.listJobsForWorkflowRun({ owner, repo, run_id: Number(runId), filter: 'all', per_page: 100, page }));
  const jobProfiles = new Map(PROFILE_CONTRACTS.map(({ job, profile }) => [job, profile]));
  const expected = new Set();
  const jobUrls = new Map();
  for (const job of jobs) {
    if (String(job.run_attempt || attempt) !== attempt) continue;
    const profile = jobProfiles.get(job.name);
    if (!profile) continue;
    const uploaded = (job.steps || []).some((step) => step.name === CI_UPLOAD_STEP && step.conclusion === 'success');
    if (job.conclusion === 'success' || uploaded) expected.add(profile);
    if (job.html_url) jobUrls.set(profile, job.html_url);
  }
  const artifacts = await pages((page) => github.rest.actions.listWorkflowRunArtifacts({ owner, repo, run_id: Number(runId), per_page: 100, page }));
  const usable = artifacts.filter((artifact) => !artifact.expired && artifact.workflow_run?.id === Number(runId) && artifactNamePattern(runId, attempt).test(artifact.name));
  // 取りこぼした成果物は最後に失敗として報告する。
  // ここで打ち切ると、同じrunの他のジョブが記録した回復まで起票されない。
  const missing = [...expected].filter((profile) => !usable.some((artifact) => profileFromArtifactName(artifact.name) === profile));
  for (const profile of missing) source.summary(`missing report artifact for ${profile}`);
  const reports = options.reports || await collectFromArtifacts({ github, owner, repo, artifacts: usable });
  for (const report of reports) {
    const profile = profileFromArtifactName(report.artifactName) || report.manifest.profile;
    report.jobUrl = jobUrls.get(profile) || '';
  }
  return { groups: aggregateManifests(reports, source), fatal: missing.length > 0 ? `missing report artifact for ${missing.join(', ')}` : '', fileIssues: true };
}

// collectHunt は Flake Hunt のrunから manifest を集める。
// 期待するartifactはコンテナ数がdispatch入力で決まるため、ジョブ名から動的に作る。
// 45分のgo testを回す都合でコンテナが落ちるのは普通に起きるので、欠落はwarningに留める。
async function collectHunt({ github, owner, repo, runId, attempt, source, options }) {
  const jobs = await pages((page) => github.rest.actions.listJobsForWorkflowRun({ owner, repo, run_id: Number(runId), filter: 'all', per_page: 100, page }));
  const expected = new Set();
  for (const job of jobs) {
    if (String(job.run_attempt || attempt) !== attempt) continue;
    if (!HUNT_JOB.test(job.name || '')) continue;
    const uploaded = (job.steps || []).some((step) => step.name === HUNT_UPLOAD_STEP && step.conclusion === 'success');
    if (job.conclusion === 'success' || uploaded) expected.add(job.name);
  }
  const artifacts = await pages((page) => github.rest.actions.listWorkflowRunArtifacts({ owner, repo, run_id: Number(runId), per_page: 100, page }));
  const issueFilingMarker = artifacts.some((artifact) =>
    !artifact.expired && artifact.workflow_run?.id === Number(runId) && artifact.name === `flake-hunt-no-issues-${runId}-${attempt}`);
  const usable = artifacts.filter((artifact) => !artifact.expired && artifact.workflow_run?.id === Number(runId) && huntArtifactPattern(runId, attempt).test(artifact.name));
  const missing = [...expected].filter((id) => !usable.some((artifact) => huntIdFromArtifactName(artifact.name, runId, attempt) === id));
  for (const id of missing) source.summary(`missing flake hunt report for ${id}`);
  const collected = options.reports || await collectFromHuntArtifacts({ github, owner, repo, runId, attempt, artifacts: usable });
  const reports = [];
  for (const report of collected) {
    // ハントは任意のブランチからdispatchでき、ブランチ側のツールが作ったmanifestを
    // default branchの検証器が読む。知らない版はrunを落とさず読み飛ばす。
    if (report.manifest?.schema_version !== HUNT_SCHEMA_VERSION) {
      source.summary(`skipping ${report.artifactName}: unsupported schema_version`);
      continue;
    }
    reports.push(report);
  }
  return { groups: aggregateHuntManifests(reports, source), fatal: '', fileIssues: !issueFilingMarker };
}

async function run(options) {
  const github = options.github;
  const owner = options.owner || options.context?.repo?.owner;
  const repo = options.repo || options.context?.repo?.repo;
  const runId = String(options.sourceRunId || options.context?.payload?.workflow_run?.id || options.context?.repo?.run_id || '');
  const attempt = String(options.sourceAttempt || options.context?.payload?.workflow_run?.run_attempt || '1');
  if (!github || !owner || !repo || !/^\d+$/.test(runId) || !/^\d+$/.test(attempt)) throw new Error('source run and repository are required');
  const sourceRun = (await github.rest.actions.getWorkflowRun({ owner, repo, run_id: Number(runId) })).data;
  // workflow_runの workflows: と同じく、workflowの表示名に依存する。pathも合わせて確認する。
  const contract = WORKFLOW_CONTRACTS.find((item) => item.name === sourceRun.name && (!sourceRun.path || sourceRun.path === item.path));
  if (!contract) throw new Error('source run is not a supported workflow');
  if (sourceRun.status && sourceRun.status !== 'completed') throw new Error('source run has not completed');
  if (sourceRun.run_attempt && String(sourceRun.run_attempt) !== attempt) throw new Error('source run attempt does not match requested attempt');
  const source = {
    owner, repo, runId, attempt, kind: contract.kind, event: sourceRun.event || '', ref: sourceRun.head_branch || sourceRun.head_sha || '',
    apiHeadSha: sourceRun.head_sha || '', prUrl: sourceRun.pull_requests?.[0]?.html_url || '', runUrl: sourceRun.html_url || `https://github.com/${owner}/${repo}/actions/runs/${runId}`,
    commitUrl: sourceRun.head_sha ? `https://github.com/${owner}/${repo}/commit/${sourceRun.head_sha}` : '',
    summary: (message) => options.core?.warning?.(message),
  };
  const collect = contract.kind === 'hunt' ? collectHunt : collectCI;
  const { groups, fatal, fileIssues: collectedFileIssues } = await collect({ github, owner, repo, runId, attempt, source, options });
  const fileIssues = options.fileIssues ?? collectedFileIssues ?? true;
  const issues = fileIssues ? await listIssues(github, owner, repo) : null;
  const results = [];
  const skipped = [];
  const notFiled = [];
  for (const group of groups) {
    if (!fileIssues) {
      results.push({ title: group.title, action: 'not-filed' });
      notFiled.push(group.title);
      continue;
    }
    if (results.length >= MAX_ISSUES_PER_RUN) {
      skipped.push(group.title);
      continue;
    }
    results.push({ title: group.title, action: await upsertGroup({ github, owner, repo, group, source, issues }) });
  }
  const summary = `flaky reports: ${results.length} issue(s); ${results.filter((item) => item.action === 'created').length} created, ${results.filter((item) => item.action === 'commented' || item.action === 'reopened-commented').length} commented`;
  if (options.core?.summary) {
    const writer = options.core.summary.addHeading('Flaky test reports').addRaw(`${summary}\n`);
    if (!fileIssues) writer.addRaw(`Not filed (file-issues=false):\n${notFiled.map((title) => `- ${title}`).join('\n') || '- none'}\n`);
    if (skipped.length > 0) writer.addRaw(`Not filed (over the ${MAX_ISSUES_PER_RUN} issue limit for one run):\n${skipped.map((title) => `- ${title}`).join('\n')}\n`);
    await writer.write();
  }
  if (fatal) throw new Error(fatal);
  return { source, results, groups, skipped, fileIssues, notFiled };
}

module.exports = {
  aggregateHuntManifests,
  aggregateManifests,
  buildIssueBody,
  buildIssueComment,
  issueTitle,
  marker,
  readZipManifest,
  run,
  upsertGroup,
  validateHuntManifest,
  validateManifest,
};
