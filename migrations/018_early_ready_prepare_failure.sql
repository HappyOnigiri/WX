-- early ready の後に失敗した準備は隔離せず貸出を続けるため、どの区間で止まったかを slot に残す。
-- failure_code と failure_detail_path だけでは、エージェントへ何の準備が落ちたかを伝えられない。
ALTER TABLE slots ADD COLUMN failure_phase TEXT;
-- 準備失敗の案内は最初の user-prompt-submit で 1 回だけ出す。
-- daemon のメモリで持つと再起動のたびに同じ案内が再送されるため、session 行に残す。
ALTER TABLE sessions ADD COLUMN prepare_notice_delivered_at TEXT;
