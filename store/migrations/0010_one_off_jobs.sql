-- Jobs the bot runs once, ever, such as converting messages posted before a
-- change. A row means the job finished.
create table one_off_jobs (
    name    text primary key,
    done_at integer not null
);
