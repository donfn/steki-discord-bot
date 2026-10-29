-- Log reconciliation: on boot the bot reads its own moderator log channel,
-- newest to oldest, and records every line that ties an email hash to a member
-- or to a plaintext address. That evidence is what binds the hashes imported
-- from the old Mongo database to their owners.

-- How far the scan of a channel has got. oldest_id moves back one page at a
-- time until the start of the channel is reached and complete is set;
-- newest_id lets the next boot pick up lines posted since.
create table if not exists log_scan_state (
    channel_id text primary key,
    oldest_id  text,
    newest_id  text,
    complete   boolean     not null default false,
    scanned    bigint      not null default 0,
    updated_at timestamptz not null default now()
);

-- One row per hash-bearing log line. kind is 'submitted' (a member sent an
-- address with this hash; user_id is that member) or 'in_use' (the bot refused
-- an address as already registered; email is it, when it hashes to sha256).
create table if not exists legacy_log_evidence (
    message_id text        not null,
    sha256     text        not null,
    kind       text        not null,
    user_id    text,
    email      text,
    posted_at  timestamptz not null,
    primary key (message_id, sha256, kind),
    constraint legacy_log_evidence_kind_check check (kind in ('submitted', 'in_use'))
);

create index if not exists legacy_log_evidence_sha256_idx on legacy_log_evidence (sha256);
