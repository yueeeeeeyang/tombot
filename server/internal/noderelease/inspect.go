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
	"slices"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/infra/jsonshape"
)

// Inspect 从归档实际内容计算每项摘要，仅暂存小清单和 ELF 头。早期标准包缺少版本标签，调用者用原包摘要显示。
func Inspect(ctx context.Context, source io.Reader) (Manifest, error) {
	hashes := map[string]string{}
	var sums, profileBytes []byte
	headers := map[string][]byte{}
	kind := Image
	err := walkArchive(ctx, source, func(h *tar.Header, entry io.Reader) error {
		hash := sha256.New()
		entry = io.TeeReader(entry, hash)
		var err error
		switch h.Name {
		case "SHA256SUMS", ProgramSums:
			sums, err = io.ReadAll(entry)
		case "node-release.json", "program-release.json":
			profileBytes, err = io.ReadAll(entry)
		default:
			if h.Name == "tomnode" || slices.Contains(ComputerPrograms[:], h.Name) {
				headers[h.Name] = make([]byte, 64)
				_, err = io.ReadFull(entry, headers[h.Name])
			}
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
	if _, ok := hashes[ProgramSums]; ok {
		kind = Programs
	} else if _, ok := headers["tomnode"]; ok {
		kind = ""
	}
	checksum := hashes[SumFile(kind)]
	delete(hashes, SumFile(kind))
	if err != nil || !maps.Equal(hashes, expected) {
		return Manifest{}, ErrPackage
	}
	if kind == Programs {
		var p ProgramProfile
		if jsonshape.Validate(profileBytes, &p) != nil || json.Unmarshal(profileBytes, &p) != nil || !p.Valid(p.Platform) {
			return Manifest{}, ErrPackage
		}
		for _, header := range headers {
			if ValidateExecutable(header, p.Platform) != nil {
				return Manifest{}, ErrPackage
			}
		}
		return Manifest{Version: 2, Kind: kind, Label: p.Label, Platform: p.Platform, Checksums: checksum}, nil
	}
	var profile Profile
	if jsonshape.Validate(profileBytes, &profile) != nil || json.Unmarshal(profileBytes, &profile) != nil {
		return Manifest{}, ErrPackage
	}
	if (kind == Image && (profile.Version != 2 || profile.Label == "")) || (kind == "" && profile.Version != 1) || (profile.Label != "" && !labelPattern.MatchString(profile.Label)) {
		return Manifest{}, ErrPackage
	}
	if profile.ArchiveSHA256 != hashes["computer-image.tar"] || computer.ValidateImageReference(profile.ImageReference) != nil || (profile.Platform != "linux/amd64" && profile.Platform != "linux/arm64") || (kind == "" && ValidateExecutable(headers["tomnode"], profile.Platform) != nil) {
		return Manifest{}, ErrPackage
	}
	runtimeDigest, err := RuntimeDigest(profile.Docker)
	if err != nil || jsonshape.Validate(profile.Docker, &dockerbootstrap.Manifest{}) != nil {
		return Manifest{}, ErrPackage
	}
	manifest := Manifest{Version: profile.Version, Kind: kind, Label: profile.Label, Platform: profile.Platform, Checksums: checksum}
	if kind == Image {
		manifest.RuntimeSHA256 = runtimeDigest
		manifest.ImageReference = profile.ImageReference
		manifest.ArchiveSHA256 = profile.ArchiveSHA256
	}
	return manifest, nil
}
