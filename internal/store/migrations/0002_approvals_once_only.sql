-- An approval whose answer covers one call only (a browser step): "session"
-- and "always" are refused, so it never becomes a grant (docs/browser.md).
ALTER TABLE approvals ADD COLUMN once_only INTEGER NOT NULL DEFAULT 0;
