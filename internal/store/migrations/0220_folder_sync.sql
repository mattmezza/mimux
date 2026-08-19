-- 0220: which folders the steady-state loop keeps re-reading.
--
-- Until now the steady state was the inbox and nothing else: everything else
-- was only re-read when the IMAP connection was re-established, so mail sent
-- from your phone took until the next reconnect to appear in Sent. sync = 1
-- means "visit this folder every cycle".
--
-- The default set is inbox + sent + drafts: the three folders you write to from
-- another client and expect to see here. Everything else is opt-in per account
-- in Settings → Syncing, because each extra folder costs a SELECT + a FETCH per
-- cycle. The inbox is not really optional — SyncedFolders ORs it back in — but
-- it carries the flag anyway so the checkbox list has nothing special to say.
ALTER TABLE folders ADD COLUMN sync INTEGER NOT NULL DEFAULT 0;
UPDATE folders SET sync = 1 WHERE special_use IN ('inbox', 'sent', 'drafts');
