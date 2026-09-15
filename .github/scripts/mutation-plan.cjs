'use strict';

const fs = require('node:fs');
const path = require('node:path');

const EXCLUDED_PACKAGES = Object.freeze({
  'internal/fdexec': 'process/OS adapter (unix.Close, unix.Exec, os.Exit) can terminate or disconnect the hosted runner',
});

const DEDICATED_PACKAGES = new Set([
  'internal/daemon',
  'internal/workspace',
  'internal/cli',
  'cmd/wx',
]);

// 重量級パッケージはhunt側でdry-runの変異数を計測してファイル分割する。
const DYNAMIC_SHARD_COUNTS = Object.freeze({
  'internal/daemon': 4,
  'internal/cli': 2,
  'internal/workspace': 2,
});

// archive の production source は固定の shard 定義で管理し、追加・削除を自動で吸収しない。
const ARCHIVE_SHARDS = Object.freeze([
  Object.freeze({ id: 'package-internal-archive', files: Object.freeze([
    'archive.go', 'conflict.go', 'gitstate.go', 'lfs.go', 'orphan_refs.go', 'remove.go',
    'restore.go', 'skip_worktree.go', 'submodule_capsule.go', 'submodules.go', 'workspace.go',
    'workspace_exclusions.go',
  ]) }),
]);

