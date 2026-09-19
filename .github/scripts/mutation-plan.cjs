'use strict';

const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');

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

const MUTATION_WEIGHTS_VERSION = 1;
const DEFAULT_MUTATION_WEIGHTS = path.join(__dirname, 'mutation-weights.json');
const PROFILE_KEY = /^(?:\.|[A-Za-z0-9][A-Za-z0-9._-]*(?:\/[A-Za-z0-9][A-Za-z0-9._-]*)*)$/u;

function packageProfile(value) {
  const normalized = String(value || '').replaceAll('\\', '/').trim();
  if (normalized === '.') return '.';
  return normalized.replace(/^\.\//u, '').replace(/\/+$/u, '');
}

function packagePath(profile) {
  return profile === '.' ? '.' : `./${profile}`;
}

function validateMutationWeights(value, source = 'mutation weights') {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${source} must be an object`);
  if (value.version !== MUTATION_WEIGHTS_VERSION) throw new Error(`${source} has unsupported version`);
  if (!value.packages || typeof value.packages !== 'object' || Array.isArray(value.packages)) {
    throw new Error(`${source} packages must be an object`);
  }
  const packages = {};
  for (const [profile, weight] of Object.entries(value.packages)) {
    if (!PROFILE_KEY.test(profile)) throw new Error(`${source} has an invalid profile key ${profile}`);
    if (typeof weight !== 'number' || !Number.isFinite(weight) || weight < 0) {
      throw new Error(`${source} has an invalid weight for ${profile}`);
    }
    packages[profile] = weight;
  }
  return {
    version: MUTATION_WEIGHTS_VERSION,
    run_id: value.run_id === undefined ? '' : String(value.run_id),
    packages,
  };
}

function loadMutationWeights(input = DEFAULT_MUTATION_WEIGHTS) {
  if (input && typeof input === 'object') return validateMutationWeights(input);
  const weightPath = String(input || DEFAULT_MUTATION_WEIGHTS);
  let data;
  try {
    data = fs.readFileSync(weightPath, 'utf8');
  } catch (error) {
    if (error?.code === 'ENOENT') {
      return { version: MUTATION_WEIGHTS_VERSION, run_id: '', packages: {} };
    }
    throw new Error(`read mutation weights ${weightPath}: ${error.message}`);
  }
  let value;
  try {
    value = JSON.parse(data);
  } catch (error) {
    throw new Error(`parse mutation weights ${weightPath}: ${error.message}`);
  }
  return validateMutationWeights(value, `mutation weights ${weightPath}`);
}

function medianWeight(values) {
  const sorted = values.filter((value) => Number.isFinite(value) && value >= 0).sort((a, b) => a - b);
  if (sorted.length === 0) return 1;
  const middle = Math.floor(sorted.length / 2);
  const median = sorted.length % 2 === 0 ? (sorted[middle - 1] + sorted[middle]) / 2 : sorted[middle];
  return median > 0 && Number.isFinite(median) ? median : 1;
}

// 重みの降順（同値は名前順）で、最も軽いbucketへ詰めるLPTを使う。
function allocateWeightedBuckets(profiles, count, weights) {
  if (!Number.isInteger(count) || count < 1 || count > 20) throw new Error('groups must be 1-20');
  const table = weights || { packages: {} };
  const packages = table.packages || (table.version === undefined ? table : {});
  for (const [profile, weight] of Object.entries(packages)) {
    if (typeof weight !== 'number' || !Number.isFinite(weight) || weight < 0) {
      throw new Error(`mutation weights has an invalid weight for ${profile}`);
    }
  }
  const values = Object.values(packages);
  const median = medianWeight(values);
  const unweighted = [];
  const items = profiles.map((profile) => {
    if (!Object.prototype.hasOwnProperty.call(packages, profile)) unweighted.push(profile);
    return {
      profile,
      weight: Object.prototype.hasOwnProperty.call(packages, profile) ? packages[profile] : median,
    };
  });
  items.sort((left, right) => {
    if (right.weight !== left.weight) return right.weight - left.weight;
    return left.profile < right.profile ? -1 : left.profile > right.profile ? 1 : 0;
  });
  const buckets = Array.from({ length: count }, () => ({ profiles: [], total: 0 }));
  for (const item of items) {
    let target = 0;
    for (let index = 1; index < buckets.length; index += 1) {
      if (buckets[index].total < buckets[target].total) target = index;
    }
    buckets[target].profiles.push(item.profile);
    buckets[target].total += item.weight;
  }
  for (const bucket of buckets) bucket.profiles.sort();
  return {
    buckets: buckets.filter((bucket) => bucket.profiles.length > 0),
    unweighted: unweighted.sort(),
    median,
  };
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

function productionGoSources(root, profile) {
  const directory = path.join(root, ...profile.split('/'));
  return fs.readdirSync(directory, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.go') && !entry.name.endsWith('_test.go'))
    .map((entry) => entry.name)
    .sort();
}

function fileShardID(profile, file) {
  const stem = file.replace(/\.go$/u, '').replace(/[^a-z0-9-]+/giu, '-').replace(/^-+|-+$/gu, '').toLowerCase() || 'source';
  const digest = crypto.createHash('sha256').update(`${profile}\n${file}`).digest('hex').slice(0, 10);
  return `package-${profile.replaceAll('/', '-')}-file-${stem}-${digest}`;
}

function daemonShardEntries(root) {
  const profile = 'internal/daemon';
  const sources = productionGoSources(root, profile);
  return sources.map((file) => shardEntry(
    fileShardID(profile, file),
    [packagePath(profile)],
    [profile],
    ['_test\\.go$', ...sources.filter((candidate) => candidate !== file).map((candidate) => `${regexpEscape(candidate)}$`)],
    { shardFiles: [`${profile}/${file}`] },
  ));
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

function buildPlan({ packages, groups = 4, root = process.cwd(), weights, weightPath, weightsPath }) {
  if (!Number.isInteger(groups) || groups < 1 || groups > 20) throw new Error('groups must be 1-20');
  const archive = validateArchiveSharding(root);
  const filtered = filterPackages(packages);
  const weightInput = weights !== undefined ? weights : weightPath !== undefined ? weightPath : weightsPath;
  const weightTable = loadMutationWeights(weightInput);
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
    } else if (profile === 'internal/daemon') {
      matrix.push(...daemonShardEntries(root));
    } else if (DEDICATED_PACKAGES.has(profile)) {
      matrix.push(...dedicatedShardEntries(profile));
    } else {
      light.push(profile);
    }
  }
  const allocation = allocateWeightedBuckets(light, groups, weightTable);
  allocation.buckets.forEach((bucket, index) => {
    matrix.push(shardEntry(
      `group-${index + 1}`,
      bucket.profiles.map(packagePath),
      bucket.profiles,
    ));
  });
  return {
    matrix,
    expectedShards: matrix,
    excluded: filtered.excluded,
    unweighted: allocation.unweighted,
    archiveSources: archive.sources,
  };
}

function parseArguments(argv) {
  const options = { groups: 4, root: process.cwd(), weights: DEFAULT_MUTATION_WEIGHTS };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === '--groups') {
      if (index + 1 >= argv.length) throw new Error(`${argument} requires a value`);
      options.groups = Number(argv[++index]);
    } else if (argument === '--root') {
      if (index + 1 >= argv.length) throw new Error(`${argument} requires a value`);
      options.root = argv[++index];
    } else if (argument === '--weights' || argument === '--weight-file' || argument === '--weight') {
      if (index + 1 >= argv.length) throw new Error(`${argument} requires a value`);
      options.weights = argv[++index];
    } else {
      throw new Error(`unknown argument ${argument}`);
    }
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
  DEFAULT_MUTATION_WEIGHTS,
  MUTATION_WEIGHTS_VERSION,
  allocateWeightedBuckets,
  archiveExcludeFiles,
  buildPlan,
  filterPackages,
  daemonShardEntries,
  fileShardID,
  loadWeights: loadMutationWeights,
  loadMutationWeights,
  medianWeight,
  packagePath,
  packageProfile,
  productionArchiveSources,
  productionGoSources,
  validateMutationWeights,
  validateArchiveSharding,
};
