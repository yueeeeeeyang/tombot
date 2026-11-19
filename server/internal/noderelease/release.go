// Package noderelease 验证节点升级包的架构和有序分片。
package noderelease

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/dockerbootstrap"
)

const ChunkSize = 4 << 20
const MaxSize int64 = 17 << 30

const MaxManifest = 600 << 10

var digestPattern = regexp.MustCompile("^[0-9a-f]{64}$")
var labelPattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$")
var ErrPackage = errors.New("节点离线包格式或校验无效")

const Programs = "programs"
const Image = "image"
const ProgramSums = "PROGRAM-SHA256SUMS"
const ProgramRoot = "/usr/lib/tombot/programs"
const ProgramImageLabel = "io.tombot.computer.external-programs"

var ComputerPrograms = [...]string{"computer-agent", "computer-task", "computer-probe", "bot-agent"}

type ProgramProfile struct {
	Label    string `json:"label"`
	Version  int    `json:"version"`
	Platform string `json:"platform"`
}

func (p ProgramProfile) Valid(platform string) bool {
	return p.Version == 2 && labelPattern.MatchString(p.Label) && p.Platform == platform && (platform == "linux/amd64" || platform == "linux/arm64")
}

func SumFile(kind string) string {
	if kind == Programs {
		return ProgramSums
	}
	return "SHA256SUMS"
}

type Manifest struct {
	RuntimeSHA256  string   `json:"runtime_sha256,omitempty"`
	ImageReference string   `json:"image_reference,omitempty"`
	ArchiveSHA256  string   `json:"archive_sha256,omitempty"`
	Kind           string   `json:"kind,omitempty"`
	Checksums      string   `json:"checksums"`
	Version        int      `json:"version"`
	Label          string   `json:"label"`
	Platform       string   `json:"platform"`
	Size           int64    `json:"size"`
	Chunks         []string `json:"chunks"`
}

func Digest(content []byte) string          { sum := sha256.Sum256(content); return hex.EncodeToString(sum[:]) }
func ValidDigest(value string) bool         { return digestPattern.MatchString(value) }
func (m Manifest) PartSize(index int) int64 { return min(ChunkSize, m.Size-int64(index)*ChunkSize) }

// Validate 将任务绑定到已核验归档的有序分片，限制读盘范围和分配大小。
func (m Manifest) Validate(artifact string) error {
	switch m.Kind {
	case "":
		if m.Version != 1 {
			return ErrPackage
		}
	case Programs:
		if m.Version != 2 {
			return ErrPackage
		}
	case Image:
		if m.Version != 2 || computer.ValidateImageReference(m.ImageReference) != nil || !ValidDigest(m.ArchiveSHA256) || !ValidDigest(m.RuntimeSHA256) {
			return ErrPackage
		}
	default:
		return ErrPackage
	}
	if m.Kind != Image && (m.ImageReference != "" || m.ArchiveSHA256 != "" || m.RuntimeSHA256 != "") {
		return ErrPackage
	}
	if !ValidDigest(m.Checksums) || !labelPattern.MatchString(m.Label) || (m.Platform != "linux/amd64" && m.Platform != "linux/arm64") || m.Size < 1 || m.Size > MaxSize || int64(len(m.Chunks)) != (m.Size+ChunkSize-1)/ChunkSize {
		return ErrPackage
	}
	for _, chunk := range m.Chunks {
		if !ValidDigest(chunk) {
			return ErrPackage
		}
	}
	if Digest([]byte(strings.Join(m.Chunks, "\n"))) != artifact {
		return ErrPackage
	}
	return nil
}

// Profile 随离线包交付，同一份清单供上传验包和本机安装读取。
type Profile struct {
	Label          string          `json:"label,omitempty"`
	Version        int             `json:"version"`
	Platform       string          `json:"platform"`
	ImageReference string          `json:"image_reference"`
	ArchiveSHA256  string          `json:"archive_sha256"`
	Docker         json.RawMessage `json:"docker"`
}

// ValidateExecutable 在执行前核对 Linux ELF 头、可执行类型与架构。
func ValidateExecutable(content []byte, platform string) error {
	machine := map[string]uint16{"linux/amd64": 62, "linux/arm64": 183}[platform]
	if machine == 0 || len(content) < 64 || !bytes.Equal(content[:7], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1}) || binary.LittleEndian.Uint16(content[18:20]) != machine {
		return errors.New("节点安装程序必须是与宿主架构匹配的 Linux 二进制")
	}
	if kind := binary.LittleEndian.Uint16(content[16:18]); kind != 2 && kind != 3 {
		return errors.New("节点安装程序必须是可执行文件")
	}
	return nil
}

// ParseChecksums 解析包内文件摘要并拒绝重复项。
func ParseChecksums(encoded []byte) (map[string]string, error) {
	files := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(encoded), "\n"), "\n") {
		digest, name, ok := strings.Cut(line, "  ")
		if !ok || !ValidDigest(digest) || files[name] != "" {
			return nil, errors.New("节点发布包校验清单无效")
		}
		files[name] = digest
	}
	return files, nil
}

// RuntimeDigest 规范化镜像附带的宿主安装预算，判断复用镜像是否也能复用本机配置。
func RuntimeDigest(raw json.RawMessage) (string, error) {
	manifest, err := dockerbootstrap.ParseManifest(raw)
	if err != nil {
		return "", err
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return Digest(content), nil
}
