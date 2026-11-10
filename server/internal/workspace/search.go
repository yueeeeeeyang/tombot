package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"io.yueyang/tom/internal/domain"
)

const MaxEntries = 10000

type Entry struct {
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}
type Listing struct {
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

// walk 使用有界批次读取目录，不跟随符号链接，避免目录环与无界内存加载。
func walk(ctx context.Context, root *os.Root, path string, depth int, visited *int, depthReached *bool, visit func(string, os.FileInfo) error) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fileError(err)
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			if name == ".git" || name == "node_modules" || name == "__pycache__" || strings.HasPrefix(name, ".tom-write-") {
				continue
			}
			*visited++
			if *visited > MaxEntries {
				return failure("TREE_LIMIT", "The traversal reached its entry limit. Use a narrower path.")
			}
			stat, err := entry.Info()
			if err != nil {
				return fileError(err)
			}
			child := filepath.Join(path, name)
			if err = visit(child, stat); err != nil {
				return err
			}
			if stat.IsDir() && depth == 1 {
				*depthReached = true
			}
			if stat.IsDir() && depth > 1 {
				if err = walk(ctx, root, child, depth-1, visited, depthReached, visit); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fileError(readErr)
		}
	}
}
func isCode(err error, code string) bool {
	var public *domain.Error
	return errors.As(err, &public) && public.Code == code
}
func (f *Files) List(ctx context.Context, botID, path string, depth int) (result Listing, err error) {
	if depth == 0 {
		depth = 2
	}
	if depth < 1 || depth > 16 {
		return result, failure("INVALID_INPUT", "Directory depth must be between 1 and 16.")
	}
	root, name, err := f.scope(botID, path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	result.Entries = []Entry{}
	visited := 0
	requested := path
	err = walk(ctx, root, name, depth, &visited, &result.Truncated, func(path string, stat os.FileInfo) error {
		kind := "file"
		if stat.IsDir() {
			kind = "directory"
		} else if stat.Mode()&os.ModeSymlink != 0 {
			kind = "symlink"
		} else if !stat.Mode().IsRegular() {
			kind = "special"
		}
		result.Entries = append(result.Entries, Entry{Path: f.displayPath(requested, path), Kind: kind, Size: stat.Size(), ModifiedAt: stat.ModTime().UTC()})
		return nil
	})
	if isCode(err, "TREE_LIMIT") {
		result.Truncated = true
		err = nil
	}
	sort.Slice(result.Entries, func(a, b int) bool {
		if result.Entries[a].ModifiedAt.Equal(result.Entries[b].ModifiedAt) {
			return result.Entries[a].Path < result.Entries[b].Path
		}
		return result.Entries[a].ModifiedAt.After(result.Entries[b].ModifiedAt)
	})
	return result, err
}

func (f *Files) Glob(ctx context.Context, botID, path, pattern string) (result Listing, err error) {
	if pattern == "" || len(pattern) > 512 {
		return result, failure("INVALID_INPUT", "A bounded glob pattern is required.")
	}
	if _, err = filepath.Match(pattern, ""); err != nil {
		return result, failure("INVALID_PATTERN", "The glob pattern is invalid.")
	}
	result, err = f.List(ctx, botID, path, 16)
	if err != nil {
		return result, err
	}
	matches := []Entry{}
	for _, entry := range result.Entries {
		candidate := filepath.Base(entry.Path)
		if strings.Contains(pattern, "/") {
			candidate = entry.Path
		}
		matched, matchErr := filepath.Match(pattern, candidate)
		if matchErr != nil {
			return result, matchErr
		}
		if matched {
			matches = append(matches, entry)
		}
	}
	result.Entries = matches
	return result, nil
}

type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}
type SearchResult struct {
	Matches      []Match `json:"matches"`
	Truncated    bool    `json:"truncated"`
	SkippedFiles int     `json:"skipped_files"`
}

func (f *Files) Search(ctx context.Context, botID, path, pattern string, limit int) (result SearchResult, err error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 || pattern == "" || len(pattern) > 4096 {
		return result, failure("INVALID_INPUT", "Provide a bounded regular expression and a match limit from 1 to 1000.")
	}
	expression, err := regexp.Compile(pattern)
	if err != nil {
		return result, failure("INVALID_PATTERN", "The regular expression is invalid.")
	}
	root, name, err := f.scope(botID, path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	result.Matches = []Match{}
	visited := 0
	requested := path
	err = walk(ctx, root, name, 16, &visited, &result.Truncated, func(path string, stat os.FileInfo) error {
		if !stat.Mode().IsRegular() {
			return nil
		}
		content, readErr := readText(ctx, root, path)
		if isCode(readErr, "NOT_TEXT_FILE") || isCode(readErr, "FILE_TOO_LARGE") {
			result.SkippedFiles++
			return nil
		}
		if readErr != nil {
			return readErr
		}
		for line, text := range strings.Split(string(content), "\n") {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !expression.MatchString(text) {
				continue
			}
			if len(result.Matches) == limit {
				return failure("MATCH_LIMIT", "The search reached its match limit.")
			}
			if len(text) > 4096 {
				text = string([]rune(text)[:min(1000, len([]rune(text)))]) + " [line truncated]"
				result.Truncated = true
			}
			result.Matches = append(result.Matches, Match{Path: f.displayPath(requested, path), Line: line + 1, Text: text})
		}
		return nil
	})
	if isCode(err, "TREE_LIMIT") || isCode(err, "MATCH_LIMIT") {
		result.Truncated = true
		err = nil
	}
	return result, err
}
