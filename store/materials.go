package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Screening states of a material. A material is listed on the page only once
// it is approved.
const (
	MaterialPending  = "pending"
	MaterialApproved = "approved"
	MaterialRejected = "rejected"
)

// Material is one PDF or JPEG attachment posted in the guild.
type Material struct {
	AttachmentID  string
	MessageID     string
	ChannelID     string
	HomeChannelID string
	GuildID       string
	UploaderID    string
	UploaderBot   bool
	Filename      string
	ContentType   string
	Size          int64
	Caption       string
	SourceURL     string
	ObjectKey     string
	PostedAt      time.Time
	DiscoveredAt  time.Time

	BackedUpAt     *time.Time
	BackupAttempts int
	BackupError    *string

	Status     string
	ScreenedAt *time.Time
	ScreenNote *string

	DeletedAt *time.Time
}

const materialColumns = `attachment_id, message_id, channel_id, home_channel_id, guild_id,
	uploader_id, uploader_bot, filename, content_type, size, caption, source_url, object_key, posted_at, discovered_at,
	backed_up_at, backup_attempts, backup_error, status, screened_at, screen_note, deleted_at`

func scanMaterial(row scanner) (*Material, error) {
	var m Material
	var caption sql.NullString
	err := row.Scan(&m.AttachmentID, &m.MessageID, &m.ChannelID, &m.HomeChannelID, &m.GuildID,
		&m.UploaderID, &m.UploaderBot, &m.Filename, &m.ContentType, &m.Size, &caption, &m.SourceURL, &m.ObjectKey,
		timeDest{&m.PostedAt}, timeDest{&m.DiscoveredAt}, nullTimeDest{&m.BackedUpAt},
		&m.BackupAttempts, &m.BackupError, &m.Status, nullTimeDest{&m.ScreenedAt}, &m.ScreenNote,
		nullTimeDest{&m.DeletedAt})
	if err != nil {
		return nil, noRows(err)
	}
	m.Caption = caption.String
	return &m, nil
}

