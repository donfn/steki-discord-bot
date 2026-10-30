package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The tools. All three only read, and say so in their annotations so a client
// can run them without asking the member each time.

const (
	toolCourses  = "list_course_channels"
	toolIndex    = "get_material_index"
	toolDownload = "download_material"
)

// userError is a failure the model can act on, such as an unknown channel. It
// is returned as a tool result with isError set, not as a protocol error.
type userError string

func (e userError) Error() string { return string(e) }

type callEnv struct {
	ctx  context.Context
	user string
	rd   Reader
	args json.RawMessage
}

type toolResult struct {
	text string
	// data is the structured form of the result.
	data any
	// file, when set, is streamed after text as the result's last content.
	file *Download
}

type tool struct {
	Name         string         `json:"name"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations"`
	run          func(*callEnv) (*toolResult, error)
}

func findTool(name string) *tool {
	for i := range toolDefs {
		if toolDefs[i].Name == name {
			return &toolDefs[i]
		}
	}
	return nil
}

var readOnly = map[string]any{
	"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false,
}

func annotations(title string) map[string]any {
	a := map[string]any{"title": title}
	for k, v := range readOnly {
		a[k] = v
	}
	return a
}

var channelProp = map[string]any{
	"type": "string",
	"description": "The course channel: its id from list_course_channels, or its name " +
		"(case-insensitive; a unique part of the name is enough).",
}

var toolDefs []tool

func init() {
	toolDefs = []tool{
		{
			Name:  toolCourses,
			Title: "List course channels",
			Description: "Lists the course channels of the Steki server (the channels under the course categories) " +
				"that you can see, with their category, how many files of material each has and when the newest was " +
				"posted. Start here to find a channel's id. Channel names are in Greek.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string",
						"description": "Only channels whose name or category contains this text (case-insensitive)."},
					"only_with_material": map[string]any{"type": "boolean",
						"description": "Hide channels that have no material yet. Default false."},
				},
				"additionalProperties": false,
			},
			OutputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{"type": "integer"},
					"channels": map[string]any{"type": "array", "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"},
							"category": map[string]any{"type": "string"}, "files": map[string]any{"type": "integer"},
							"total_bytes":   map[string]any{"type": "integer"},
							"latest_posted": map[string]any{"type": "string", "format": "date-time"},
						},
						"required": []string{"id", "name", "category", "files"},
					}},
				},
				"required": []string{"count", "channels"},
			},
			Annotations: annotations("List course channels"),
			run:         runCourses,
		},
		{
			Name:  toolIndex,
			Title: "Get a channel's material index",
			Description: "Returns everything the server knows about the material of one course channel, newest first: " +
				"for each PDF or JPEG its id, file name, type, size, the caption the uploader wrote, when it was posted, " +
				"who shared it, the thread it came from, and a resource URI. Use the ids with download_material.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"channel": channelProp,
					"type": map[string]any{"type": "string", "enum": []string{"pdf", "image"},
						"description": "Only PDFs or only images."},
					"query": map[string]any{"type": "string",
						"description": "Only files whose name, caption, uploader or thread contains this text (case-insensitive)."},
				},
				"required":             []string{"channel"},
				"additionalProperties": false,
			},
			OutputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"channel":     map[string]any{"type": "object"},
					"count":       map[string]any{"type": "integer"},
					"total_bytes": map[string]any{"type": "integer"},
					"files":       map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
				},
				"required": []string{"channel", "count", "files"},
			},
			Annotations: annotations("Get a channel's material index"),
			run:         runIndex,
		},
		{
			Name:  toolDownload,
			Title: "Download a file of material",
			Description: "Downloads one PDF or JPEG from a course channel's material and returns its content: " +
				"images as an image you can look at, PDFs as an embedded resource. Files over 32 MB are refused. " +
				"Get file ids from get_material_index.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"channel": channelProp,
					"file_id": map[string]any{"type": "string",
						"description": "The file's id from get_material_index (its resource URI also works)."},
				},
				"required":             []string{"channel", "file_id"},
				"additionalProperties": false,
			},
			Annotations: annotations("Download a file of material"),
			run:         runDownload,
		},
	}
}

func decodeArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return userError("the arguments do not match the tool's input schema: " + err.Error())
	}
	return nil
}

// ── list_course_channels ────────────────────────────────────────────────────

type courseJSON struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Category     string     `json:"category"`
	Files        int        `json:"files"`
	TotalBytes   int64      `json:"total_bytes"`
	LatestPosted *time.Time `json:"latest_posted,omitempty"`
}

