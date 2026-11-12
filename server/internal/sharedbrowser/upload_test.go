package sharedbrowser

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestUploadCanonicalPayloadAndLimits(t *testing.T) {
	valid := UploadRequest{Sequence: 1, Selector: "#files", Files: []UploadFile{{Name: "中文.txt", Content: []byte("shared contents")}}}
	for _, mode := range []string{"正常", "空文件", "边界大小", "超限", "无文件", "文件数量", "路径", "空名称", "重复名称", "错误序号", "错误选择器"} {
		t.Run(mode, func(t *testing.T) {
			request := valid
			request.Files = append([]UploadFile(nil), valid.Files...)
			switch mode {
			case "空文件":
				request.Files[0].Content = []byte{}
			case "边界大小":
				request.Files[0].Content = make([]byte, MaximumUploadBytes)
			case "超限":
				request.Files[0].Content = make([]byte, MaximumUploadBytes+1)
			case "无文件":
				request.Files = nil
			case "文件数量":
				request.Files = make([]UploadFile, MaximumUploadFiles+1)
			case "路径":
				request.Files[0].Name = "../other"
			case "空名称":
				request.Files[0].Name = ""
			case "重复名称":
				request.Files = append(request.Files, request.Files[0])
			case "错误序号":
				request.Sequence = 0
			case "错误选择器":
				request.Selector = ""
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeUpload(encoded)
			want := mode == "正常" || mode == "空文件" || mode == "边界大小"
			if (err == nil) != want {
				t.Fatal(mode, err)
			}
		})
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{append([]byte(" "), encoded...), bytes.Replace(encoded, []byte(`"sequence":1`), []byte(`"sequence":1,"sequence":2`), 1), bytes.Replace(encoded, []byte(`"name"`), []byte(`"Name"`), 1), bytes.Replace(encoded, []byte(`"files"`), []byte(`"path":"/etc/private","files"`), 1), append(encoded, []byte("{}")...)} {
		if _, err := decodeUpload(changed); !errors.Is(err, ErrInvalid) {
			t.Fatal("非规范文件正文被接受", err)
		}
	}
}
