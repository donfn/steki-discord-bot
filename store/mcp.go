package store

import (
	"context"
	"time"
)

// MCPAnnouncement is a message explaining the MCP server, whose button link
// the bot keeps fresh.
type MCPAnnouncement struct {
	ChannelID string
	MessageID string
}

// AddMCPAnnouncement records a posted announcement.
func (s *Store) AddMCPAnnouncement(ctx context.Context, channelID, messageID string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into mcp_announcements (message_id, channel_id, posted_at) values (?1, ?2, ?3)
		on conflict (message_id) do nothing`, messageID, channelID, ms(time.Now()))
	return err
}

// MCPAnnouncements lists the announcements, oldest first.
func (s *Store) MCPAnnouncements(ctx context.Context) ([]MCPAnnouncement, error) {
	rows, err := s.db.QueryContext(ctx,
		`select channel_id, message_id from mcp_announcements order by posted_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPAnnouncement
	for rows.Next() {
		var a MCPAnnouncement
		if err := rows.Scan(&a.ChannelID, &a.MessageID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteMCPAnnouncement forgets an announcement whose message is gone.
func (s *Store) DeleteMCPAnnouncement(ctx context.Context, messageID string) error {
	_, err := s.db.ExecContext(ctx, `delete from mcp_announcements where message_id = ?1`, messageID)
	return err
}
