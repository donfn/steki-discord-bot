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