function packageProfile(value) {
  const normalized = String(value || '').replaceAll('\\', '/').trim();
  if (normalized === '.') return '.';
  return normalized.replace(/^\.\//u, '').replace(/\/+$/u, '');
}

function packagePath(profile) {
  return profile === '.' ? '.' : `./${profile}`;
}

function filterPackages(values) {
  const selected = new Set();
  const excluded = [];
  const excludedNames = new Set();
  for (const value of values || []) {
    const profile = packageProfile(value);
    if (!profile) continue;
    const reason = EXCLUDED_PACKAGES[profile];
    if (reason) {
      if (!excludedNames.has(profile)) {
        excluded.push({ package: packagePath(profile), reason });
        excludedNames.add(profile);
      }
      continue;
    }
    if (profile !== 'internal/testsupport' && !profile.startsWith('tools/')) selected.add(profile);
  }
  return { selected: [...selected].sort(), excluded };
}

function productionArchiveSources(root) {
  const directory = path.join(root, 'internal', 'archive');
  return fs.readdirSync(directory, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.go') && !entry.name.endsWith('_test.go'))
    .map((entry) => entry.name)
    .sort();
}

function validateArchiveSharding(root, definitions = ARCHIVE_SHARDS) {
  if (!Array.isArray(definitions) || definitions.length === 0) throw new Error('archive shard definitions must not be empty');
  const seenIDs = new Set();
  const seenFiles = new Map();
  for (const shard of definitions) {
    if (!shard || typeof shard.id !== 'string' || !/^package-internal-archive(?:-shard-[1-9]\d*)?$/u.test(shard.id)) {
      throw new Error('archive shard has an invalid ID');
    }
    if (seenIDs.has(shard.id)) throw new Error(`archive shard ${shard.id} is duplicated`);
    seenIDs.add(shard.id);
    if (!Array.isArray(shard.files) || shard.files.length === 0) throw new Error(`archive shard ${shard.id} has no files`);
    for (const file of shard.files) {
      if (typeof file !== 'string' || !/^[-_a-z0-9]+\.go$/iu.test(file) || file.endsWith('_test.go')) {
        throw new Error(`archive shard ${shard.id} has an invalid source ${file}`);
      }
      const previous = seenFiles.get(file);
      if (previous) throw new Error(`archive source ${file} is assigned to ${previous} and ${shard.id}`);
      seenFiles.set(file, shard.id);
    }
  }
  const actual = productionArchiveSources(root);
  const missing = actual.filter((file) => !seenFiles.has(file));
  const unexpected = [...seenFiles.keys()].filter((file) => !actual.includes(file));
  if (missing.length > 0) throw new Error(`archive sources are not assigned: ${missing.join(', ')}`);
  if (unexpected.length > 0) throw new Error(`archive shard assigns missing sources: ${unexpected.join(', ')}`);
  return { sources: actual, shards: definitions.map((shard) => ({ id: shard.id, files: [...shard.files] })) };
}

function regexpEscape(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/gu, '\\$&');
}

function archiveExcludeFiles(allSources, selectedSources) {
  const selected = new Set(selectedSources);
  return ['_test\\.go$'].concat(allSources.filter((file) => !selected.has(file)).map((file) => `${regexpEscape(file)}$`));
}

function shardEntry(id, packages, profiles, excludeFiles = [], options = {}) {
  return {
    id,
    packages: packages.join(' '),
    profiles: profiles.join(' '),
    exclude_files: excludeFiles.join(' '),
    shard_count: options.shardCount || 1,
    shard_index: options.shardIndex || 0,
    shard: (options.shardIndex || 0) + 1,
    shards: options.shardCount || 1,
    shard_files: (options.shardFiles || []).join(' '),
  };
}

function dedicatedShardEntries(profile) {
  const count = DYNAMIC_SHARD_COUNTS[profile] || 1;
  const packageName = packagePath(profile);
  const baseID = `package-${profile.replaceAll('/', '-')}`;
  if (count === 1) return [shardEntry(baseID, [packageName], [profile])];
  return Array.from({ length: count }, (_, index) => shardEntry(
    `${baseID}-${index + 1}`,
    [packageName],
    [profile],
    [],
    { shardCount: count, shardIndex: index },
  ));
}

function buildPlan({ packages, groups = 4, root = process.cwd() }) {
  if (!Number.isInteger(groups) || groups < 1 || groups > 20) throw new Error('groups must be 1-20');
  const archive = validateArchiveSharding(root);
  const filtered = filterPackages(packages);
  const matrix = [];
  const light = [];
  for (const profile of filtered.selected) {
    if (profile === 'internal/archive') {
      for (const shard of archive.shards) {
        matrix.push(shardEntry(
          shard.id,
          ['./internal/archive'],
          ['internal/archive'],
          archiveExcludeFiles(archive.sources, shard.files),
          { shardFiles: shard.files.map((file) => `internal/archive/${file}`) },
        ));
      }
    } else if (DEDICATED_PACKAGES.has(profile)) {
      matrix.push(...dedicatedShardEntries(profile));
    } else {
      light.push(profile);
    }
  }
  const buckets = Array.from({ length: groups }, () => []);
  light.forEach((profile, index) => buckets[index % groups].push(profile));
  buckets.forEach((bucket, index) => {
    if (bucket.length === 0) return;
    matrix.push(shardEntry(
      `group-${index + 1}`,
      bucket.map(packagePath),
      bucket,
    ));
  });
  return {
    matrix,
    expectedShards: matrix,
    excluded: filtered.excluded,
    archiveSources: archive.sources,
  };
}

function parseArguments(argv) {
  const options = { groups: 4, root: process.cwd() };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === '--groups') options.groups = Number(argv[++index]);
    else if (argument === '--root') options.root = argv[++index];
    else throw new Error(`unknown argument ${argument}`);
  }
  return options;
}

function main() {
  const options = parseArguments(process.argv.slice(2));
  const input = fs.readFileSync(0, 'utf8').split(/\r?\n/u).filter(Boolean);
  process.stdout.write(`${JSON.stringify(buildPlan({ ...options, packages: input }))}\n`);
}

if (require.main === module) {
  try {
    main();
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}

module.exports = {
  ARCHIVE_SHARDS,
  DEDICATED_PACKAGES,
  DYNAMIC_SHARD_COUNTS,
  EXCLUDED_PACKAGES,
  archiveExcludeFiles,
  buildPlan,
  filterPackages,
  packagePath,
  packageProfile,
  productionArchiveSources,
  validateArchiveSharding,
};
