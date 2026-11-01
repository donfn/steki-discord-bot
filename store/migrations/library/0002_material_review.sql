-- Images are checked by a vision model before they reach the bucket. One it
-- flags (not course material, people in it, explicit) gets status 'review':
-- unlisted and not backed up until an admin approves it from the card the bot
-- posts in the log channel.
--
-- vision_at is when a file passed the check, so a rescan only visits images
-- that never had one. review_message_id is that card, kept so a restart does
-- not post it twice.
alter table materials add column vision_at integer;
alter table materials add column review_message_id text;
create index materials_review on materials (status) where status = 'review';
