package dockernode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"io.yueyang/tom/internal/computer"
)

type imageDescription struct {
	ID           string `json:"Id"`
	RepoDigests  []string
	OS           string `json:"Os"`
	Architecture string
	Variant      string
	Config       struct {
		User       string
		Entrypoint []string
		Cmd        []string
		WorkingDir string
		Labels     map[string]string
	}
}

func validDockerImageID(value string) bool {
	return len(value) == 71 && strings.HasPrefix(value, "sha256:") && strings.Trim(value[7:], "0123456789abcdef") == ""
}

// validate 核验固定摘要、平台和代理入口；不把镜像元数据当作实际工具与电脑能力证明。
func (image imageDescription) validate(reference, platform string) error {
	if computer.ValidateImageReference(reference) != nil || (platform != "linux/amd64" && platform != "linux/arm64") {
		return failure("image_request_invalid")
	}
	if !validDockerImageID(image.ID) || !slices.Contains(image.RepoDigests, reference) {
		return failure("image_digest_mismatch")
	}
	if image.OS+"/"+image.Architecture != platform || image.Variant != "" && !(platform == "linux/arm64" && image.Variant == "v8") {
		return failure("image_platform_mismatch")
	}
	if !slices.Equal(image.Config.Entrypoint, []string{"/usr/lib/tombot/computer-agent"}) || len(image.Config.Cmd) != 0 || image.Config.WorkingDir != "/workspace" || image.Config.User != "" && image.Config.User != "0:0" {
		return failure("image_agent_contract_mismatch")
	}
	if image.Config.Labels[computer.StorageLayoutLabel] != computer.StorageLayoutVersion {
		return failure("image_storage_layout_mismatch")
	}
	return nil
}

func (engine *dockerEngine) inspectImage(ctx context.Context, reference string) (imageDescription, error) {
	var image imageDescription
	err := engine.request(ctx, http.MethodGet, "/images/"+url.PathEscape(reference)+"/json", nil, &image, http.StatusOK)
	return image, err
}

// pullImage 只拉取清单中的一个固定摘要，不读取 CLI 配置，不导入归档、不改标签，也不重放未知拉取。
func (engine *dockerEngine) pullImage(ctx context.Context, reference, platform, authentication string) (result error) {
	if computer.ValidateImageReference(reference) != nil || platform != "linux/amd64" && platform != "linux/arm64" {
		return failure("image_request_invalid")
	}
	if _, ok := ctx.Deadline(); !ok {
		return failure("image_pull_deadline_missing")
	}
	query := url.Values{"fromImage": {reference}, "platform": {platform}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/v1.47/images/create?"+query.Encode(), nil)
	if err != nil {
		return failure("image_pull_request_invalid")
	}
	if authentication != "" {
		request.Header.Set("X-Registry-Auth", authentication)
	}
	// 大镜像的总时限由调用上下文限定；连接及响应头仍沿用节点传输层的短时限。
	client := *engine.client
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure("image_pull_result_unknown")
	}
	defer func() {
		if response.Body.Close() != nil {
			result = errors.Join(result, failure("image_pull_response_close_failed"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return failure("image_pull_rejected")
	}
	if err := readImageProgress(response.Body); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return ctx.Err()
}

// readImageProgress 消费有界流，只识别错误；最终成功必须另行 Inspect，不能靠进度文案或 HTTP 200。
func readImageProgress(reader io.Reader) error {
	const maximumStream = 64 << 20
	limited := &io.LimitedReader{R: reader, N: maximumStream + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	count := 0
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			return failure("image_pull_progress_invalid")
		}
		var message struct {
			Status      string           `json:"status"`
			Error       string           `json:"error"`
			ErrorDetail *json.RawMessage `json:"errorDetail"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return failure("image_pull_progress_invalid")
		}
		if message.Error != "" || message.ErrorDetail != nil {
			return failure("image_pull_stream_failed")
		}
		if message.Status == "" {
			return failure("image_pull_progress_invalid")
		}
		count++
	}
	if scanner.Err() != nil || limited.N <= 0 || count == 0 {
		return failure("image_pull_stream_incomplete")
	}
	return nil
}