func collectMaterials(rows *sql.Rows) ([]*Material, error) {
	defer rows.Close()
	var out []*Material
	for rows.Next() {
		m, err := scanMaterial(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMaterial records a newly seen attachment, pending backup and screening.
// It reports false when the attachment was already known, which makes the
// history scan and the live path safe to overlap.
func (s *Store) AddMaterial(ctx context.Context, m *Material) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		insert into materials (attachment_id, message_id, channel_id, home_channel_id, guild_id,
			uploader_id, uploader_bot, filename, content_type, size, caption, source_url,
			object_key, posted_at, discovered_at, status)
		values (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16)
		on conflict (attachment_id) do nothing`,
		m.AttachmentID, m.MessageID, m.ChannelID, m.HomeChannelID, m.GuildID, m.UploaderID,
		m.UploaderBot, m.Filename, m.ContentType, m.Size, nullStr(m.Caption), m.SourceURL,
		m.ObjectKey, ms(m.PostedAt), ms(time.Now()), MaterialPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Cursor pages through a backlog in (posted_at, attachment_id) order. The
// zero Cursor starts at the beginning; Next moves it past the last row read.
type Cursor struct {
	postedAt int64
	id       string
}

// Next returns the cursor that follows m.
func (Cursor) Next(m *Material) Cursor { return Cursor{postedAt: ms(m.PostedAt), id: m.AttachmentID} }

// MaterialBacklog lists materials still to be copied to the bucket that have
// failed fewer than maxAttempts times, oldest first, after cur. Deleted
// materials are included: the bucket is a backup and keeps everything that
// was posted.
func (s *Store) MaterialBacklog(ctx context.Context, maxAttempts int, cur Cursor, limit int) ([]*Material, error) {
	rows, err := s.db.QueryContext(ctx, `select `+materialColumns+` from materials
		where backed_up_at is null and backup_attempts < ?1
			and (posted_at, attachment_id) > (?2, ?3)
		order by posted_at, attachment_id limit ?4`, maxAttempts, cur.postedAt, cur.id, limit)
	if err != nil {
		return nil, err
	}
	return collectMaterials(rows)
}

// MarkBackedUp records a successful copy to the bucket.
func (s *Store) MarkBackedUp(ctx context.Context, attachmentID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `update materials
		set backed_up_at = ?2, backup_error = null where attachment_id = ?1`,
		attachmentID, ms(at))
	return err
}

// MarkBackupFailed counts a failed copy and keeps its reason.
func (s *Store) MarkBackupFailed(ctx context.Context, attachmentID, reason string) error {
	_, err := s.db.ExecContext(ctx, `update materials
		set backup_attempts = backup_attempts + 1, backup_error = ?2 where attachment_id = ?1`,
		attachmentID, reason)
	return err
}

// SetMaterialSource replaces an attachment's download URL. Discord's CDN
// links are signed and expire, so a retry fetches a fresh one.
func (s *Store) SetMaterialSource(ctx context.Context, attachmentID, url string) error {
	_, err := s.db.ExecContext(ctx,
		`update materials set source_url = ?2 where attachment_id = ?1`, attachmentID, url)
	return err
}

// ScreeningBacklog lists backed-up materials still waiting for a screening
// verdict, oldest first, after cur.
func (s *Store) ScreeningBacklog(ctx context.Context, cur Cursor, limit int) ([]*Material, error) {
	rows, err := s.db.QueryContext(ctx, `select `+materialColumns+` from materials
		where status = ?1 and backed_up_at is not null and deleted_at is null
			and (posted_at, attachment_id) > (?2, ?3)
		order by posted_at, attachment_id limit ?4`, MaterialPending, cur.postedAt, cur.id, limit)
	if err != nil {
		return nil, err
	}
	return collectMaterials(rows)
}

// SetScreening records a screening verdict.
func (s *Store) SetScreening(ctx context.Context, attachmentID, status, note string) error {
	_, err := s.db.ExecContext(ctx, `update materials
		set status = ?2, screen_note = ?3, screened_at = ?4 where attachment_id = ?1`,
		attachmentID, status, nullStr(note), ms(time.Now()))
	return err
}

// DeleteMaterialsByMessage hides every material of the given messages from
// the page. The bucket objects are kept. It returns the home channels that
// had a matching material, whose stickies need a new count.
func (s *Store) DeleteMaterialsByMessage(ctx context.Context, messageIDs ...string) ([]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	args := []any{ms(time.Now())}
	for _, id := range messageIDs {
		args = append(args, id)
	}
	return s.deleteMaterials(ctx, `message_id in (`+placeholders(len(messageIDs))+`)`, args)
}

// DeleteMaterialsRemovedFrom hides the materials of a message that are no
// longer among keep, which is what an edit that removes an attachment leaves.
func (s *Store) DeleteMaterialsRemovedFrom(ctx context.Context, messageID string, keep []string) ([]string, error) {
	args := []any{ms(time.Now()), messageID}
	cond := `message_id = ?`
	if len(keep) > 0 {
		for _, id := range keep {
			args = append(args, id)
		}
		cond += ` and attachment_id not in (` + placeholders(len(keep)) + `)`
	}
	return s.deleteMaterials(ctx, cond, args)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// deleteMaterials soft-deletes the live rows matching cond, whose positional
// parameters follow the deletion time in args.
func (s *Store) deleteMaterials(ctx context.Context, cond string, args []any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `update materials set deleted_at = ?
		where deleted_at is null and `+cond+`
		returning home_channel_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// visibleMaterial is the condition for a material the page may show.
const visibleMaterial = `status = 'approved' and backed_up_at is not null and deleted_at is null`

// ChannelMaterials lists what the page of a course channel shows, newest
// first.
func (s *Store) ChannelMaterials(ctx context.Context, homeChannelID string) ([]*Material, error) {
	rows, err := s.db.QueryContext(ctx, `select `+materialColumns+` from materials
		where home_channel_id = ?1 and `+visibleMaterial+`
		order by posted_at desc`, homeChannelID)
	if err != nil {
		return nil, err
	}
	return collectMaterials(rows)
}

// ChannelMaterial returns one material the page of homeChannelID may serve,
// or ErrNotFound, so a token for one channel cannot fetch another's files.
func (s *Store) ChannelMaterial(ctx context.Context, homeChannelID, attachmentID string) (*Material, error) {
	return scanMaterial(s.db.QueryRowContext(ctx, `select `+materialColumns+` from materials
		where attachment_id = ?1 and home_channel_id = ?2 and `+visibleMaterial,
		attachmentID, homeChannelID))
}

// CountMemberMaterials counts the listed files members (not bots) posted in a
// course channel, for its sticky message.
func (s *Store) CountMemberMaterials(ctx context.Context, homeChannelID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from materials
		where home_channel_id = ?1 and uploader_bot = 0 and `+visibleMaterial,
		homeChannelID).Scan(&n)
	return n, err
}

// MaterialStats summarises the backup for /bot stats.
type MaterialStats struct {
	Total, BackedUp, Failed, Listed int
}

// MaterialStats counts materials by state. Failed are the ones that ran out
// of backup attempts.
func (s *Store) MaterialStats(ctx context.Context, maxAttempts int) (MaterialStats, error) {
	var st MaterialStats
	err := s.db.QueryRowContext(ctx, `select
			count(*),
			count(backed_up_at),
			coalesce(sum(backed_up_at is null and backup_attempts >= ?1), 0),
			coalesce(sum(`+visibleMaterial+`), 0)
		from materials`, maxAttempts).Scan(&st.Total, &st.BackedUp, &st.Failed, &st.Listed)
	return st, err
}

// ScanState is how far the history scan has got in one channel: everything
// from HighID down to LowID has been read, and Done means LowID is the
// channel's first message.
type ScanState struct {
	HighID, LowID string
	Done          bool
}

// MaterialScan returns a channel's scan state; a channel never scanned has
// the zero state.
func (s *Store) MaterialScan(ctx context.Context, channelID string) (ScanState, error) {
	var st ScanState
	var high, low sql.NullString
	err := s.db.QueryRowContext(ctx,
		`select high_id, low_id, done from material_scan where channel_id = ?1`, channelID).
		Scan(&high, &low, &st.Done)
	if errors.Is(err, sql.ErrNoRows) {
		return ScanState{}, nil
	}
	st.HighID, st.LowID = high.String, low.String
	return st, err
}

// SetMaterialScan saves a channel's scan state.
func (s *Store) SetMaterialScan(ctx context.Context, channelID string, st ScanState) error {
	_, err := s.db.ExecContext(ctx, `
		insert into material_scan (channel_id, high_id, low_id, done, updated_at)
		values (?1, ?2, ?3, ?4, ?5)
		on conflict (channel_id) do update set high_id = excluded.high_id,
			low_id = excluded.low_id, done = excluded.done, updated_at = excluded.updated_at`,
		channelID, nullStr(st.HighID), nullStr(st.LowID), st.Done, ms(time.Now()))
	return err
}

// Sticky returns the message currently kept at the bottom of a channel, or
// ErrNotFound.
func (s *Store) Sticky(ctx context.Context, channelID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`select message_id from stickies where channel_id = ?1`, channelID).Scan(&id)
	return id, noRows(err)
}

// DeleteSticky forgets the sticky of a channel that no longer has one.
func (s *Store) DeleteSticky(ctx context.Context, channelID string) error {
	_, err := s.db.ExecContext(ctx, `delete from stickies where channel_id = ?1`, channelID)
	return err
}

// SetSticky records the message now kept at the bottom of a channel.
func (s *Store) SetSticky(ctx context.Context, channelID, messageID string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into stickies (channel_id, message_id, posted_at) values (?1, ?2, ?3)
		on conflict (channel_id) do update set message_id = excluded.message_id,
			posted_at = excluded.posted_at`,
		channelID, messageID, ms(time.Now()))
	return err
}
