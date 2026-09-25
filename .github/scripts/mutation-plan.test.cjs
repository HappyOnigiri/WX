'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const planner = require('./mutation-plan.cjs');

const root = path.resolve(__dirname, '../..');

test('excludes fdexec from default and explicit package selection with a reason', () => {
  const values = ['./internal/config', './internal/fdexec', './internal/fdexec'];
  const result = planner.filterPackages(values);
  assert.deepEqual(result.selected, ['internal/config']);
  assert.deepEqual(result.excluded, [{
    package: './internal/fdexec',
    reason: planner.EXCLUDED_PACKAGES['internal/fdexec'],
  }]);
});

test('archive production sources are assigned exactly once', () => {
  const result = planner.validateArchiveSharding(root);
  assert.equal(result.sources.length, 13);
  assert.equal(new Set(result.shards.flatMap((shard) => shard.files)).size, result.sources.length);
  assert.deepEqual(result.shards.flatMap((shard) => shard.files).sort(), result.sources);
});

test('archive sharding rejects duplicate, missing, and unexpected sources', () => {
  const definitions = planner.ARCHIVE_SHARDS.map((shard) => ({ ...shard, files: [...shard.files] }));
  definitions[0].files.push(definitions[0].files[0]);
  assert.throws(() => planner.validateArchiveSharding(root, definitions), /assigned to/);

  const missing = planner.ARCHIVE_SHARDS.map((shard) => ({ ...shard, files: [...shard.files] }));
  missing[0].files.pop();
  assert.throws(() => planner.validateArchiveSharding(root, missing), /not assigned/);

  const unexpected = planner.ARCHIVE_SHARDS.map((shard) => ({ ...shard, files: [...shard.files] }));
  unexpected[0].files.push('future.go');
  assert.throws(() => planner.validateArchiveSharding(root, unexpected), /missing sources/);
});

test('archive matrix uses unique artifacts and excludes tests and other sources', () => {
  const plan = planner.buildPlan({ packages: ['./internal/archive'], groups: 4, root });
  assert.equal(plan.matrix.length, 1);
  assert.equal(new Set(plan.matrix.map((item) => item.id)).size, 1);
  assert.deepEqual(plan.matrix.map((item) => item.profiles), ['internal/archive']);
  const assigned = plan.matrix.flatMap((item) => {
    const excluded = item.exclude_files.split(' ').filter(Boolean);
    return plan.archiveSources.filter((source) => !excluded.includes(`${source}$`));
  });
  assert.deepEqual([...new Set(assigned)].sort(), plan.archiveSources);
  for (const item of plan.matrix) assert.match(item.exclude_files, /_test\\\.go\$/u);
});

test('planner fails when a production archive source is added', () => {
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'wx-mutation-plan-'));
  try {
    fs.mkdirSync(path.join(temporary, 'internal', 'archive'), { recursive: true });
    for (const source of planner.productionArchiveSources(root)) {
      fs.writeFileSync(path.join(temporary, 'internal', 'archive', source), 'package archive\n');
    }
    fs.writeFileSync(path.join(temporary, 'internal', 'archive', 'added.go'), 'package archive\n');
    assert.throws(() => planner.validateArchiveSharding(temporary), /not assigned/);
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true });
  }
});

test('planner creates a normal shard for remaining packages', () => {
  // shard生成と除外だけを見る。配分順がコミット済みの重みで変わらないよう、空の重み表を渡す。
  const plan = planner.buildPlan({
    packages: ['./internal/fdexec', './internal/config', './internal/state'],
    groups: 2,
    root,
    weights: { version: 1, run_id: '', packages: {} },
  });
  assert.deepEqual(plan.matrix.map((item) => item.id), ['group-1', 'group-2']);
  assert.equal(plan.matrix[0].packages, './internal/config');
  assert.equal(plan.matrix[1].packages, './internal/state');
  assert.equal(plan.excluded[0].package, './internal/fdexec');
});

test('light packages use deterministic weighted LPT and compact group IDs', () => {
  const weights = {
    version: 1,
    run_id: 'run-1',
    packages: { 'internal/config': 10, 'internal/state': 8, 'internal/foo': 7, 'internal/bar': 1 },
  };
  const values = ['./internal/bar', './internal/state', './internal/foo', './internal/config'];
  const first = planner.buildPlan({ packages: values, groups: 3, root, weights });
  const second = planner.buildPlan({ packages: [...values].reverse(), groups: 3, root, weights });
  assert.deepEqual(first.matrix, second.matrix);
  assert.deepEqual(first.matrix.map((item) => [item.id, item.profiles]), [
    ['group-1', 'internal/config'],
    ['group-2', 'internal/state'],
    ['group-3', 'internal/bar internal/foo'],
  ]);
  assert.deepEqual(first.unweighted, []);
});

