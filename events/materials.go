package events

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/materials"
	"steki-bot/mcp"
	"steki-bot/store"
	"steki-bot/verify"
)

// The PDFs and JPEGs posted in the course channels (and their threads) are
// backed up to the bucket. Nothing runs on its own schedule or at start-up:
// files reach the materials table in two ways only, a new message in a course
// channel as it arrives, and the /materials scan command, which walks the
// history of the course channels back to their first message (resumably) and
// places the stickies. The materials.Worker then copies each file into the
// bucket and screens it, waking when there is something to do. Course
// channels get a sticky card (sticky.go) whose button hands each member a
// personal link to the material page (links.go), which lists the screened
// files of that channel and its threads.

// courseCategoryIDs are the categories whose text channels are course
// channels and get the material page and sticky message.
var courseCategoryIDs = []string{
	"938731151705649152", "938731277492838441", "938731351035748362",
	"938731377971576892", "938731437421658133", "938731458288312321",
	"938731705794166784", "938731783774695435", "938731975525675028",
}

const (
	// noStickyPrefix marks the channels where members post exam results, which
	// get no sticky.
	noStickyPrefix = "στατιστικά-"

	// scanPagePause spaces out history requests, leaving the rate limit for
	// the bot's real work.
	scanPagePause = 300 * time.Millisecond

	// scanTimeout bounds one channel's history scan. A cut-off scan resumes
	// from where it stopped on the next pass.
	scanTimeout = 30 * time.Minute
)

// isCourseChannel reports whether ch is a course channel.
func isCourseChannel(ch *discordgo.Channel) bool {
	return ch != nil && ch.Type == discordgo.ChannelTypeGuildText &&
		slices.Contains(courseCategoryIDs, ch.ParentID)
}

// hasSticky reports whether ch keeps a sticky message: course channels do,
// except the statistics channels, where members post their exam results.
func hasSticky(ch *discordgo.Channel) bool {
	return isCourseChannel(ch) && !strings.HasPrefix(strings.ToLower(ch.Name), noStickyPrefix)
}

// materialLink returns one member's personal link to a course channel's
// material page, and the link's ID. viewer is the staff member viewing as them,
// if any.
func (h *Handler) materialLink(userID, channelID, viewer string) (link, tokenID string, err error) {
	guildID := h.guildID()
	if guildID == "" {
		return "", "", errors.New("materials: guild not known yet")
	}
	if tokenID, err = newTokenID(); err != nil {
		return "", "", err
	}
	token, err := h.tokens.IssueMaterials(userID, guildID, channelID, tokenID, viewer, time.Now().Add(personalLinkTTL))
	if err != nil {
		return "", "", err
	}
	return h.cfg.PublicURL + "/materials?t=" + token, tokenID, nil
}

// materialType returns the content type of an attachment that counts as
// material, a PDF or a JPEG, judged by name or declared type, or "" for
// anything else. The screener checks the content itself before the file is
// listed.
func materialType(a *discordgo.MessageAttachment) string {
	ext := strings.ToLower(path.Ext(a.Filename))
	declared := strings.ToLower(a.ContentType)
	switch {
	case ext == ".pdf" || strings.HasPrefix(declared, materials.TypePDF):
		return materials.TypePDF
	case ext == ".jpg" || ext == ".jpeg" || strings.HasPrefix(declared, materials.TypeJPEG):
		return materials.TypeJPEG
	}
	return ""
}

// captionLimit caps the message text kept with a file.
const captionLimit = 300

// materialHome is where a channel's files belong: the channel itself, or for
// a thread its parent. folders are the bucket folders, outermost first.
type materialHome struct {
	id      string
	folders []string
}

// homeFor resolves the home of ch, looking its parent up when it is a thread.
func (h *Handler) homeFor(ctx context.Context, s *discordgo.Session, ch *discordgo.Channel) materialHome {
	if !ch.IsThread() {
		return materialHome{id: ch.ID, folders: []string{ch.Name}}
	}
	parentName := ch.ParentID
	if p, err := h.channel(ctx, s, ch.ParentID); err == nil {
		parentName = p.Name
	}
	return materialHome{id: ch.ParentID, folders: []string{parentName, ch.Name}}
}

