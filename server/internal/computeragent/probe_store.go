package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
)

const probeRecordName = "probe.json"

func (store *FileStore) LoadProbe(ctx context.Context) (*ProbeSnapshot, error) {
	encoded, err := store.readRecord(ctx, probeRecordName)
	if err != nil || encoded == nil {
		return nil, err
	}
	var snapshot ProbeSnapshot
	if err := agentchannel.Decode(encoded, &snapshot); err != nil {
		return nil, err
	}
	if err := snapshot.validate(); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (store *FileStore) SaveProbe(ctx context.Context, snapshot ProbeSnapshot) error {
	if err := snapshot.validate(); err != nil {
		return err
	}
	encoded, err := agentchannel.Encode(snapshot)
	if err != nil || len(encoded) > 16384 {
		return errors.New("电脑代理探针记录超过限制")
	}
	return store.writeRecord(ctx, probeRecordName, encoded)
}
