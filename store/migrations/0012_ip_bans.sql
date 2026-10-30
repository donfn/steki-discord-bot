-- Addresses the HTTP shield put behind its challenge page or banned. A row is
-- one block: kind is challenge or ban, until is when it ends. The shield loads
-- the ones still running at start-up, so a redeploy does not lift them, and
-- rows that ended long ago are pruned as new ones come in.
--
-- ip is the address the platform proxy saw; IPv6 addresses are kept as their
-- /64 network (2001:db8:1:2::/64), since one host can hold all of it.
create table ip_bans (
    id         integer primary key autoincrement,
    ip         text not null,
    kind       text not null,
    reason     text not null,
    started_at integer not null,
    until      integer not null
);
create index ip_bans_until on ip_bans (until);
create index ip_bans_ip on ip_bans (ip, started_at);
