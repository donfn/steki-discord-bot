-- Schema for the Steki registration bot (SQLite).
--
-- Timestamps are integer Unix milliseconds, UTC. Emails are stored in
-- plaintext by design: the old unsalted SHA-256 hashes gave no real protection
-- while making it impossible for moderators to answer "which account used this
-- address?". Tables keyed by text use WITHOUT ROWID, which stores each row once
-- in the primary-key b-tree instead of twice.

-- Addresses that have been used to verify an account. One address, one account,
-- forever. user_id is nullable because rows recovered from the historical log
-- channel may name an address whose owner could not be determined.
create table verified_emails (
    email       text primary key,
    user_id     text,
    domain      text    not null,
    source      text    not null default 'live'
        check (source in ('live', 'discord_log', 'moderator')),
    verified_at integer not null default (cast(unixepoch('subsec') * 1000 as integer))
) without rowid;

create index verified_emails_user_id_idx on verified_emails (user_id);

-- Hashes imported from the old Mongo `usedEmails` collection for which no
-- plaintext address was recovered. They still block re-registration: the dedup
-- check tries plaintext first, then sha256(input) against this table.
create table legacy_email_hashes (
    sha256      text primary key,
    user_id     text,
    source      text    not null,
    imported_at integer not null default (cast(unixepoch('subsec') * 1000 as integer))
) without rowid;

-- Every member the bot has seen. Survives leaving and rejoining, which is what
-- makes join_count and kicked_count meaningful to a moderator.
create table members (
    user_id        text primary key,
    username       text    not null default '',
    first_seen_at  integer not null default (cast(unixepoch('subsec') * 1000 as integer)),
    last_join_at   integer not null default (cast(unixepoch('subsec') * 1000 as integer)),
    join_count     integer not null default 1,
    left_at        integer,
    verified_at    integer,
    verified_email text references verified_emails (email) on delete set null,
    kicked_count   integer not null default 0
) without rowid;

-- One row per registration attempt. At most one may be open per user at a time,
-- enforced by the partial unique index below.
create table registrations (
    id               text    primary key,
    user_id          text    not null references members (user_id) on delete cascade,
    guild_id         text    not null,
    channel_id       text,
    category_id      text,
    step             text    not null default 'awaiting_email'
        check (step in ('awaiting_email', 'awaiting_code', 'done')),
    email            text,
    code             text,
    code_expires_at  integer,
    code_sent_count  integer not null default 0,
    failed_attempts  integer not null default 0,
    last_activity_at integer not null default (cast(unixepoch('subsec') * 1000 as integer)),
    nudged_at        integer,
    created_at       integer not null default (cast(unixepoch('subsec') * 1000 as integer)),
    completed_at     integer,
    outcome          text
        check (outcome is null or outcome in
               ('verified', 'abandoned_kicked', 'left', 'restarted', 'moderator'))
) without rowid;

-- The invariant that keeps duplicate register channels from ever existing.
create unique index registrations_one_open_per_user
    on registrations (user_id) where outcome is null;

create index registrations_open_activity_idx
    on registrations (last_activity_at) where outcome is null;

create index registrations_channel_idx on registrations (channel_id);

-- Append-only audit trail. This is what /reg history reads, and the reason a
-- moderator can reconstruct what happened to a member without scrolling Discord.
-- detail is a JSON object.
create table registration_events (
    id         integer primary key autoincrement,
    user_id    text    not null,
    reg_id     text,
    kind       text    not null,
    detail     text    not null default '{}',
    actor_id   text,
    created_at integer not null default (cast(unixepoch('subsec') * 1000 as integer))
);

create index registration_events_user_idx
    on registration_events (user_id, created_at desc);

create index registration_events_kind_idx
    on registration_events (kind, created_at desc);

-- Media guard: a member who posts an attachment or a link twice within an hour
-- is timed out until they solve a captcha. Hijacked accounts spray scam images
-- and links across every channel within minutes; real members rarely do.
-- One row per member who has ever posted an attachment or a link.
create table media_guard (
    user_id        text primary key,
    guild_id       text    not null,
    -- Start of the current one-hour window, or null when no window is open.
    window_start   integer,
    -- Set after a solved captcha; flagged posts are ignored until then.
    exempt_until   integer,
    -- The outstanding verification link (its JWT ID) and the DM carrying it.
    -- token_id is null once the link has been used or superseded.
    token_id       text,
    restricted_at  integer,
    dm_channel_id  text,
    dm_message_id  text,
    updated_at     integer not null default (cast(unixepoch('subsec') * 1000 as integer))
) without rowid;

-- Log reconciliation: on boot the bot reads its own moderator log channel,
-- newest to oldest, and records every line that ties an email hash to a member
-- or to a plaintext address. That evidence is what binds the hashes imported
-- from the old Mongo database to their owners.
--
-- How far the scan of a channel has got. oldest_id moves back one page at a
-- time until the start of the channel is reached and complete is set;
-- newest_id lets the next boot pick up lines posted since.
create table log_scan_state (
    channel_id text    primary key,
    oldest_id  text,
    newest_id  text,
    complete   integer not null default 0,
    scanned    integer not null default 0,
    updated_at integer not null default (cast(unixepoch('subsec') * 1000 as integer))
) without rowid;

-- One row per hash-bearing log line. kind is 'submitted' (a member sent an
-- address with this hash; user_id is that member) or 'in_use' (the bot refused
-- an address as already registered; email is it, when it hashes to sha256).
create table legacy_log_evidence (
    message_id text    not null,
    sha256     text    not null,
    kind       text    not null check (kind in ('submitted', 'in_use')),
    user_id    text,
    email      text,
    posted_at  integer not null,
    primary key (message_id, sha256, kind)
) without rowid;

create index legacy_log_evidence_sha256_idx on legacy_log_evidence (sha256);

-- Help channel access. The help channel is hidden from everyone; a member
-- stuck in registration presses "Χρειάζομαι βοήθεια" and is let in with a
-- permission overwrite of their own. Each grant is recorded here so the bot
-- removes exactly the overwrites it added, never ones a moderator set, once
-- the registration is completed, restarted or closed.
create table help_access (
    user_id    text    primary key,
    granted_at integer not null default (cast(unixepoch('subsec') * 1000 as integer))
) without rowid;
