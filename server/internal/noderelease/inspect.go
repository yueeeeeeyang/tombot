package noderelease

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/infra/jsonshape"
)

// Inspect 从归档实际内容计算每项摘要，仅暂存小清单和 ELF 头。早期标准包缺少版本标签，调用者用原包摘要显示。
func Inspect(ctx context.Context, source io.Reader) (Manifest, error) {
	hashes := map[string]string{}
	var sums, profileBytes, executable []byte
	err := walkArchive(ctx, source, func(h *tar.Header, entry io.Reader) error {
		hash := sha256.New()
		entry = io.TeeReader(entry, hash)
		var err error
		switch h.Name {
		case "SHA256SUMS":
			sums, err = io.ReadAll(entry)
		case "node-release.json":
			profileBytes, err = io.ReadAll(entry)
		case "tomnode":
			executable = make([]byte, 64)
			_, err = io.ReadFull(entry, executable)
		}
		if err != nil {
			return err
		}
		if _, err = io.Copy(io.Discard, entry); err != nil {
			return err
		}
		hashes[h.Name] = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	if err != nil {
		return Manifest{}, errors.Join(ErrPackage, err)
	}
	expected, err := ParseChecksums(sums)
	checksum := hashes["SHA256SUMS"]
	delete(hashes, "SHA256SUMS")
	if err != nil || !maps.Equal(hashes, expected) {
		return Manifest{}, ErrPackage
	}
	var profile Profile
	if jsonshape.Validate(profileBytes, &profile) != nil || json.Unmarshal(profileBytes, &profile) != nil {
		return Manifest{}, ErrPackage
	}
	if profile.Version != 1 || (profile.Label != "" && !labelPattern.MatchString(profile.Label)) {
		return Manifest{}, ErrPackage
	}
	if profile.ArchiveSHA256 != hashes["computer-image.tar"] || computer.ValidateImageReference(profile.ImageReference) != nil || ValidateExecutable(executable, profile.Platform) != nil {
		return Manifest{}, ErrPackage
	}
	if _, err = dockerbootstrap.ParseManifest(profile.Docker); err != nil || jsonshape.Validate(profile.Docker, &dockerbootstrap.Manifest{}) != nil {
		return Manifest{}, ErrPackage
	}
	return Manifest{Version: 1, Label: profile.Label, Platform: profile.Platform, Checksums: checksum}, nil
}
