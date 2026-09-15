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
  assert.equal(result.sources.length, 12);
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
  const plan = planner.buildPlan({ packages: ['./internal/fdexec', './internal/config', './internal/state'], groups: 2, root });
  assert.deepEqual(plan.matrix.map((item) => item.id), ['group-1', 'group-2']);
  assert.equal(plan.matrix[0].packages, './internal/config');
  assert.equal(plan.matrix[1].packages, './internal/state');
  assert.equal(plan.excluded[0].package, './internal/fdexec');
});

test('heavy packages expand into deterministic file-shard matrix entries', () => {
  const plan = planner.buildPlan({
    packages: ['./internal/daemon', './internal/cli', './internal/workspace', './cmd/wx'],
    groups: 4,
    root,
  });
  assert.deepEqual(plan.matrix.map((item) => item.id), [
    'package-cmd-wx',
    'package-internal-cli-1',
    'package-internal-cli-2',
    'package-internal-daemon-1',
    'package-internal-daemon-2',
    'package-internal-daemon-3',
    'package-internal-daemon-4',
    'package-internal-workspace-1',
    'package-internal-workspace-2',
  ]);
  const daemon = plan.matrix.filter((item) => item.profiles === 'internal/daemon');
  assert.deepEqual(daemon.map((item) => [item.shard_count, item.shard_index]), [[4, 0], [4, 1], [4, 2], [4, 3]]);
  assert.deepEqual(daemon.map((item) => [item.shard, item.shards]), [[1, 4], [2, 4], [3, 4], [4, 4]]);
  assert.equal(plan.matrix.find((item) => item.id === 'package-cmd-wx').shard_count, 1);
});

test('workflow wires planned shards, archive exclusions, and resource diagnostics', () => {
  const workflow = fs.readFileSync(path.join(root, '.github', 'workflows', 'mutation-hunt.yml'), 'utf8');
  const makefile = fs.readFileSync(path.join(root, 'Makefile'), 'utf8');
  assert.match(workflow, /shards: \$\{\{ steps\.plan\.outputs\.shards \}\}/u);
  assert.match(workflow, /EXPECTED_SHARDS: \$\{\{ needs\.plan\.outputs\.shards \}\}/u);
  assert.match(workflow, /expectedShards,/u);
  assert.match(workflow, /--exclude-files/u);
  assert.match(workflow, /mutationshard/u);
  assert.match(workflow, /--dry-run/u);
  assert.match(workflow, /-shard-files/u);
  assert.match(workflow, /Mutation resource heartbeat/u);
  assert.match(makefile, /\.\/internal\/fdexec\|internal\/fdexec/u);
});
