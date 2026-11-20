package deployment

import (
	"encoding/json"
	"errors"
	"os"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/noderelease"
)

type updateReceipt struct {
	Job     string `json:"job"`
	Release string `json:"release"`
	State   string `json:"state"`
}

const receiptPath = updateCache + "/receipt.json"

// previousReceipt 只为同一作业重发相同结果。服务端丢失回执不能导致节点重新执行停机。
func (fs filesystem) previousReceipt(a noderelease.Assignment) (string, error) {
	if _, err := os.Lstat(fs.resolve(receiptPath)); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	b, err := fs.readConfig(fileRule{path: receiptPath, maximum: 1024, private: true}, account{})
	if err != nil {
		return "", err
	}
	var r updateReceipt
	if err := decodePreparation(b, &r); err != nil {
		return "", errors.New("已有升级回执无法读取")
	}
	_, _, valid := noderelease.ParseUpgradeOutcome(r.State)
	if (computer.NodeQuery{NodeID: r.Job}).Validate() != nil || !noderelease.ValidDigest(r.Release) || !valid {
		return "", errors.New("已有升级回执无效，未重新执行")
	}
	if r.Job != a.Job {
		return "", nil
	}
	if r.Release != a.Release {
		return "", errors.New("服务端升级任务对应的发布包已改变")
	}
	return r.State, nil
}
func (fs filesystem) recordReceipt(a noderelease.Assignment, state string) error {
	b, err := json.Marshal(updateReceipt{a.Job, a.Release, state})
	if err != nil {
		return err
	}
	return (serverInstaller{fs: fs}).replaceServerFile(receiptPath, b, 0600)
}
