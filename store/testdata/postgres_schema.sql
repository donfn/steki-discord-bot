-- Initial schema for the Steki registration bot.
--
-- Replaces the previous MongoDB collections `activeRegistrations` and
-- `usedEmails`. Emails are stored in plaintext by design: the old unsalted
-- SHA-256 hashes gave no real protection while making it impossible for
-- moderators to answer "which account used this address?".

-- Addresses that have been used to verify an account. One address, one account,
-- forever. user_id is nullable because rows recovered from the historical log
-- channel may name an address whose owner could not be determined.
create table if not exists verified_emails (
    email       text primary key,
    user_id     text,
    domain      text        not null,
    source      text        not null default 'live',
    verified_at timestamptz not null default now(),
    constraint verified_emails_source_check
        check (source in ('live', 'discord_log', 'moderator'))
);

create index if not exists verified_emails_user_id_idx on verified_emails (user_id);

-- Hashes imported from the old Mongo `usedEmails` collection for which no
-- plaintext address was recovered. They still block re-registration: the dedup
-- check tries plaintext first, then sha256(input) against this table.
create table if not exists legacy_email_hashes (
    sha256      text primary key,
    user_id     text,
    source      text        not null,
    imported_at timestamptz not null default now()
);

-- Every member the bot has seen. Survives leaving and rejoining, which is what
-- makes join_count and kicked_count meaningful to a moderator.
create table if not exists members (
    user_id        text primary key,
    username       text        not null default '',
    first_seen_at  timestamptz not null default now(),
    last_join_at   timestamptz not null default now(),
    join_count     int         not null default 1,
    left_at        timestamptz,
    verified_at    timestamptz,
    verified_email text references verified_emails (email) on delete set null,
    kicked_count   int         not null default 0
);

-- One row per registration attempt. At most one may be open per user at a time,
-- enforced by the partial unique index below.
create table if not exists registrations (
    id               uuid        primary key default gen_random_uuid(),
    user_id          text        not null references members (user_id) on delete cascade,
    guild_id         text        not null,
    channel_id       text,
    category_id      text,
    step             text        not null default 'awaiting_email',
    email            text,
    code             text,
    code_expires_at  timestamptz,
    code_sent_count  int         not null default 0,
    failed_attempts  int         not null default 0,
    last_activity_at timestamptz not null default now(),
    nudged_at        timestamptz,
    created_at       timestamptz not null default now(),
    completed_at     timestamptz,
    outcome          text,
    constraint registrations_step_check
        check (step in ('awaiting_email', 'awaiting_code', 'done')),
    constraint registrations_outcome_check
        check (outcome is null or outcome in
               ('verified', 'abandoned_kicked', 'left', 'restarted', 'moderator'))
);

-- The invariant that keeps duplicate register channels from ever existing.
create unique index if not exists registrations_one_open_per_user
    on registrations (user_id) where outcome is null;

create index if not exists registrations_open_activity_idx
    on registrations (last_activity_at) where outcome is null;

create index if not exists registrations_channel_idx on registrations (channel_id);

-- Append-only audit trail. This is what /reg history reads, and the reason a
-- moderator can reconstruct what happened to a member without scrolling Discord.
create table if not exists registration_events (
    id         bigserial   primary key,
    user_id    text        not null,
    reg_id     uuid,
    kind       text        not null,
    detail     jsonb       not null default '{}'::jsonb,
    actor_id   text,
    created_at timestamptz not null default now()
);

create index if not exists registration_events_user_idx
    on registration_events (user_id, created_at desc);

create index if not exists registration_events_kind_idx
    on registration_events (kind, created_at desc);
-- Media guard: a member who posts an attachment or a link twice within an hour
-- is timed out until they solve a captcha. Hijacked accounts spray scam images
-- and links across every channel within minutes; real members rarely do.

-- One row per member who has ever posted an attachment or a link.
create table if not exists media_guard (
    user_id        text primary key,
    guild_id       text        not null,
    -- Start of the current one-hour window, or null when no window is open.
    window_start   timestamptz,
    -- Set after a solved captcha; flagged posts are ignored until then.
    exempt_until   timestamptz,
    -- The outstanding verification link (its JWT ID) and the DM carrying it.
    -- token_id is null once the link has been used or superseded.
    token_id       text,
    restricted_at  timestamptz,
    dm_channel_id  text,
    dm_message_id  text,
    updated_at     timestamptz not null default now()
);
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
-- Help channel access. The help channel is hidden from everyone; a member
-- stuck in registration presses "Χρειάζομαι βοήθεια" and is let in with a
-- permission overwrite of their own. Each grant is recorded here so the bot
-- removes exactly the overwrites it added, never ones a moderator set, once
-- the registration is completed, restarted or closed.
create table if not exists help_access (
    user_id    text        primary key,
    granted_at timestamptz not null default now()
);
