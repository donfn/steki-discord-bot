-- Course material: every PDF and JPEG members post, backed up to the steki-backups
-- bucket and listed on the material page of the channel it was posted in.
--
-- A row is written as soon as the bot sees the attachment and then moves
-- through two independent steps: the backup (backed_up_at) and the screening
-- (status). The page lists only rows that are backed up, approved and not
-- deleted. Deleting the message on Discord sets deleted_at; the object stays
-- in the bucket.
create table materials (
    attachment_id   text primary key,
    message_id      text not null,
    channel_id      text not null,
    -- The channel the page and sticky belong to. For a thread this is its
    -- parent; for anything else it equals channel_id.
    home_channel_id text not null,
    guild_id        text not null,
    uploader_id     text not null,
    uploader_bot    integer not null default 0,
    filename        text not null,
    -- application/pdf or image/jpeg
    content_type    text not null,
    size            integer not null,
    -- The text of the message the file came with, or the title given in the
    -- upload form, shown on the page under the file name.
    caption         text,
    source_url      text not null,
    object_key      text not null,
    posted_at       integer not null,
    discovered_at   integer not null,
    backed_up_at    integer,
    backup_attempts integer not null default 0,
    backup_error    text,
    -- pending | approved | rejected
    status          text not null default 'pending',
    screened_at     integer,
    screen_note     text,
    deleted_at      integer
);
create index materials_home on materials (home_channel_id, status, deleted_at);
create index materials_message on materials (message_id);
create index materials_backlog on materials (posted_at, attachment_id) where backed_up_at is null or status = 'pending';

-- How far the history scan has got in each channel. Messages from high_id
-- down to low_id have been scanned without gaps; done means low_id reached the
-- channel's first message. A later pass fills the gap above high_id (messages
-- posted while the bot was offline) before carrying on below low_id.
create table material_scan (
    channel_id text primary key,
    high_id    text,
    low_id     text,
    done       integer not null default 0,
    updated_at integer not null
);

-- The "Προβολή υλικού" message currently kept at the bottom of each course
-- channel, so a restart replaces the old one rather than leaving duplicates.
create table stickies (
    channel_id text primary key,
    message_id text not null,
    posted_at  integer not null
);
