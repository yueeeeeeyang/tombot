package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/memory"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/workspace"
)

// Memories 以文件为权威，在同一事务内维护元数据和两套派生全文索引。
type Memories struct {
	db     *sql.DB
	bots   *Bots
	files  *workspace.Files
	events *bus.Bus
	mu     sync.Mutex
}

// NewMemories 绑定持久索引、Bot 生命周期和私有工作区；所有依赖由应用启动统一创建。
func NewMemories(db *sql.DB, bots *Bots, files *workspace.Files, events *bus.Bus) *Memories {
	return &Memories{db: db, bots: bots, files: files, events: events}
}

func memoryEntry(file workspace.MemoryFile, doc memory.Document) domain.Memory {
	return domain.Memory{Path: file.Path, Title: doc.Title, Summary: doc.Summary, Tags: doc.Tags, Importance: doc.Importance, SizeBytes: file.Bytes, SHA256: file.SHA256, UpdatedAt: file.ModifiedAt, TooLong: doc.TooLong, Content: doc.Body}
}

// Sync 校正手工修改与删除，禁止以旧索引掩盖文件读取或解析失败；重建时同时清理残留 FTS 行。
func (s *Memories) Sync(ctx context.Context, botID string, rebuild bool) ([]domain.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.bots.beginFileWrite(ctx, botID)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.syncLocked(ctx, botID, rebuild)
}

