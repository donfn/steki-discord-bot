package dc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// Discord's hard limits. Hitting the first of these with no overflow handling
// is what silently broke every new join in production:
//
//	code 50035, CHANNEL_PARENT_MAX_CHANNELS: Maximum number of channels in category reached (50)
const (
	maxPerCategory = 50
	maxPerGuild    = 500

	// headroom leaves a slot free as a margin against a gateway cache that
	// has not yet seen a channel created moments ago.
	headroom = 1
)

// overflowPrefix names the categories this package creates when the configured
// register category is full.
const overflowPrefix = "register-overflow-"

// ErrGuildChannelLimit means the guild itself is out of channel slots. No
// overflow category can help; a moderator has to delete something.
var ErrGuildChannelLimit = errors.New("guild channel limit reached")

// Occupancy describes how full a register category is, for /bot stats.
type Occupancy struct {
	CategoryID string
	Name       string
	Used       int
	Capacity   int
}

// Placer creates registration channels in a category that has room,
// provisioning register-overflow-N categories when every known one is full.
//
// Placement is serialised: counting free slots and creating the channel is a
// read-then-write, and two joins interleaving it could both claim a category's
// last slot or both create "register-overflow-2".
type Placer struct {
	baseCategoryID string
	log            *slog.Logger
	mu             sync.Mutex
}

// NewPlacer returns a Placer that fills baseCategoryID first.
func NewPlacer(baseCategoryID string, logger *slog.Logger) *Placer {
	return &Placer{baseCategoryID: baseCategoryID, log: logger}
}

// Create makes a registration channel and returns it with the category it
// landed in. data.ParentID is overwritten.
func (p *Placer) Create(ctx context.Context, s *discordgo.Session, guildID string,
	data discordgo.GuildChannelCreateData) (*discordgo.Channel, string, error) {

	p.mu.Lock()
	defer p.mu.Unlock()

	channels, err := GuildChannels(ctx, s, p.log, guildID)
	if err != nil {
		return nil, "", fmt.Errorf("list guild channels: %w", err)
	}
	if len(channels) >= maxPerGuild-headroom {
		return nil, "", fmt.Errorf("%w: %d/%d channels", ErrGuildChannelLimit, len(channels), maxPerGuild)
	}

	categoryID, err := p.pickCategory(ctx, s, guildID, channels)
	if err != nil {
		return nil, "", err
	}

	created, err := p.createChannel(ctx, s, guildID, categoryID, data)

	// The cache can lag behind Discord, so a category may turn out to be full
	// anyway. Rather than fail the join, provision a fresh overflow category
	// and try once more.
	if isCategoryFull(err) {
		p.log.Warn("category full on create, provisioning overflow", "category_id", categoryID)
		if categoryID, err = p.createOverflow(ctx, s, guildID, channels); err != nil {
			return nil, "", err
		}
		created, err = p.createChannel(ctx, s, guildID, categoryID, data)
	}
	if err != nil {
		return nil, categoryID, err
	}
	return created, categoryID, nil
}

// Occupancies reports usage for the base category and every overflow category.
func (p *Placer) Occupancies(ctx context.Context, s *discordgo.Session, guildID string) ([]Occupancy, error) {
	channels, err := GuildChannels(ctx, s, p.log, guildID)
	if err != nil {
		return nil, err
	}
	counts := childCounts(channels)

	var out []Occupancy
	for _, c := range registerCategories(channels, p.baseCategoryID) {
		out = append(out, Occupancy{
			CategoryID: c.ID,
			Name:       c.Name,
			Used:       counts[c.ID],
			Capacity:   maxPerCategory,
		})
	}
	return out, nil
}

