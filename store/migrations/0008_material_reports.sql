-- Reports members file against a material from its page. A report is checked
-- by the AI pipeline straight away: a valid one pulls the material off the page
-- (materials.status becomes 'reported') and posts a card for the admins, who
-- either remove the file for good or put it back.
--
-- status is checking while the pipeline looks at it, then valid or invalid.
-- A report whose check could not run is deleted again, so it does not count
-- towards the weekly limit.
create table material_reports (
    id              integer primary key autoincrement,
    attachment_id   text not null,
    reporter_id     text not null,
    reason          text not null,
    created_at      integer not null,
    status          text not null default 'checking',
    verdict_note    text,
    card_message_id text
);
-- A member reports a file once.
create unique index material_reports_once on material_reports (reporter_id, attachment_id);
create index material_reports_week on material_reports (reporter_id, created_at);
create index material_reports_card on material_reports (status) where status = 'valid' and card_message_id is null;
