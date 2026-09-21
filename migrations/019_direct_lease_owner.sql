-- wx -n で起動した agent が wx new を呼ぶと、随伴 client を持たない path 貸出が残る。
-- 返却の契機になる起動元プロセスを client_pid とは別の列に持ち、
-- wx clear --all の終了要求・wx release の拒否・lease.ttl 掃引の判定を変えない。
ALTER TABLE sessions ADD COLUMN lease_owner_pid INTEGER;
