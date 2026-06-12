-- v25 (compatible with v19+): Store which Matrix message is being replied to
ALTER TABLE message ADD COLUMN reply_to_mxid TEXT NOT NULL DEFAULT '';