// recordMaterials adds the PDFs and JPEGs of one message to the materials
// table and returns how many were new.
func (h *Handler) recordMaterials(ctx context.Context, m *discordgo.Message, home materialHome, guildID string) int {
	if m.Author == nil {
		return 0
	}
	return h.recordAttachments(ctx, m, home, guildID, m.Author.ID, m.Author.Bot, truncateRunes(m.Content, captionLimit))
}

// recordAttachments is recordMaterials with the uploader and caption given,
// for files the bot reposts on a member's behalf.
func (h *Handler) recordAttachments(ctx context.Context, m *discordgo.Message, home materialHome, guildID,
	uploaderID string, uploaderBot bool, caption string) int {
	added := 0
	for _, a := range m.Attachments {
		ctype := materialType(a)
		if ctype == "" {
			continue
		}
		mat := &store.Material{
			AttachmentID:  a.ID,
			MessageID:     m.ID,
			ChannelID:     m.ChannelID,
			HomeChannelID: home.id,
			GuildID:       guildID,
			UploaderID:    uploaderID,
			UploaderBot:   uploaderBot,
			Filename:      a.Filename,
			ContentType:   ctype,
			Size:          int64(a.Size),
			Caption:       caption,
			SourceURL:     a.URL,
			ObjectKey:     materials.ObjectKey(home.folders, a.ID, a.Filename),
			PostedAt:      m.Timestamp,
		}
		ok, err := h.st.AddMaterial(ctx, mat)
		if err != nil {
			h.log.Error("materials: record attachment", "attachment_id", a.ID, "error", err)
			continue
		}
		if ok {
			added++
		}
	}
	return added
}

