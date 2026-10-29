package events

import (
	"context"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
	"steki-bot/legacylog"
	"steki-bot/store"
)

// The log reconciliation. The emails imported from the old bot are only
// hashes, most with no owner; the old bot's moderator log channel is the one
// record of who submitted which. On boot the bot reads that channel page by
// page, newest to oldest, saving each page's evidence and its position in the
// database, so a restart resumes where the last one stopped and a finished
// scan only reads what was posted since. After every scan it binds each hash
// it can to its owner.
const (
	logPageSize  = 100
	logPagePause = 500 * time.Millisecond
	logScanLimit = 2 * time.Hour
)

// startLegacyScan runs the scan and reconciliation once per process, in the
// background: it may take minutes and nothing else waits on it.
func (h *Handler) startLegacyScan(s *discordgo.Session) {
	h.legacyOnce.Do(func() {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.safely("legacy_scan", func() { h.legacyScan(s) })
		}()
	})
}

func (h *Handler) legacyScan(s *discordgo.Session) {
	ctx, cancel := h.ctxTimeout(logScanLimit)
	defer cancel()

	st, err := h.st.LogScanState(ctx, config.LogChannelID)
	if err != nil {
		h.log.Error("legacy scan: load state", "error", err)
		return
	}
	before := st.Scanned
	h.log.Info("legacy scan: starting", "complete", st.Complete, "scanned", st.Scanned)

	if err := h.scanLog(ctx, s, &st); err != nil {
		// The pages read so far are saved; the next boot carries on.
		h.log.Warn("legacy scan: stopped early", "scanned", st.Scanned, "error", err)
	}
	h.log.Info("legacy scan: done", "complete", st.Complete,
		"scanned", st.Scanned, "new_messages", st.Scanned-before)

	res, err := h.reconcileLegacy(ctx)
	if err != nil {
		h.log.Error("legacy reconcile failed", "error", err)
		return
	}
	h.log.Info("legacy reconcile: done", "claims", res.claims, "linked", res.linked,
		"plaintext", res.plaintext, "unresolved", res.unresolved)
	if res.linked > 0 || res.plaintext > 0 {
		h.alertMods(s, "🧾 Συμφιλίωση ιστορικού email από το κανάλι καταγραφής: "+
			"**%d** email συνδέθηκαν με τον κάτοχό τους και **%d** βρέθηκαν σε μορφή κειμένου. "+
			"**%d** παραμένουν χωρίς γνωστό κάτοχο.", res.linked, res.plaintext, res.unresolved)
	}
}

// scanLog first reads what was posted since the newest message seen, then
// carries on back from the oldest message seen until the channel's start.
func (h *Handler) scanLog(ctx context.Context, s *discordgo.Session, st *store.LogScan) error {
	if st.NewestID != "" {
		for {
			msgs, err := h.logPage(ctx, s, "", st.NewestID)
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				break
			}
			for _, m := range msgs {
				if snowflakeAfter(m.ID, st.NewestID) {
					st.NewestID = m.ID
				}
			}
			if err := h.saveLogPage(ctx, st, msgs); err != nil {
				return err
			}
			if len(msgs) < logPageSize {
				break
			}
		}
	}

	for !st.Complete {
		msgs, err := h.logPage(ctx, s, st.OldestID, "")
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			st.Complete = true
			if err := h.st.SaveLogPage(ctx, *st, nil); err != nil {
				return err
			}
			break
		}
		for _, m := range msgs {
			if st.OldestID == "" || snowflakeAfter(st.OldestID, m.ID) {
				st.OldestID = m.ID
			}
			if st.NewestID == "" || snowflakeAfter(m.ID, st.NewestID) {
				st.NewestID = m.ID
			}
		}
		if err := h.saveLogPage(ctx, st, msgs); err != nil {
			return err
		}
	}
	return nil
}

// logPage fetches one page, before or after a message, pausing first so the
// scan never competes with the bot's real work for the rate limit.
func (h *Handler) logPage(ctx context.Context, s *discordgo.Session, beforeID, afterID string) ([]*discordgo.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(logPagePause):
	}
	var msgs []*discordgo.Message
	err := h.retry(ctx, "log.history", func() error {
		var err error
		msgs, err = s.ChannelMessages(config.LogChannelID, logPageSize, beforeID, afterID, "", discordgo.WithContext(ctx))
		return err
	})
	return msgs, err
}

func (h *Handler) saveLogPage(ctx context.Context, st *store.LogScan, msgs []*discordgo.Message) error {
	var evidence []store.LogEvidence
	for _, m := range msgs {
		if e, ok := legacylog.Parse(m.ID, m.Content, m.Timestamp); ok {
			evidence = append(evidence, e)
		}
	}
	st.Scanned += int64(len(msgs))
	return h.st.SaveLogPage(ctx, *st, evidence)
}

// snowflakeAfter reports whether Discord ID a is newer than b. IDs are
// decimal integers, so a longer one is larger.
func snowflakeAfter(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

type legacyResult struct {
	claims, linked, plaintext, unresolved int
}

// reconcileLegacy binds every claim the evidence can settle. It only ever
// fills in what is missing, so running it after every scan is safe.
func (h *Handler) reconcileLegacy(ctx context.Context) (legacyResult, error) {
	var res legacyResult
	claims, err := h.st.LegacyClaims(ctx)
	if err != nil {
		return res, err
	}
	res.claims = len(claims)
	if len(claims) == 0 {
		return res, nil
	}

	hashes := make([]string, len(claims))
	for i, c := range claims {
		hashes[i] = c.Hash
	}
	evidence, err := h.st.EvidenceFor(ctx, hashes)
	if err != nil {
		return res, err
	}

	for _, c := range claims {
		owner, email := legacylog.Resolve(evidence[c.Hash])
		if c.UserID != nil {
			owner = "" // already bound; never replaced
		}
		if c.Email != "" {
			email = "" // already plaintext
		}
		if owner == "" && email == "" {
			if c.UserID == nil {
				res.unresolved++
			}
			continue
		}
		if err := h.st.ResolveClaim(ctx, c, owner, email); err != nil {
			h.log.Warn("legacy reconcile: resolve claim", "hash", c.Hash[:12], "error", err)
			continue
		}
		if owner != "" {
			res.linked++
		} else if c.UserID == nil {
			res.unresolved++
		}
		if email != "" {
			res.plaintext++
		}
	}
	return res, nil
}
