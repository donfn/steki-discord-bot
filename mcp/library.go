package mcp

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrForbidden is returned by Library.Reader for someone who may not read the
// course material: not in the server, or neither a verified member nor a
// verified outsider.
var ErrForbidden = errors.New("mcp: not allowed to read the course material")

// ErrNotFound is returned by a Reader for a channel that is not a course
// channel the member can see, or a file the channel does not list.
var ErrNotFound = errors.New("mcp: no such material")

// Library is the bot side of the MCP server: it knows who may read the
// material and where it is kept.
type Library interface {
	// Reader returns the member's view of the material, or ErrForbidden. It is
	// called with every request, so a member who leaves the server or loses
	// their role stops being served at once, whatever their token says.
	Reader(ctx context.Context, userID string) (Reader, error)
}

// Reader is what one member may read. It only ever shows channels the member
// can see in Discord.
type Reader interface {
	// Courses lists the course channels.
	Courses(ctx context.Context) ([]Course, error)
	// Index lists a course channel's material, newest first.
	Index(ctx context.Context, channelID string) ([]File, error)
	// Open returns one file of a course channel. The caller closes Body.
	Open(ctx context.Context, channelID, fileID string) (*Download, error)
}

// Course is a course channel.
type Course struct {
	ID       string
	Name     string
	Category string
	// Files and Bytes count the listed material; Latest is when the newest
	// file was posted.
	Files  int
	Bytes  int64
	Latest time.Time
}

// File is one piece of material.
type File struct {
	ID string
	// ChannelID is the course channel the file belongs to.
	ChannelID string
	// Name is the file's name as it was posted.
	Name string
	// Type is the MIME type: application/pdf or image/jpeg.
	Type     string
	Size     int64
	Caption  string
	Posted   time.Time
	Uploader string
	// Thread names the thread the file was posted in, if not the channel itself.
	Thread string
}

// Download is an open file.
type Download struct {
	File
	Body io.ReadCloser
}
