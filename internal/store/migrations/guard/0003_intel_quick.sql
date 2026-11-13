-- A media entry is found from a message without downloading anything: by the
-- file's name and size. Only a candidate found this way is downloaded and
-- compared with the entry's hash (its key).
alter table intel add column quick text;
create index intel_quick_idx on intel (quick) where quick is not null;
