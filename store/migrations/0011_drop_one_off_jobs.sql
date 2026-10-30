-- One-off passes over messages already in Discord (the MCP announcements) are
-- commands now, run on demand and safe to repeat, so nothing records that they
-- finished.
drop table one_off_jobs;
