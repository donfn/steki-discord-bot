-- The messages moderators posted to explain the MCP server. Each carries a
-- button to the setup page whose link is replaced every half hour, so the bot
-- has to find the messages again to edit them, across restarts.
create table mcp_announcements (
    message_id text primary key,
    channel_id text not null,
    posted_at  integer not null
);
