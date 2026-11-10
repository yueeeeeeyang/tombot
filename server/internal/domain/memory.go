package domain

import "time"

const MaxMemoryFiles = 512
const MaxMemoryTotalBytes = 8 << 20
const MaxMemoryFileBytes = 1 << 20
const MaxMemoryIndexBytes = 2048

type Memory struct {
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	Tags       []string  `json:"tags"`
	Importance int       `json:"importance"`
	SizeBytes  int       `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	UpdatedAt  time.Time `json:"updated_at"`
	TooLong    bool      `json:"too_long"`
	Content    string    `json:"content,omitempty"`
	Snippet    string    `json:"snippet,omitempty"`
}

type MemoryInput struct {
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	Content    string   `json:"content"`
	Tags       []string `json:"tags"`
	Importance int      `json:"importance"`
}