func newCourseJSON(c Course) courseJSON {
	j := courseJSON{ID: c.ID, Name: c.Name, Category: c.Category, Files: c.Files, TotalBytes: c.Bytes}
	if !c.Latest.IsZero() {
		t := c.Latest.UTC()
		j.LatestPosted = &t
	}
	return j
}

func runCourses(e *callEnv) (*toolResult, error) {
	var a struct {
		Query            string `json:"query"`
		OnlyWithMaterial bool   `json:"only_with_material"`
	}
	if err := decodeArgs(e.args, &a); err != nil {
		return nil, err
	}
	courses, err := e.rd.Courses(e.ctx)
	if err != nil {
		return nil, err
	}
	q := fold(a.Query)
	out := make([]courseJSON, 0, len(courses))
	var b strings.Builder
	lastCat := ""
	for _, c := range courses {
		if a.OnlyWithMaterial && c.Files == 0 {
			continue
		}
		if q != "" && !strings.Contains(fold(c.Name+" "+c.Category), q) {
			continue
		}
		if c.Category != lastCat {
			fmt.Fprintf(&b, "\n## %s\n", c.Category)
			lastCat = c.Category
		}
		fmt.Fprintf(&b, "- #%s (id %s): %d files, %s", c.Name, c.ID, c.Files, humanSize(c.Bytes))
		if !c.Latest.IsZero() {
			fmt.Fprintf(&b, ", newest %s", c.Latest.UTC().Format("2006-01-02"))
		}
		b.WriteByte('\n')
		out = append(out, newCourseJSON(c))
	}
	text := fmt.Sprintf("%d course channels.\n%s", len(out), b.String())
	if len(out) == 0 {
		text = "No course channels match."
	}
	return &toolResult{text: strings.TrimSpace(text), data: map[string]any{"count": len(out), "channels": out}}, nil
}

// ── get_material_index ──────────────────────────────────────────────────────

type fileJSON struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	SizeBytes int64     `json:"size_bytes"`
	Caption   string    `json:"caption,omitempty"`
	Posted    time.Time `json:"posted"`
	Uploader  string    `json:"uploader,omitempty"`
	Thread    string    `json:"thread,omitempty"`
	URI       string    `json:"uri"`
}

func runIndex(e *callEnv) (*toolResult, error) {
	var a struct {
		Channel string `json:"channel"`
		Type    string `json:"type"`
		Query   string `json:"query"`
	}
	if err := decodeArgs(e.args, &a); err != nil {
		return nil, err
	}
	course, err := e.course(a.Channel)
	if err != nil {
		return nil, err
	}
	var wantType string
	switch strings.ToLower(a.Type) {
	case "":
	case "pdf":
		wantType = "application/pdf"
	case "image", "jpeg", "jpg":
		wantType = "image/jpeg"
	default:
		return nil, userError(`type must be "pdf" or "image"`)
	}
	files, err := e.rd.Index(e.ctx, course.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, userError("that channel has no material page")
	}
	if err != nil {
		return nil, err
	}

	q := fold(a.Query)
	out := make([]fileJSON, 0, len(files))
	var total int64
	var b strings.Builder
	for _, f := range files {
		if wantType != "" && f.Type != wantType {
			continue
		}
		if q != "" && !strings.Contains(fold(f.Name+" "+f.Caption+" "+f.Uploader+" "+f.Thread), q) {
			continue
		}
		total += f.Size
		out = append(out, fileJSON{
			ID: f.ID, Name: f.Name, Type: f.Type, SizeBytes: f.Size, Caption: f.Caption,
			Posted: f.Posted.UTC(), Uploader: f.Uploader, Thread: f.Thread, URI: materialURI(course.ID, f.ID),
		})
		fmt.Fprintf(&b, "- %s | id %s | %s | %s | %s", f.Name, f.ID, shortType(f.Type), humanSize(f.Size), f.Posted.UTC().Format("2006-01-02"))
		if f.Uploader != "" {
			fmt.Fprintf(&b, " | by %s", f.Uploader)
		}
		if f.Thread != "" {
			fmt.Fprintf(&b, " | thread %q", f.Thread)
		}
		if f.Caption != "" {
			fmt.Fprintf(&b, " | caption %q", oneLine(f.Caption))
		}
		b.WriteByte('\n')
	}
	head := fmt.Sprintf("#%s (id %s, category %s): %d files, %s.", course.Name, course.ID, course.Category, len(out), humanSize(total))
	if len(out) == 0 {
		head += " Nothing matches."
	}
	return &toolResult{
		text: head + "\n" + b.String(),
		data: map[string]any{
			"channel":     map[string]any{"id": course.ID, "name": course.Name, "category": course.Category},
			"count":       len(out),
			"total_bytes": total,
			"files":       out,
		},
	}, nil
}

