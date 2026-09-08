-- 失敗した job の具体的な失敗理由を残す。error_code だけでは
-- 「準備に失敗」までしか説明できず、doctor が原因を報告できない。
ALTER TABLE jobs ADD COLUMN error_message TEXT;