func (p *Placer) createChannel(ctx context.Context, s *discordgo.Session, guildID, categoryID string,
	data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {

	data.ParentID = categoryID
	var created *discordgo.Channel
	err := Do(ctx, p.log, "channel.create", func() error {
		ch, cerr := s.GuildChannelCreateComplex(guildID, data, discordgo.WithContext(ctx))
		if cerr != nil {
			return cerr
		}
		created = ch
		return nil
	})
	if err != nil {
		return nil, err
	}
	cache(s, created)
	return created, nil
}

// pickCategory returns the first register category with a free slot, creating
// an overflow category when they are all full.
func (p *Placer) pickCategory(ctx context.Context, s *discordgo.Session, guildID string,
	channels []*discordgo.Channel) (string, error) {

	counts := childCounts(channels)
	for _, c := range registerCategories(channels, p.baseCategoryID) {
		if counts[c.ID] < maxPerCategory-headroom {
			return c.ID, nil
		}
	}
	return p.createOverflow(ctx, s, guildID, channels)
}

// createOverflow provisions the next register-overflow-N category, copying the
// base category's permission overwrites so visibility rules are identical.
func (p *Placer) createOverflow(ctx context.Context, s *discordgo.Session, guildID string,
	channels []*discordgo.Channel) (string, error) {

	var overwrites []*discordgo.PermissionOverwrite
	for _, c := range channels {
		if c.ID == p.baseCategoryID {
			overwrites = c.PermissionOverwrites
			break
		}
	}

	name := fmt.Sprintf("%s%d", overflowPrefix, nextOverflowIndex(channels))
	var created *discordgo.Channel
	err := Do(ctx, p.log, "category.create", func() error {
		ch, cerr := s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
			Name:                 name,
			Type:                 discordgo.ChannelTypeGuildCategory,
			PermissionOverwrites: overwrites,
		}, discordgo.WithContext(ctx))
		if cerr != nil {
			return cerr
		}
		created = ch
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("create overflow category %s: %w", name, err)
	}
	cache(s, created)

	p.log.Info("provisioned overflow register category", "name", name, "category_id", created.ID)
	return created.ID, nil
}

// GuildChannels returns a snapshot of the guild's channels, preferring the
// gateway state cache and falling back to the REST API.
func GuildChannels(ctx context.Context, s *discordgo.Session, log *slog.Logger,
	guildID string) ([]*discordgo.Channel, error) {

	if g, err := s.State.Guild(guildID); err == nil {
		// Copy under the state lock: the gateway goroutine mutates this slice.
		s.State.RLock()
		channels := slices.Clone(g.Channels)
		s.State.RUnlock()
		if len(channels) > 0 {
			return channels, nil
		}
	}

	var channels []*discordgo.Channel
	err := Do(ctx, log, "guild.channels", func() error {
		cs, cerr := s.GuildChannels(guildID, discordgo.WithContext(ctx))
		if cerr != nil {
			return cerr
		}
		channels = cs
		return nil
	})
	return channels, err
}

// cache records a channel this process just created, so the next placement
// counts it even before the gateway's CHANNEL_CREATE arrives.
func cache(s *discordgo.Session, ch *discordgo.Channel) {
	if s.StateEnabled && ch != nil {
		_ = s.State.ChannelAdd(ch)
	}
}

func isCategoryFull(err error) bool {
	return err != nil && APICode(err) == 50035 &&
		strings.Contains(err.Error(), "CHANNEL_PARENT_MAX_CHANNELS")
}

// registerCategories returns the base category followed by overflow categories
// in numeric order, so channels fill predictably.
func registerCategories(channels []*discordgo.Channel, baseCategoryID string) []*discordgo.Channel {
	var base *discordgo.Channel
	var overflow []*discordgo.Channel

	for _, c := range channels {
		if c.Type != discordgo.ChannelTypeGuildCategory {
			continue
		}
		if c.ID == baseCategoryID {
			base = c
			continue
		}
		if _, ok := overflowIndex(c.Name); ok {
			overflow = append(overflow, c)
		}
	}

	sort.Slice(overflow, func(i, j int) bool {
		a, _ := overflowIndex(overflow[i].Name)
		b, _ := overflowIndex(overflow[j].Name)
		return a < b
	})

	out := make([]*discordgo.Channel, 0, len(overflow)+1)
	if base != nil {
		out = append(out, base)
	}
	return append(out, overflow...)
}

// nextOverflowIndex returns the first unused overflow number. Numbering starts
// at 2, the base category being implicitly number 1.
func nextOverflowIndex(channels []*discordgo.Channel) int {
	next := 2
	for _, c := range channels {
		if c.Type != discordgo.ChannelTypeGuildCategory {
			continue
		}
		if n, ok := overflowIndex(c.Name); ok && n >= next {
			next = n + 1
		}
	}
	return next
}

func overflowIndex(name string) (int, bool) {
	if !strings.HasPrefix(name, overflowPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, overflowPrefix))
	if err != nil || n < 2 {
		return 0, false
	}
	return n, true
}

func childCounts(channels []*discordgo.Channel) map[string]int {
	counts := map[string]int{}
	for _, c := range channels {
		if c.ParentID != "" {
			counts[c.ParentID]++
		}
	}
	return counts
}
