-- v26 (compatible with v19+): Store the MXID of the event that replaces (i.e. edits) this event
ALTER TABLE message ADD COLUMN replaced_by_mxid TEXT NOT NULL DEFAULT '';
