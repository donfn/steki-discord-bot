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
