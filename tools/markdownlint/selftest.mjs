// @ts-check

import assert from "node:assert/strict";
import { dirname, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const entryArgument = process.argv[2];
assert(entryArgument, "markdownlint entry path is required");

const entryPath = resolve(entryArgument);
const entry = await import(pathToFileURL(entryPath));
const promiseModulePath = entry.resolveModule("markdownlint/promise", [ dirname(entryPath) ]);
const { lint } = await import(pathToFileURL(promiseModulePath));
const { default: rule } = await import(pathToFileURL(resolve(dirname(import.meta.filename), "wx013.mjs")));

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

