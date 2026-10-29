-- Help channel access. The help channel is hidden from everyone; a member
-- stuck in registration presses "Χρειάζομαι βοήθεια" and is let in with a
-- permission overwrite of their own. Each grant is recorded here so the bot
-- removes exactly the overwrites it added, never ones a moderator set, once
-- the registration is completed, restarted or closed.
create table if not exists help_access (
    user_id    text        primary key,
    granted_at timestamptz not null default now()
);
