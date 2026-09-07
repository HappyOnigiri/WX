// @ts-check

import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const entryArgument = process.argv[2];
assert(entryArgument, "markdownlint entry path is required");

const entryPath = resolve(entryArgument);
const entry = await import(pathToFileURL(entryPath));
const promiseModulePath = entry.resolveModule("markdownlint/promise", [ dirname(entryPath) ]);
const { lint } = await import(pathToFileURL(promiseModulePath));
const toolsDirectory = dirname(dirname(resolve(import.meta.filename)));
const { default: rule } = await import(pathToFileURL(join(toolsDirectory, "markdownlint", "wx013.mjs")));
const { default: linkRule } = await import(pathToFileURL(join(toolsDirectory, "checkdoclinks", "wx014.mjs")));

const source = [
  "a".repeat(200),
  "a".repeat(201),
  "あ".repeat(100),
  "あ".repeat(101),
  "①".repeat(200),
  "ｶ".repeat(200),
  "```text",
  "z".repeat(300),
  "```",
  "    " + "i".repeat(296),
  "| key | value |",
  "| --- | --- |",
  `| ${"t".repeat(300)} | value |`,
  "<!-- markdownlint-disable-next-line WX013 -->",
  "b".repeat(201)
].join("\n");

const result = await lint({
  strings: { fixture: source },
  config: {
    default: false,
    WX013: { line_width: 200 }
  },
  customRules: [ rule ]
});
const errors = result.fixture || [];

assert.deepEqual(errors.map((error) => error.lineNumber), [ 2, 4 ]);
assert.equal(errors[0].ruleNames[0], "WX013");
assert.equal(errors[0].ruleNames[1], "line-width");
assert.match(
  errors[0].errorDetail,
  /one sentence per line.*200 display columns.*full-width characters as two columns/
);

console.log("markdownlint WX013 self-test passed");

// WX014は文書のディスク上の位置を基準に解決するため、fixtureは実ファイルとして置く。
const fixtureRoot = mkdtempSync(join(tmpdir(), "wx-markdownlint-selftest-"));
try {
  writeFileSync(join(fixtureRoot, "real.txt"), "");
  writeFileSync(join(fixtureRoot, "sp ace.txt"), "");
  const documentPath = join(fixtureRoot, "guide.md");
  writeFileSync(documentPath, [
    "# Fixture",
    "",
    "[ok](./real.txt) [missing](./gone.txt)",
    "[url](https://example.com/none.txt) [anchor](#section) [frag](./real.txt#top)",
    "[encoded](./sp%20ace.txt) [absolute](/nowhere.txt)",
    "",
    "```text",
    "[fenced](./gone-fenced.txt)",
    "```",
    "",
    "`[span](./gone-span.txt)`",
    "",
    "[def]: ./gone-definition.txt",
    ""
  ].join("\n"));

  const linkConfig = { default: false, WX014: true };
  const fileResult = await lint({
    files: [ documentPath ],
    config: linkConfig,
    customRules: [ linkRule ]
  });
  const linkErrors = fileResult[documentPath] || [];
  assert.deepEqual(linkErrors.map((error) => error.lineNumber), [ 3, 13 ]);
  assert.equal(linkErrors[0].ruleNames[0], "WX014");
  assert.equal(linkErrors[0].ruleNames[1], "doc-file-links");
  assert.match(linkErrors[0].errorDetail, /Relative link target does not exist: \.\/gone\.txt/);
  assert.match(linkErrors[1].errorDetail, /gone-definition\.txt/);

  // 位置を持たない入力は相対解決の基準が無いため、報告せず通す。
  const stringResult = await lint({
    strings: { fixture: "[missing](./gone.txt)\n" },
    config: linkConfig,
    customRules: [ linkRule ]
  });
  assert.deepEqual(stringResult.fixture || [], []);
} finally {
  rmSync(fixtureRoot, { recursive: true, force: true });
}

console.log("markdownlint WX014 self-test passed");