test('unknown light packages use the weight-file median and are reported', () => {
  const plan = planner.buildPlan({
    packages: ['./internal/state', './internal/config', './internal/unknown'],
    groups: 2,
    root,
    weights: { version: 1, run_id: 'run-1', packages: { 'internal/config': 10, 'unrelated/profile': 2 } },
  });
  assert.deepEqual(plan.matrix.map((item) => item.profiles), ['internal/config', 'internal/state internal/unknown']);
  assert.deepEqual(plan.unweighted, ['internal/state', 'internal/unknown']);
});

test('weights loader tolerates a missing file and rejects malformed values', () => {
  const missing = planner.loadMutationWeights(path.join(os.tmpdir(), 'wx-mutation-weights-does-not-exist.json'));
  assert.deepEqual(missing.packages, {});
  for (const value of [
    {},
    { version: 2, packages: {} },
    { version: 1, packages: { '../outside': 1 } },
    { version: 1, packages: { 'internal/config': -1 } },
    { version: 1, packages: { 'internal/config': Infinity } },
  ]) {
    assert.throws(() => planner.validateMutationWeights(value), /mutation weights/);
  }
});

test('median weight uses all values and falls back for empty or zero tables', () => {
  assert.equal(planner.medianWeight([9, 1, 5]), 5);
  assert.equal(planner.medianWeight([10, 2]), 6);
  assert.equal(planner.medianWeight([]), 1);
  assert.equal(planner.medianWeight([0, 0]), 1);
});

test('heavy packages expand into deterministic file-shard matrix entries', () => {
  const plan = planner.buildPlan({
    packages: ['./internal/daemon', './internal/cli', './internal/workspace', './cmd/wx'],
    groups: 4,
    root,
  });
  const daemon = plan.matrix.filter((item) => item.profiles === 'internal/daemon');
  const sources = planner.productionGoSources(root, 'internal/daemon');
  assert.equal(daemon.length, sources.length);
  assert.equal(new Set(daemon.map((item) => item.id)).size, sources.length);
  assert.deepEqual(daemon.map((item) => item.shard_files).sort(), sources.map((file) => `internal/daemon/${file}`).sort());
  for (const item of daemon) {
    assert.equal(item.shard_count, 1);
    assert.match(item.id, /^package-internal-daemon-file-[a-z0-9-]+-[0-9a-f]{10}$/u);
    const selected = path.basename(item.shard_files);
    const patterns = item.exclude_files.split(' ');
    for (const source of sources) {
      const pattern = `^${source.replace('.', '\\.')}\$`;
      assert.equal(patterns.includes(pattern), source !== selected);
    }
    assert.equal(patterns.some((pattern) => new RegExp(pattern, 'u').test(selected)), false);
  }
  assert.deepEqual(plan.matrix.filter((item) => item.profiles === 'internal/cli').map((item) => item.id), [
    'package-internal-cli-1', 'package-internal-cli-2',
  ]);
  assert.equal(plan.matrix.find((item) => item.id === 'package-cmd-wx').shard_count, 1);
});

test('workflow wires planned shards, independent deadlines, and diagnostics', () => {
  const workflow = fs.readFileSync(path.join(root, '.github', 'workflows', 'mutation-hunt.yml'), 'utf8');
  const runner = fs.readFileSync(path.join(root, '.github', 'scripts', 'run-mutation-shard.sh'), 'utf8');
  const makefile = fs.readFileSync(path.join(root, 'Makefile'), 'utf8');
  assert.match(workflow, /shards: \$\{\{ steps\.plan\.outputs\.shards \}\}/u);
  assert.match(workflow, /EXPECTED_SHARDS: \$\{\{ needs\.plan\.outputs\.shards \}\}/u);
  assert.match(workflow, /expectedShards,/u);
  assert.match(workflow, /max-parallel: 8/u);
  assert.doesNotMatch(workflow, /job-timeout/u);
  assert.match(workflow, /timeout-minutes: 360\n/u);
  assert.match(workflow, /HUNT_JOB_TIMEOUT: "360"/u);
  assert.match(workflow, /validate-exclusions/u);
  assert.match(workflow, /run-mutation-shard\.sh/u);
  assert.match(runner, /--exclude-files/u);
  assert.match(runner, /mutationshard/u);
  assert.match(runner, /--dry-run/u);
  assert.match(runner, /-shard-files/u);
  assert.match(runner, /mutation-runner\.cjs/u);
  assert.match(runner, /HUNT_JOB_TIMEOUT - 20/u);
  assert.match(runner, /prlimit --as="\$mutation_address_space_limit" -- \.tools\/bin\/gremlins/u);
  assert.match(runner, /duration-seconds/u);
  assert.match(workflow, /\.unweighted\[\]/u);
  assert.match(runner, /execution\.json/u);
  assert.match(runner, /preflight\.json/u);
  assert.ok(runner.includes(`printf '{"files":[]}\\n'`));
  assert.doesNotMatch(runner, /failures=\$\(\(failures \+ one_survivors\)\)/u);
  assert.match(makefile, /\.\/internal\/fdexec\|internal\/fdexec/u);
  assert.match(makefile, /mutation-weights/u);
});
