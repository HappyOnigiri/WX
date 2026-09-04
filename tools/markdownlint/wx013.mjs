// @ts-check

const DEFAULT_LINE_WIDTH = 200;
const ERROR_DETAIL =
  "Write one sentence per line; split sentences over 200 display columns; " +
  "count full-width characters as two columns.";

// East Asian Width W/F characters occupy two terminal columns. Ambiguous
// characters intentionally remain narrow, as do half-width katakana.
const FULL_WIDTH_RE = /[ᄀ-ᅟ⌚-⌛〈-〉⏩-⏬⏰⏳◽-◾☔-☕♈-♓♿⚓⚡⚪-⚫⚽-⚾⛄-⛅⛎⛔⛪⛲-⛳⛵⛺⛽✅✊-✋✨❌❎❓-❕❗➕-➗➰➿⬛-⬜⭐⭕⺀-⺙⺛-⻳⼀-⿕⿰-⿻　-〾ぁ-ゖ゙-ヿㄅ-ㄯㄱ-ㆎ㆐-㇣ㇰ-㈞㈠-㉇㉐-㋿㌀-䶿一-꓆ꥠ-ꥼ가-힣豈-﫿︐-︙︰-﹫！-｠￠-￦🀄🃏🆎🆑-🆚🈀-🈂🈐-🈻🉀-🉈🉐-🉑🌀-🙏🚀-🛿🤀-🧿🩰-🫿𠀀-𿿽]/u;

/**
 * @param {string} line
 * @returns {number}
 */
function displayWidth(line) {
  let width = 0;
  for (const character of line) {
    width += FULL_WIDTH_RE.test(character) ? 2 : 1;
  }
  return width;
}

/**
 * @param {import("markdownlint").MicromarkToken[]} tokens
 * @param {Set<number>} excludedLines
 */
function collectExcludedLines(tokens, excludedLines) {
  for (const token of tokens || []) {
    if ([ "codeFenced", "codeIndented", "table" ].includes(token.type)) {
      for (let lineNumber = token.startLine; lineNumber <= token.endLine; lineNumber++) {
        excludedLines.add(lineNumber);
      }
    }
    collectExcludedLines(token.children || [], excludedLines);
  }
}

/** @type {import("markdownlint").Rule} */
export default {
  names: [ "WX013", "line-width" ],
  description: "Line display width",
  tags: [ "line_length" ],
  parser: "micromark",
  function: function WX013(params, onError) {
    const configuredWidth = Number(params.config?.line_width ?? DEFAULT_LINE_WIDTH);
    if (!Number.isFinite(configuredWidth) || configuredWidth <= 0) {
      return;
    }

    const excludedLines = new Set();
    collectExcludedLines(params.parsers.micromark?.tokens || [], excludedLines);
    for (const [index, line] of params.lines.entries()) {
      const lineNumber = index + 1;
      if (!excludedLines.has(lineNumber) && displayWidth(line) > configuredWidth) {
        onError({ lineNumber, detail: ERROR_DETAIL });
      }
    }
  }
};

