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
