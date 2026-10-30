-- The email-history reconciliation is finished. The evidence it gathered from
-- the moderator log channel has already been applied to verified_emails and
-- legacy_email_hashes, which are what the bot reads, so the scan bookkeeping
-- and the raw evidence are no longer needed.
drop table if exists legacy_log_evidence;
drop table if exists log_scan_state;
