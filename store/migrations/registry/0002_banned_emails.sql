-- Addresses that may not register again. Keyed by the SHA-256 of the
-- normalized address, so an imported legacy hash (no plaintext known) can be
-- banned the same way as a recorded address; email is kept when it is known,
-- for display and for lifting a ban by address.
create table banned_emails (
    hash      text primary key,
    email     text,
    user_id   text,
    reason    text,
    banned_by text,
    banned_at integer not null
);
create index banned_emails_user on banned_emails (user_id);