func (s *Memories) syncLocked(ctx context.Context, botID string, rebuild bool) ([]domain.Memory, error) {
	files, err := s.files.MemoryFiles(ctx, botID)
	if err != nil {
		return nil, err
	}
	entries := make([]domain.Memory, 0, len(files))
	for _, file := range files {
		doc, err := memory.Parse(file.Path, file.Content)
		if err != nil {
			return nil, err
		}
		entries = append(entries, memoryEntry(file, doc))
	}
	changed := false
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		bot, err := ReadBot(ctx, tx, botID)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT path,content_hash,updated_at FROM memory_index WHERE bot_id=?", botID)
		if err != nil {
			return err
		}
		type fingerprint struct {
			hash    string
			updated int64
		}
		previous := map[string]fingerprint{}
		for rows.Next() {
			var path, hash string
			var updated int64
			if err = rows.Scan(&path, &hash, &updated); err != nil {
				break
			}
			previous[path] = fingerprint{hash, updated}
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return err
		}
		if rebuild {
			for _, table := range []string{"memory_fts_ascii", "memory_fts_cjk"} {
				if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE bot_id=?", botID); err != nil {
					return err
				}
			}
			changed = true
		}
		for _, entry := range entries {
			old, existed := previous[entry.Path]
			delete(previous, entry.Path)
			if !rebuild && existed && old.hash == entry.SHA256 && old.updated == entry.UpdatedAt.UnixMilli() {
				continue
			}
			changed = true
			tags, err := json.Marshal(entry.Tags)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO memory_index(bot_id,path,title,summary,tags,importance,size_bytes,content_hash,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(bot_id,path) DO UPDATE SET title=excluded.title,summary=excluded.summary,tags=excluded.tags,importance=excluded.importance,size_bytes=excluded.size_bytes,content_hash=excluded.content_hash,updated_at=excluded.updated_at`, botID, entry.Path, entry.Title, entry.Summary, string(tags), entry.Importance, entry.SizeBytes, entry.SHA256, entry.UpdatedAt.UnixMilli()); err != nil {
				return err
			}
			for _, table := range []string{"memory_fts_ascii", "memory_fts_cjk"} {
				if !rebuild {
					if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE bot_id=? AND path=?", botID, entry.Path); err != nil {
						return err
					}
				}
				if _, err = tx.ExecContext(ctx, "INSERT INTO "+table+"(bot_id,path,title,summary,tags,content) VALUES(?,?,?,?,?,?)", botID, entry.Path, entry.Title, entry.Summary, strings.Join(entry.Tags, " "), entry.Content); err != nil {
					return err
				}
			}
		}
		for path := range previous {
			changed = true
			for _, table := range []string{"memory_index", "memory_fts_ascii", "memory_fts_cjk"} {
				if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE bot_id=? AND path=?", botID, path); err != nil {
					return err
				}
			}
		}
		if changed {
			_, err = bus.Append(ctx, tx, "memory.updated", bot.ConversationID, map[string]any{"bot_id": botID, "count": len(entries), "rebuilt": rebuild})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if changed {
		s.events.Notify()
	}
	return entries, nil
}

// List 校正指定 Bot 的索引并返回不含正文的条目；文件校验失败时不返回旧索引。
func (s *Memories) List(ctx context.Context, botID string) ([]domain.Memory, error) {
	entries, err := s.Sync(ctx, botID, false)
	if err != nil {
		return nil, err
	}
	for index := range entries {
		entries[index].Content = ""
	}
	return entries, nil
}

// Read 返回当前正文并记录使用时间；路径无效、文件缺失或校验失败均明确报错。
func (s *Memories) Read(ctx context.Context, botID, path string) (domain.Memory, error) {
	if !memory.ValidPath(path) {
		return domain.Memory{}, domain.NewError(400, "MEMORY_PATH_INVALID", "Use a Markdown file directly under .tom/memory/.")
	}
	entries, err := s.Sync(ctx, botID, false)
	if err != nil {
		return domain.Memory{}, err
	}
	for _, entry := range entries {
		if entry.Path == path {
			if _, err = s.db.ExecContext(ctx, "UPDATE memory_index SET last_used_at=? WHERE bot_id=? AND path=?", time.Now().UnixMilli(), botID, path); err != nil {
				return domain.Memory{}, err
			}
			return entry, nil
		}
	}
	return domain.Memory{}, domain.NewError(404, "MEMORY_NOT_FOUND", "The private memory does not exist.")
}

func (s *Memories) audit(actor, action, botID string) workspace.BeforeWrite {
	return func(ctx context.Context, path, hash string) error {
		return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
			return store.Audit(ctx, tx, actor, action, botID, map[string]string{"path": path, "old_sha256": hash})
		})
	}
}

// Write 在空路径时按标题创建，否则按 expected 摘要更新；文件已保存但索引失败会返回专用错误，禁止盲目重试。
func (s *Memories) Write(ctx context.Context, botID, path string, input domain.MemoryInput, expected, actor string) (domain.Memory, error) {
	if path == "" {
		path = memory.Slug(input.Title)
	}
	if !memory.ValidPath(path) {
		return domain.Memory{}, domain.NewError(400, "MEMORY_PATH_INVALID", "Use a Markdown file directly under .tom/memory/.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.bots.beginFileWrite(ctx, botID)
	if err != nil {
		return domain.Memory{}, err
	}
	defer release()
	var previous *memory.Document
	old, err := s.files.ReadMemory(ctx, botID, path)
	if err == nil {
		if expected == "" || expected != old.SHA256 {
			return domain.Memory{}, domain.NewError(409, "MEMORY_CHANGED", "The memory changed or already exists. Read it before writing again.")
		}
		doc, err := memory.Parse(path, old.Content)
		if err != nil {
			return domain.Memory{}, err
		}
		previous = &doc
	} else {
		var public *domain.Error
		if expected != "" || !errors.As(err, &public) || public.Code != "FILE_NOT_FOUND" {
			return domain.Memory{}, err
		}
	}
	content, err := memory.Render(previous, input)
	if err != nil {
		return domain.Memory{}, err
	}
	files, err := s.files.MemoryFiles(ctx, botID)
	if err != nil {
		return domain.Memory{}, err
	}
	total, count := len(content), 1
	for _, file := range files {
		if file.Path != path {
			total += file.Bytes
			count++
		}
	}
	if count > domain.MaxMemoryFiles || total > domain.MaxMemoryTotalBytes {
		return domain.Memory{}, domain.NewError(400, "MEMORY_CAPACITY", "The bot's private memory exceeds its file count or total byte limit.")
	}
	if _, err = s.files.WriteMemory(ctx, botID, path, content, expected, s.audit(actor, "memory.write", botID)); err != nil {
		return domain.Memory{}, err
	}
	persist, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entries, err := s.syncLocked(persist, botID, false)
	if err != nil {
		return domain.Memory{}, errors.Join(domain.NewError(500, "MEMORY_SAVED_INDEX_FAILED", "The memory file was saved but its index update failed. Refresh before repeating the write."), err)
	}
	for _, entry := range entries {
		if entry.Path == path {
			return entry, nil
		}
	}
	return domain.Memory{}, domain.NewError(409, "MEMORY_CHANGED", "The memory was removed by another writer after saving.")
}

// Delete 以摘要约束删除并同步派生索引；审计、版本冲突和文件提交后的索引失败分别向调用方返回。
func (s *Memories) Delete(ctx context.Context, botID, path, expected, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.bots.beginFileWrite(ctx, botID)
	if err != nil {
		return err
	}
	defer release()
	if err = s.files.DeleteMemory(ctx, botID, path, expected, s.audit(actor, "memory.delete", botID)); err != nil {
		return err
	}
	persist, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err = s.syncLocked(persist, botID, false); err != nil {
		return errors.Join(domain.NewError(500, "MEMORY_SAVED_INDEX_FAILED", "The memory file was deleted but its index update failed. Refresh before repeating the deletion."), err)
	}
	return nil
}

// FileChanged 接收普通文件工具和用户编辑的完成钩子；Shell 等外部修改由按需和定期扫描校正。
func (s *Memories) FileChanged(ctx context.Context, botID, path string) error {
	canonical, _, err := s.files.Describe(botID, path)
	if err != nil {
		return err
	}
	if !memory.ValidPath(canonical) {
		return nil
	}
	_, err = s.Sync(ctx, botID, false)
	if err != nil {
		return errors.Join(domain.NewError(500, "MEMORY_SAVED_INDEX_FAILED", "The file was saved but private memory indexing failed. Refresh before repeating the edit."), err)
	}
	return nil
}
