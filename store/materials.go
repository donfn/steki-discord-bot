package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// MaterialReview holds a file the content check flagged until an admin
	// decides. It is unlisted and kept out of the bucket meanwhile.
	MaterialReview = "review"
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

	// VisionAt is when the content check passed the file; ReviewMessageID is
	// the admin card of a flagged one.
	VisionAt        *time.Time
	ReviewMessageID *string
}

const materialColumns = `attachment_id, message_id, channel_id, home_channel_id, guild_id,
	uploader_id, uploader_bot, filename, content_type, size, caption, source_url, object_key, posted_at, discovered_at,
	backed_up_at, backup_attempts, backup_error, status, screened_at, screen_note, deleted_at,
	vision_at, review_message_id`

func scanMaterial(row scanner) (*Material, error) {
	var m Material
	var caption sql.NullString
	err := row.Scan(&m.AttachmentID, &m.MessageID, &m.ChannelID, &m.HomeChannelID, &m.GuildID,
		&m.UploaderID, &m.UploaderBot, &m.Filename, &m.ContentType, &m.Size, &caption, &m.SourceURL, &m.ObjectKey,
		timeDest{&m.PostedAt}, timeDest{&m.DiscoveredAt}, nullTimeDest{&m.BackedUpAt},
		&m.BackupAttempts, &m.BackupError, &m.Status, nullTimeDest{&m.ScreenedAt}, &m.ScreenNote,
		nullTimeDest{&m.DeletedAt}, nullTimeDest{&m.VisionAt}, &m.ReviewMessageID)
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
//
// A JPEG in one of courseIDs (the channels the content check covers) is copied
// only once the check approved it, so flagged ones never reach the bucket;
// everything else is copied as soon as it is seen and screened afterwards.
func (s *Store) MaterialBacklog(ctx context.Context, maxAttempts int, cur Cursor, limit int, courseIDs []string) ([]*Material, error) {
	rows, err := s.db.QueryContext(ctx, `select `+materialColumns+` from materials
		where backed_up_at is null and backup_attempts < ?1
			and (posted_at, attachment_id) > (?2, ?3)
			and (status = 'approved' or (status = 'pending'
				and not (content_type = 'image/jpeg' and home_channel_id in (select value from json_each(?5)))))
		order by posted_at, attachment_id limit ?4`, maxAttempts, cur.postedAt, cur.id, limit, jsonList(courseIDs))
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

// ScreeningBacklog lists materials waiting for a screening verdict, oldest
// first, after cur: backed-up ones, plus the JPEGs of courseIDs not yet in the
// bucket. With rescan it also lists the listed JPEGs of courseIDs the content
// check has never seen, which is how files uploaded before it existed get
// checked. Channels outside courseIDs are never part of the content check.
func (s *Store) ScreeningBacklog(ctx context.Context, cur Cursor, limit int, courseIDs []string, rescan bool) ([]*Material, error) {
	rows, err := s.db.QueryContext(ctx, `select `+materialColumns+` from materials
		where deleted_at is null and (posted_at, attachment_id) > (?2, ?3)
			and ((status = ?1 and (backed_up_at is not null
					or (content_type = 'image/jpeg' and home_channel_id in (select value from json_each(?5)))))
				or (?6 and status = 'approved' and content_type = 'image/jpeg'
					and backed_up_at is not null and vision_at is null
					and home_channel_id in (select value from json_each(?5))))
		order by posted_at, attachment_id limit ?4`, MaterialPending, cur.postedAt, cur.id, limit, jsonList(courseIDs), rescan)
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

// MarkVisionChecked records that the content check passed a file.
func (s *Store) MarkVisionChecked(ctx context.Context, attachmentID string) error {
	_, err := s.db.ExecContext(ctx, `update materials set vision_at = ?2 where attachment_id = ?1`,
		attachmentID, ms(time.Now()))
	return err
}

// HoldMaterial takes a flagged file off the page and out of the backup queue
// until an admin decides. The caller has already removed any bucket copy, so
// backed_up_at is cleared.
func (s *Store) HoldMaterial(ctx context.Context, attachmentID, note string) error {
	now := ms(time.Now())
	_, err := s.db.ExecContext(ctx, `update materials
		set status = ?2, screen_note = ?3, screened_at = ?4, vision_at = ?4,
			backed_up_at = null, backup_attempts = 0, backup_error = null
		where attachment_id = ?1`, attachmentID, MaterialReview, nullStr(note), now)
	return err
}

// ReviewsToAnnounce lists held files whose admin card is not posted yet.
func (s *Store) ReviewsToAnnounce(ctx context.Context, limit int) ([]*Material, error) {
	rows, err := s.db.QueryContext(ctx, `select `+materialColumns+` from materials
		where status = ?1 and review_message_id is null and deleted_at is null
		order by posted_at, attachment_id limit ?2`, MaterialReview, limit)
	if err != nil {
		return nil, err
	}
	return collectMaterials(rows)
}

// SetReviewMessage remembers the admin card of a held file.
func (s *Store) SetReviewMessage(ctx context.Context, attachmentID, messageID string) error {
	_, err := s.db.ExecContext(ctx, `update materials set review_message_id = ?2 where attachment_id = ?1`,
		attachmentID, messageID)
	return err
}

// ResolveReview records an admin's decision on a held file: approved files go
// back to the backup queue, rejected ones stay out for good. It reports false
// when the file was no longer awaiting a decision, so two admins clicking at
// once cannot both win. The file's row is returned either way.
func (s *Store) ResolveReview(ctx context.Context, attachmentID string, approve bool, note string) (*Material, bool, error) {
	status := MaterialRejected
	if approve {
		status = MaterialApproved
	}
	res, err := s.db.ExecContext(ctx, `update materials
		set status = ?2, screen_note = ?3, screened_at = ?4
		where attachment_id = ?1 and status = ?5`,
		attachmentID, status, nullStr(note), ms(time.Now()), MaterialReview)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	m, err := scanMaterial(s.db.QueryRowContext(ctx, `select `+materialColumns+` from materials
		where attachment_id = ?1`, attachmentID))
	return m, n > 0, err
}

// jsonList encodes ids as a JSON array, for a json_each lookup.
func jsonList(ids []string) string {
	b, _ := json.Marshal(append([]string{}, ids...))
	return string(b)
}

// MaterialsOfMessage lists the files recorded for a message, deleted or not.
func (s *Store) MaterialsOfMessage(ctx context.Context, messageID string) ([]*Material, error) {
	rows, err := s.db.QueryContext(ctx, `select `+materialColumns+` from materials
		where message_id = ?1 order by attachment_id`, messageID)
	if err != nil {
		return nil, err
	}
	return collectMaterials(rows)
}

// RemoveMaterial is an admin taking a file down for good: it is unlisted,
// leaves the backup queue and cannot come back, since the row stays and the
// history scan never re-adds a known attachment. The caller has already
// removed the bucket copy.
func (s *Store) RemoveMaterial(ctx context.Context, attachmentID, note string) error {
	_, err := s.db.ExecContext(ctx, `update materials
		set status = ?2, screen_note = ?3, screened_at = ?4, backed_up_at = null
		where attachment_id = ?1`, attachmentID, MaterialRejected, nullStr(note), ms(time.Now()))
	return err
}

// CountUnchecked counts the listed JPEGs of courseIDs the content check has
// not seen.
func (s *Store) CountUnchecked(ctx context.Context, courseIDs []string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from materials
		where status = 'approved' and content_type = 'image/jpeg' and backed_up_at is not null
			and vision_at is null and deleted_at is null
			and home_channel_id in (select value from json_each(?1))`, jsonList(courseIDs)).Scan(&n)
	return n, err
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
	return s.deleteMaterials(ctx, deleteLive+`message_id in (`+placeholders(len(messageIDs))+`)`+returnHome, args)
}

// DeleteMaterialsRemovedFrom hides the materials of a message that are no
// longer among keep, which is what an edit that removes an attachment leaves.
func (s *Store) DeleteMaterialsRemovedFrom(ctx context.Context, messageID string, keep []string) ([]string, error) {
	args := []any{ms(time.Now()), messageID}
	query := deleteLive + `message_id = ?`
	if len(keep) > 0 {
		for _, id := range keep {
			args = append(args, id)
		}
		query += ` and attachment_id not in (` + placeholders(len(keep)) + `)`
	}
	return s.deleteMaterials(ctx, query+returnHome, args)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// The two ends of a soft delete. The caller puts a condition between them,
// built only from literals and placeholders, never from a value.
const (
	deleteLive = `update materials set deleted_at = ?
		where deleted_at is null and `
	returnHome = `
		returning home_channel_id`
)

// deleteMaterials soft-deletes the live rows a query built from deleteLive and
// returnHome matches, and returns their home channels. The deletion time is the
// first of args, the condition's parameters follow.
func (s *Store) deleteMaterials(ctx context.Context, query string, args []any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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

// CourseStat summarises the listed material of one course channel.
type CourseStat struct {
	Files  int
	Bytes  int64
	Latest time.Time
}

// CourseMaterialStats counts the listed material of every course channel, by
// home channel ID. Channels without material are absent.
func (s *Store) CourseMaterialStats(ctx context.Context) (map[string]CourseStat, error) {
	rows, err := s.db.QueryContext(ctx, `select home_channel_id, count(*), coalesce(sum(size), 0), max(posted_at)
		from materials where `+visibleMaterial+` group by home_channel_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CourseStat{}
	for rows.Next() {
		var id string
		var st CourseStat
		if err := rows.Scan(&id, &st.Files, &st.Bytes, timeDest{&st.Latest}); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
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
