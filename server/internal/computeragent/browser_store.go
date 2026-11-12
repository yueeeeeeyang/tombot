package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
)

const browserRecordName = "browser.json"

func (store *FileStore) LoadBrowser(ctx context.Context) (*BrowserSnapshot, error) {
	encoded, err := store.readRecord(ctx, browserRecordName)
	if err != nil || encoded == nil {
		return nil, err
	}
	var snapshot BrowserSnapshot
	if err := agentchannel.Decode(encoded, &snapshot); err != nil {
		return nil, err
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (store *FileStore) SaveBrowser(ctx context.Context, snapshot BrowserSnapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	encoded, err := agentchannel.Encode(snapshot)
	if err != nil || len(encoded) > 16384 {
		return errors.New("共享浏览器持久服务记录超过限制")
	}
	return store.writeRecord(ctx, browserRecordName, encoded)
}
