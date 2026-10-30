-- When each member of the guild last sent a message, for the ghost role.
--
-- One row per current member (bots excluded). last_message_at is null until a
-- message is seen; joined_at is the floor of the inactivity clock, so a new
-- member who has not posted yet is not a ghost. ghost mirrors whether the role
-- was last applied by the bot. synced_at is when a full member list last
-- confirmed the row; rows it did not confirm belong to members who left.
create table member_activity (
    user_id         text primary key,
    joined_at       integer not null,
    last_message_at integer,
    ghost           integer not null default 0,
    synced_at       integer not null
) without rowid;

create index member_activity_stale on member_activity (ghost, last_message_at);
