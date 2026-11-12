package moderation

import (
	"fmt"
	"strings"
	"time"
)

// statusEvery is the time between presence updates. Discord allows five per
// twenty seconds on one connection, so this is as fast as an animation can go.
const statusEvery = 4 * time.Second

// scene is one animation: a frame per presence update.
type scene []string

// slide moves sprite across a track of fill, left to right.
func slide(sprite, fill string, width int) scene {
	out := make(scene, width+1)
	for i := range out {
		out[i] = strings.Repeat(fill, i) + sprite + strings.Repeat(fill, width-i)
	}
	return out
}

// wave rotates a pattern one step per frame.
func wave(pattern string, frames int) scene {
	r := []rune(pattern)
	out := make(scene, frames)
	for i := range out {
		out[i] = string(r[i%len(r):]) + string(r[:i%len(r)])
	}
	return out
}

// loading fills a bar and empties it again.
func loading(width int) scene {
	var out scene
	for i := 0; i <= width; i++ {
		out = append(out, fmt.Sprintf("[%s%s] %d%%", strings.Repeat("#", i), strings.Repeat("-", width-i), i*100/width))
	}
	return out
}

var animations = []scene{
	slide("=^.^= ~<:3", "·", 8),
	slide("=[=>", "·", 10),
	{`\(^o^)/ ♪`, `|(^o^)| ♫`, `/(^o^)\ ♪`, `|(^o^)| ♫`, `\(^o^)/ ♪`, `|(^o^)| ♫`},
	{"(•_•)", "(•_•)>⌐■-■", "(⌐■_■)", "(⌐■_■) deal with it", "(⌐■_■) 😎"},
	wave("._.-'-._.-'-", 12),
	loading(8),
	{"( ˘▽˘)っ♨", "( ˘▽˘)っ♨ ~", "( ˘▽˘)っ♨ ~~", "( ˘▽˘)っ♨ ~~~", "(ᵔ◡ᵔ) ☕ ahh"},
	{"*  .  *  .  *", ".  *  .  *  .", "*  .  ✦  .  *", ".  ✦  .  *  .", "✦  .  *  .  ✦"},
}

// statusFrame is what the presence shows at tick n: the animations play one
// after another, and after each comes a line with the bot's counters, so the
// numbers stay visible.
func statusFrame(n int64, uptime time.Duration, blocked, processed int64) string {
	info := scene{
		"Uptime: " + formatUptime(uptime),
		fmt.Sprintf("Κακόβουλα μηνύματα: %d", blocked),
		fmt.Sprintf("Επεξεργασμένα μηνύματα: %d", processed),
	}
	var total int64
	for _, a := range animations {
		total += int64(len(a)) + 1
	}
	n %= total
	for i, a := range animations {
		if n < int64(len(a)) {
			return a[n]
		}
		n -= int64(len(a))
		if n == 0 {
			return info[i%len(info)]
		}
		n--
	}
	return info[0]
}
