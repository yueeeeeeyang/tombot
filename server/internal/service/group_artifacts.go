package service

import (
	"context"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
)

type GroupArtifactPage struct {
	Artifacts []Artifact `json:"artifacts"`
	Next      int64      `json:"next"`
}

// Artifacts 只从本群已完成消息提取真实文件，按消息序号分页，不混入成员私聊资料。
func (s *Groups) Artifacts(ctx context.Context, id string, before int64) (GroupArtifactPage, error) {
	page := GroupArtifactPage{Artifacts: []Artifact{}}
	group, err := s.Get(ctx, id)
	if err != nil {
		return page, err
	}
	if before < 0 {
		return page, badInput("Invalid artifact cursor.")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,seq,blocks_json,created_at FROM messages WHERE conversation_id=? AND status='complete' AND (?=0 OR seq<?) AND EXISTS(SELECT 1 FROM json_each(blocks_json) b WHERE json_extract(b.value,'$.artifact.path') IS NOT NULL OR json_extract(b.value,'$.tool_result.artifact.path') IS NOT NULL) ORDER BY seq DESC LIMIT 51`, group.ConversationID, before, before)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	count := 0
	var last int64
	for rows.Next() {
		var messageID, encoded string
		var seq, created int64
		if err = rows.Scan(&messageID, &seq, &encoded, &created); err != nil {
			return page, err
		}
		count++
		if count > 50 {
			page.Next = last
			break
		}
		last = seq
		var blocks []domain.MessageBlock
		if err = json.Unmarshal([]byte(encoded), &blocks); err != nil {
			return page, err
		}
		for index, block := range blocks {
			ref := block.Artifact
			if ref == nil && block.ToolResult != nil {
				ref = block.ToolResult.Artifact
			}
			if ref == nil || !strings.HasPrefix(ref.Path, "shared/") {
				continue
			}
			kind := "file"
			if ref.IsImage() {
				kind = "image"
			} else if strings.HasPrefix(ref.MediaType, "video/") {
				kind = "video"
			}
			page.Artifacts = append(page.Artifacts, Artifact{ID: messageID + ":" + strconv.Itoa(index), Title: path.Base(ref.Path), Kind: kind, CreatedAt: time.UnixMilli(created).UTC().Format(time.RFC3339Nano)})
		}
	}
	return page, rows.Err()
}
