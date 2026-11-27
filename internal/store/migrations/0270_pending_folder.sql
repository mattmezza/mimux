-- Keep the IMAP folder and UID together until the remote move succeeds.
ALTER TABLE messages ADD COLUMN pending_folder_id INTEGER REFERENCES folders(id) ON DELETE SET NULL;
CREATE INDEX idx_messages_pending_folder ON messages(pending_folder_id) WHERE pending_folder_id IS NOT NULL;
CREATE INDEX idx_messages_visible_folder ON messages(COALESCE(pending_folder_id, folder_id), date);
CREATE INDEX idx_messages_legacy_move ON messages(folder_id) WHERE pending_move = 1;
