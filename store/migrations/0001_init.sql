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
