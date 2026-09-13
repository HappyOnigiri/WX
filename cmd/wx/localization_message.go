package main

import (
	"strings"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

// localization_message.go は daemon・setup・設定検証が出す固定メッセージだけを訳す。
// path・key・外部コマンドのエラー文字列は原文のまま残す。

func localizeDaemonText(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	return applyTranslations(text, []translation{
		{"cancelled the pending stop of", "保留中の停止要求をキャンセルしました:"},
		{"stop was already requested; waiting for the daemon to exit", "停止要求は送信済みです。daemon の終了を待っています"},
		{"run wx daemon install to register the LaunchAgent first", "LaunchAgent を登録するには wx daemon install を実行してください"},
		{"stop it with wx daemon stop and start it again with wx daemon start", "wx daemon stop で停止し、wx daemon start で再起動してください"},
		{"the daemon is not managed by launchd", "daemon は launchd に管理されていません"},
		{"accepted the restart request but was not replaced within", "再起動要求を受理しましたが、次の daemon に置き換わらないまま期限を超えました"},
		{"accepted the stop request but did not exit within", "停止要求を受理しましたが、終了しないまま期限を超えました"},
		{"launchd was asked to start", "launchd に起動を依頼しました"},
		{"but no daemon answered", "が daemon から応答を受け取れませんでした"},
	})
}

// localizeErrorText は設定検証などの固定エラーだけを翻訳し、path・key・外部エラーの値は残す。
func localizeErrorText(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	return strings.ReplaceAll(text, "language must be en or ja", i18n.New(string(lang)).Localize("config.language.invalid", nil))
}

func localizeError(err error, lang i18n.Language) string {
	if err == nil {
		return ""
	}
	return localizeErrorText(err.Error(), lang)
}

func localizeSetupError(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	return applyTranslations(text, []translation{
		{"setup item ", "setup 項目 "},
		{" does not offer action ", " は操作 "},
		{"; available actions: ", "。利用可能な操作: "},
		{" action manual requires --value", " の manual 操作には --value が必要です"},
	})
}