// ── download_material ───────────────────────────────────────────────────────

func runDownload(e *callEnv) (*toolResult, error) {
	var a struct {
		Channel string `json:"channel"`
		FileID  string `json:"file_id"`
	}
	if err := decodeArgs(e.args, &a); err != nil {
		return nil, err
	}
	course, err := e.course(a.Channel)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(a.FileID)
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if !isSnowflake(id) {
		return nil, userError("file_id must be an id from get_material_index")
	}
	d, err := e.rd.Open(e.ctx, course.ID, id)
	if errors.Is(err, ErrNotFound) {
		return nil, userError("that channel's material has no such file; the file may have been deleted. Call get_material_index again")
	}
	if err != nil {
		return nil, err
	}
	if d.Size > maxDownload {
		d.Body.Close()
		return nil, userError(tooLarge(d.Size))
	}
	d.ChannelID = course.ID
	text := fmt.Sprintf("%s (%s, %s), posted %s in #%s", d.Name, shortType(d.Type), humanSize(d.Size), d.Posted.UTC().Format("2006-01-02"), course.Name)
	if d.Uploader != "" {
		text += " by " + d.Uploader
	}
	if d.Caption != "" {
		text += fmt.Sprintf(". Caption: %q", oneLine(d.Caption))
	}
	return &toolResult{text: text + ".", file: d}, nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

// course resolves a channel given as an id or a name against the channels the
// member can see.
func (e *callEnv) course(query string) (*Course, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, userError("channel is required: an id or name from list_course_channels")
	}
	courses, err := e.rd.Courses(e.ctx)
	if err != nil {
		return nil, err
	}
	return findCourse(courses, q)
}

func findCourse(courses []Course, query string) (*Course, error) {
	q := query
	// A mention (<#id>) or a hash-prefixed name is as good as a bare one.
	q = strings.TrimSpace(strings.Trim(strings.TrimPrefix(q, "<"), ">"))
	q = strings.TrimSpace(strings.TrimPrefix(q, "#"))
	lower := fold(q)
	for i := range courses {
		if courses[i].ID == q {
			return &courses[i], nil
		}
	}
	for i := range courses {
		if fold(courses[i].Name) == lower {
			return &courses[i], nil
		}
	}
	var hits []*Course
	for i := range courses {
		if strings.Contains(fold(courses[i].Name), lower) {
			hits = append(hits, &courses[i])
		}
	}
	switch len(hits) {
	case 0:
		return nil, userError(fmt.Sprintf("no course channel matches %q; call list_course_channels", query))
	case 1:
		return hits[0], nil
	}
	var names []string
	for i, h := range hits {
		if i == 8 {
			names = append(names, "…")
			break
		}
		names = append(names, fmt.Sprintf("#%s (id %s)", h.Name, h.ID))
	}
	return nil, userError(fmt.Sprintf("%q matches several channels: %s. Use an id", query, strings.Join(names, ", ")))
}

func tooLarge(size int64) string {
	return fmt.Sprintf("the file is %s, over the %s limit for one transfer", humanSize(size), humanSize(maxDownload))
}

func shortType(mime string) string {
	switch mime {
	case "application/pdf":
		return "pdf"
	case "image/jpeg":
		return "jpeg"
	}
	return mime
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// fold prepares text for matching: lower case, without Greek accents, and with
// the final sigma written as a plain one, so "αλγο" finds "Αλγόριθμοι".
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case 'ά':
			return 'α'
		case 'έ':
			return 'ε'
		case 'ή':
			return 'η'
		case 'ί', 'ϊ', 'ΐ':
			return 'ι'
		case 'ό':
			return 'ο'
		case 'ύ', 'ϋ', 'ΰ':
			return 'υ'
		case 'ώ':
			return 'ω'
		case 'ς':
			return 'σ'
		}
		return r
	}, strings.ToLower(strings.TrimSpace(s)))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func isForbidden(err error) bool { return errors.Is(err, ErrForbidden) }
func isNotFound(err error) bool  { return errors.Is(err, ErrNotFound) }