// onMaterialMessage handles a new message that passed screening: its files
// are queued for backup, and in a course channel the sticky moves back to
// the bottom.
func (h *Handler) onMaterialMessage(s *discordgo.Session, ch *discordgo.Channel, m *discordgo.Message) {
	if h.mat == nil {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()
	// Only course channels, and the threads under them, hold material.
	course := isCourseChannel(ch)
	if ch.IsThread() {
		p, err := h.channel(ctx, s, ch.ParentID)
		course = err == nil && isCourseChannel(p)
	}
	if !course {
		return
	}
	home := h.homeFor(ctx, s, ch)
	if h.recordMaterials(ctx, m, home, m.GuildID) > 0 {
		h.mat.Wake()
	}
	if hasSticky(ch) {
		h.bumpSticky(s, ch.ID)
	}
}

// forgetMaterials hides the files of deleted messages from the page. Their
// copies stay in the bucket. A deleted sticky is put back.
func (h *Handler) forgetMaterials(s *discordgo.Session, guildID, channelID string, messageIDs ...string) {
	if h.mat == nil || !h.inHomeGuild(guildID) {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()
	homes, err := h.st.DeleteMaterialsByMessage(ctx, messageIDs...)
	if err != nil {
		h.log.Error("materials: hide deleted files", "channel_id", channelID, "error", err)
	}
	for _, home := range homes {
		h.refreshSticky(s, home)
	}
	if sticky, err := h.st.Sticky(ctx, channelID); err == nil && slices.Contains(messageIDs, sticky) {
		h.bumpSticky(s, channelID)
	}
}

// onMessageDeleteBulk is a moderator purging many messages at once.
func (h *Handler) onMessageDeleteBulk(s *discordgo.Session, m *discordgo.MessageDeleteBulk) {
	for _, id := range m.Messages {
		h.msgs.take(id)
	}
	if h.inHomeGuild(m.GuildID) {
		h.track("discord_messages_bulk_deleted", map[string]any{
			"guild_id": m.GuildID, "channel_id": m.ChannelID, "count": len(m.Messages),
		})
	}
	h.forgetMaterials(s, m.GuildID, m.ChannelID, m.Messages...)
}

// pruneMaterials hides files an edit removed from a message. Discord lets
// the author remove attachments, not add them.
func (h *Handler) pruneMaterials(s *discordgo.Session, m *discordgo.Message) {
	if h.mat == nil || m.Attachments == nil || !h.inHomeGuild(m.GuildID) {
		return
	}
	keep := make([]string, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		keep = append(keep, a.ID)
	}
	ctx, cancel := h.ctx()
	defer cancel()
	homes, err := h.st.DeleteMaterialsRemovedFrom(ctx, m.ID, keep)
	if err != nil {
		h.log.Error("materials: hide removed attachments", "message_id", m.ID, "error", err)
	}
	for _, home := range homes {
		h.refreshSticky(s, home)
	}
}

// refreshMaterialSource fetches a new signed link for an attachment whose
// stored link expired before it could be backed up.
func (h *Handler) refreshMaterialSource(s *discordgo.Session) materials.Refresher {
	return func(ctx context.Context, m *store.Material) (string, error) {
		var msg *discordgo.Message
		err := h.retry(ctx, "materials.refresh", func() error {
			var err error
			msg, err = s.ChannelMessage(m.ChannelID, m.MessageID, discordgo.WithContext(ctx))
			return err
		})
		if dc.IsMissingResource(err) || dc.APICode(err) == discordgo.ErrCodeUnknownMessage {
			return "", materials.ErrSourceGone
		}
		if err != nil {
			return "", err
		}
		for _, a := range msg.Attachments {
			if a.ID == m.AttachmentID {
				return a.URL, nil
			}
		}
		return "", materials.ErrSourceGone
	}
}

// scanMaterials walks the history of every course channel and its threads and
// records the PDFs and JPEGs it finds. It returns how many channels it read
// and how many new files it found. It only runs when an admin asks.
func (h *Handler) scanMaterials(s *discordgo.Session) (channelsRead, newFiles int) {
	guildID := h.guildID()
	if guildID == "" || h.mat == nil {
		return 0, 0
	}
	ctx, cancel := h.ctxTimeout(5 * time.Minute)
	channels, err := dc.GuildChannels(ctx, s, h.log, guildID)
	cancel()
	if err != nil {
		h.log.Error("materials: list channels for scan", "error", err)
		return 0, 0
	}
	byID := make(map[string]*discordgo.Channel, len(channels))
	var courses []*discordgo.Channel
	for _, ch := range channels {
		byID[ch.ID] = ch
		if isCourseChannel(ch) {
			courses = append(courses, ch)
		}
	}

	targets := slices.Clone(courses)
	for _, th := range h.materialThreads(s, guildID, courses) {
		if isCourseChannel(byID[th.ParentID]) {
			targets = append(targets, th)
		}
	}

	started := time.Now()
	found := 0
	for _, ch := range targets {
		if h.base.Err() != nil {
			return len(targets), found
		}
		home := materialHome{id: ch.ID, folders: []string{ch.Name}}
		if ch.IsThread() {
			parentName := ch.ParentID
			if p := byID[ch.ParentID]; p != nil {
				parentName = p.Name
			}
			home = materialHome{id: ch.ParentID, folders: []string{parentName, ch.Name}}
		}
		n, err := h.scanChannel(s, ch, home, guildID)
		found += n
		switch {
		case dc.RESTCode(err) == 403 || dc.APICode(err) == discordgo.ErrCodeMissingAccess:
			h.log.Debug("materials: channel not readable; skipped", "channel_id", ch.ID, "name", ch.Name)
		case err != nil:
			h.log.Warn("materials: scan channel", "channel_id", ch.ID, "name", ch.Name, "error", err)
		}
	}
	h.log.Info("materials: history scan finished",
		"channels", len(targets), "new_files", found, "took", time.Since(started).Round(time.Second))
	return len(targets), found
}

// materialThreads lists the guild's threads: active ones, and the archived
// public threads of every channel that can have them.
func (h *Handler) materialThreads(s *discordgo.Session, guildID string, channels []*discordgo.Channel) []*discordgo.Channel {
	ctx, cancel := h.ctxTimeout(5 * time.Minute)
	defer cancel()
	var out []*discordgo.Channel
	if active, err := s.GuildThreadsActive(guildID, discordgo.WithContext(ctx)); err == nil {
		out = append(out, active.Threads...)
	} else {
		h.log.Warn("materials: list active threads", "error", err)
	}
	for _, ch := range channels {
		switch ch.Type {
		case discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews, discordgo.ChannelTypeGuildForum:
		default:
			continue
		}
		var before *time.Time
		for ctx.Err() == nil {
			list, err := s.ThreadsArchived(ch.ID, before, 100, discordgo.WithContext(ctx))
			if err != nil {
				break // typically no permission to read the channel
			}
			out = append(out, list.Threads...)
			if !list.HasMore || len(list.Threads) == 0 {
				break
			}
			last := list.Threads[len(list.Threads)-1]
			if last.ThreadMetadata == nil {
				break
			}
			t := last.ThreadMetadata.ArchiveTimestamp
			before = &t
		}
	}
	// A thread can be both just-archived and in the active list.
	slices.SortFunc(out, func(a, b *discordgo.Channel) int { return strings.Compare(a.ID, b.ID) })
	return slices.CompactFunc(out, func(a, b *discordgo.Channel) bool { return a.ID == b.ID })
}

// scanChannel brings one channel's scan up to date: first the messages
// posted above the scanned range since the last pass, then further back into
// history until the first message. Progress is saved after every page.
func (h *Handler) scanChannel(s *discordgo.Session, ch *discordgo.Channel, home materialHome, guildID string) (int, error) {
	ctx, cancel := h.ctxTimeout(scanTimeout)
	defer cancel()

	st, err := h.st.MaterialScan(ctx, ch.ID)
	if err != nil {
		return 0, err
	}
	if st.HighID == "" {
		st = store.ScanState{} // never read anything: start from the top
	}
	if st.Done && (ch.LastMessageID == "" || !newerID(ch.LastMessageID, st.HighID)) {
		return 0, nil
	}

	found := 0
	page := func(before string) ([]*discordgo.Message, error) {
		var msgs []*discordgo.Message
		err := h.retry(ctx, "materials.history", func() error {
			var err error
			msgs, err = s.ChannelMessages(ch.ID, 100, before, "", "", discordgo.WithContext(ctx))
			return err
		})
		time.Sleep(scanPagePause)
		return msgs, err
	}
	record := func(msgs []*discordgo.Message) {
		n := 0
		for _, m := range msgs {
			n += h.recordMaterials(ctx, m, home, guildID)
		}
		if n > 0 {
			found += n
			h.mat.Wake()
		}
	}

	// The gap above the scanned range. HighID only moves once the gap is
	// closed, so a scan cut off half-way repeats it rather than leaving a hole.
	if st.HighID != "" {
		newest, before := "", ""
		for {
			msgs, err := page(before)
			if err != nil {
				return found, err
			}
			fresh := msgs
			if i := slices.IndexFunc(msgs, func(m *discordgo.Message) bool { return !newerID(m.ID, st.HighID) }); i >= 0 {
				fresh = msgs[:i]
			}
			if newest == "" && len(fresh) > 0 {
				newest = fresh[0].ID
			}
			record(fresh)
			if len(fresh) < len(msgs) || len(msgs) < 100 {
				break
			}
			before = msgs[len(msgs)-1].ID
		}
		if newest != "" {
			st.HighID = newest
			if err := h.st.SetMaterialScan(ctx, ch.ID, st); err != nil {
				return found, err
			}
		}
	}

	// Further back into history.
	for !st.Done {
		msgs, err := page(st.LowID)
		if err != nil {
			return found, err
		}
		if st.HighID == "" && len(msgs) > 0 {
			st.HighID = msgs[0].ID
		}
		record(msgs)
		if len(msgs) > 0 {
			st.LowID = msgs[len(msgs)-1].ID
		}
		st.Done = len(msgs) < 100
		if err := h.st.SetMaterialScan(ctx, ch.ID, st); err != nil {
			return found, err
		}
	}
	return found, nil
}

// newerID reports whether snowflake a is later than b. Snowflakes grow with
// time, but compare as numbers, not strings.
func newerID(a, b string) bool {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	return errA == nil && errB == nil && x > y
}

// MaterialSource returns the verify.MaterialSource that serves the material
// page from the database and the bucket, or nil when the bucket is not
// configured.
func (h *Handler) MaterialSource(s *discordgo.Session) verify.MaterialSource {
	if h.mat == nil {
		return nil
	}
	return &materialSource{h: h, s: s}
}

type materialSource struct {
	h *Handler
	s *discordgo.Session
}

func (src *materialSource) Channel(ctx context.Context, channelID string) (*verify.MaterialChannel, error) {
	h := src.h
	ch, err := h.channel(ctx, src.s, channelID)
	if err != nil {
		if dc.IsMissingResource(err) {
			return nil, verify.ErrNoMaterial
		}
		return nil, err
	}
	if !isCourseChannel(ch) {
		return nil, verify.ErrNoMaterial
	}
	rows, err := h.st.ChannelMaterials(ctx, channelID)
	if err != nil {
		return nil, err
	}
	threads := map[string]string{}
	out := &verify.MaterialChannel{Name: ch.Name, Files: make([]verify.MaterialFile, 0, len(rows))}
	for _, m := range rows {
		f := verify.MaterialFile{
			ID:       m.AttachmentID,
			Name:     m.Filename,
			Type:     m.ContentType,
			Size:     m.Size,
			Caption:  m.Caption,
			PostedAt: m.PostedAt,
			Uploader: src.memberName(m.GuildID, m.UploaderID),
		}
		if m.ChannelID != channelID {
			name, ok := threads[m.ChannelID]
			if !ok {
				if t, err := h.channel(ctx, src.s, m.ChannelID); err == nil {
					name = t.Name
				}
				threads[m.ChannelID] = name
			}
			f.Thread = name
		}
		out.Files = append(out.Files, f)
	}
	return out, nil
}

// memberName is how the page credits an uploader: their server nickname or
// display name, from the cache only, so listing never waits on Discord.
func (src *materialSource) memberName(guildID, userID string) string {
	mem, err := src.s.State.Member(guildID, userID)
	if err != nil || mem.User == nil {
		return ""
	}
	if mem.Nick != "" {
		return mem.Nick
	}
	return displayName(mem.User)
}

func (src *materialSource) Open(ctx context.Context, channelID, fileID string) (*verify.MaterialBody, error) {
	m, err := src.h.st.ChannelMaterial(ctx, channelID, fileID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, verify.ErrNoMaterial
	}
	if err != nil {
		return nil, err
	}
	body, size, err := src.h.mat.Bucket().Open(ctx, m.ObjectKey)
	if err != nil {
		return nil, err
	}
	return &verify.MaterialBody{Body: body, Size: size, Type: m.ContentType}, nil
}

// Notes implements verify.Notebook.
func (src *materialSource) Notes(ctx context.Context, userID string) (map[string]map[string]json.RawMessage, error) {
	rows, err := src.h.st.StudyNotes(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]json.RawMessage, len(rows))
	for _, n := range rows {
		if out[n.AttachmentID] == nil {
			out[n.AttachmentID] = map[string]json.RawMessage{}
		}
		out[n.AttachmentID][n.Kind] = json.RawMessage(n.Data)
	}
	return out, nil
}

// SaveNote implements verify.Notebook.
func (src *materialSource) SaveNote(ctx context.Context, userID, fileID, kind string, data json.RawMessage) error {
	err := src.h.st.SetStudyNote(ctx, userID, fileID, kind, string(data))
	if errors.Is(err, store.ErrStudyQuota) {
		return verify.ErrNoteQuota
	}
	return err
}

// CanRead implements verify.AccessChecker with the MCP server's rule: a
// member who finished registration and can see the channel in Discord.
func (src *materialSource) CanRead(ctx context.Context, userID, channelID string) error {
	lib := &mcpLibrary{h: src.h, s: src.s, src: src}
	rd, err := lib.Reader(ctx, userID)
	if err == nil {
		_, err = rd.(*mcpReader).check(ctx, channelID)
	}
	if errors.Is(err, mcp.ErrForbidden) || errors.Is(err, mcp.ErrNotFound) {
		return verify.ErrNoAccess
	}
	return err
}
