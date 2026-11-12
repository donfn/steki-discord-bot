-- The intelligence feed: images and links seen in a spam attack from a hijacked
-- account. Sending one again, from anyone, trips the spam protection at once.
-- An admin removes an entry that turns out to be a mistake.
--   kind  'media' (key: SHA-256 of the file) or 'link' (key: the normalised link)
--   label what an admin recognises it by: the file name or the link
create table intel (
    id          integer primary key autoincrement,
    kind        text    not null check (kind in ('media', 'link')),
    key         text    not null,
    label       text    not null,
    user_id     text    not null,
    hits        integer not null default 0,
    added_at    integer not null default (cast(unixepoch('subsec') * 1000 as integer)),
    last_hit_at integer,
    unique (kind, key)
);
