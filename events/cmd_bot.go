package events

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// cardsShown caps how many waiting cards /bot stats counts; more than that is
// "plenty" for its purpose, which is to say a catch-up is due.
const cardsShown = 50

func (h *Handler) cmdBotStats(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "**Κατάσταση bot**\n• Uptime: %s\n", formatUptime(time.Since(h.bootTime)))
	fmt.Fprintf(&b, "• Μηνύματα: %d · Μπλοκαρισμένα: %d\n", h.processed.Load(), h.blocked.Load())

	if err := h.st.Ping(ctx); err != nil {
		fmt.Fprintf(&b, "• 🔴 Βάση: %v\n", err)
	} else {
		stat := h.st.DBStats()
		fmt.Fprintf(&b, "• 🟢 Βάση: %.1f MB · %d/%d συνδέσεις\n",
			float64(h.st.FileSize())/(1<<20), stat.InUse, stat.MaxOpenConnections)
	}

	if open, err := h.st.ListOpen(ctx); err == nil {
		fmt.Fprintf(&b, "• Ανοιχτές εγγραφές: %d\n", len(open))
	}
	// Nothing catches the bot up on its own, so say what is due.
	if last := h.lastReconcile.Load(); last > 0 {
		fmt.Fprintf(&b, "• Τελευταίος συγχρονισμός εγγραφών: <t:%d:R>\n", last)
	} else {
		b.WriteString("• Συγχρονισμός εγγραφών: δεν έχει τρέξει από την εκκίνηση (`/reg reconcile`)\n")
	}
	if h.mat != nil {
		reviews, _ := h.st.ReviewsToAnnounce(ctx, cardsShown)
		reports, _ := h.st.ReportsToAnnounce(ctx, cardsShown)
		if n := len(reviews) + len(reports); n > 0 {
			fmt.Fprintf(&b, "• ⚠️ Κάρτες που δεν έφτασαν στα logs: %d (`/materials cards`)\n", n)
		}
	}
	if es, err := h.st.EmailStats(ctx); err == nil {
		fmt.Fprintf(&b, "• Emails: %d (με κάτοχο: %d) · παλιά hashes: %d\n",
			es.Verified, es.VerifiedLinked, es.LegacyHashes)
	}

	if occ, err := h.placer.Occupancies(ctx, s, h.guildID()); err == nil {
		b.WriteString("\n**Κατηγορίες εγγραφής**\n")
		for _, o := range occ {
			warn := ""
			if o.Used >= o.Capacity-5 {
				warn = " ⚠️"
			}
			fmt.Fprintf(&b, "• `%s`: %d/%d%s\n", o.Name, o.Used, o.Capacity, warn)
		}
	}

	if counts, err := h.st.CountEventsByKind(ctx, time.Now().Add(-24*time.Hour)); err == nil && len(counts) > 0 {
		b.WriteString("\n**Τελευταίο 24ωρο**\n")
		kinds := make([]string, 0, len(counts))
		for k := range counts {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			fmt.Fprintf(&b, "• `%s`: %d\n", k, counts[k])
		}
	}

	h.reply(s, i, b.String())
}
