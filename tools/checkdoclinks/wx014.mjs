// @ts-check

// 文書から実装・テスト・スクリプトへの案内が、ファイル移動やリネームで宛先を失うのを検出する。
// micromarkが認識したリンクだけを見るため、code blockとcode spanの中は対象にならない。

import { existsSync } from "node:fs";
import { dirname, resolve } from "node:path";

const DESTINATION_TYPES = [ "resourceDestinationString", "definitionDestinationString" ];
const SCHEME_RE = /^[a-zA-Z][a-zA-Z0-9+.-]*:/;

/**
 * リンク先のうち、リポジトリ内の相対ファイルパスを指すものだけを返す。
 * scheme付きURL・protocol相対・root絶対・アンカー単独は対象外とする。
 * @param {string} destination
 * @returns {string | null}
 */
function relativeFilePath(destination) {
  const target = destination.trim();
  if (target === "" || target.startsWith("#") || target.startsWith("/") || SCHEME_RE.test(target)) {
    return null;
  }
  // fragmentは切り離してファイル部分だけを検査する。見出しIDの存在は検査しない。
  const [ withoutFragment ] = target.split("#");
  if (withoutFragment === "") {
    return null;
  }
  try {
    return decodeURIComponent(withoutFragment);
  } catch {
    return withoutFragment;
  }
}

/**
 * @param {import("markdownlint").MicromarkToken[]} tokens
 * @param {{ destination: string, lineNumber: number }[]} found
 */
function collectDestinations(tokens, found) {
  for (const token of tokens || []) {
    if (DESTINATION_TYPES.includes(token.type)) {
      found.push({ destination: token.text, lineNumber: token.startLine });
    }
    collectDestinations(token.children || [], found);
  }
}

/** @type {import("markdownlint").Rule} */
export default {
  names: [ "WX014", "doc-file-links" ],
  description: "Relative link target must exist",
  tags: [ "links" ],
  parser: "micromark",
  function: function WX014(params, onError) {
    // 文書自身の位置が分からない入力（strings指定など）では相対解決の基準が無いので何も報告しない。
    const documentPath = resolve(params.name);
    if (!existsSync(documentPath)) {
      return;
    }

    const baseDirectory = dirname(documentPath);
    const found = /** @type {{ destination: string, lineNumber: number }[]} */ ([]);
    collectDestinations(params.parsers.micromark?.tokens || [], found);
    for (const { destination, lineNumber } of found) {
      const target = relativeFilePath(destination);
      if (target !== null && !existsSync(resolve(baseDirectory, target))) {
        onError({
          lineNumber,
          detail: `Relative link target does not exist: ${destination}`,
          context: destination
        });
      }
    }
  }
};
