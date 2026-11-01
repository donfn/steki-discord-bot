-- What members keep for themselves on the material page, per file: the study
-- marks (studied, their own title, notes) as kind 'study', and what they drew
-- on the pages as kind 'ink'. data is the page's own JSON; the latest save
-- replaces it.
create table study_notes (
    user_id       text not null,
    attachment_id text not null,
    kind          text not null,
    data          text not null,
    updated_at    integer not null,
    primary key (user_id, attachment_id, kind)
);
