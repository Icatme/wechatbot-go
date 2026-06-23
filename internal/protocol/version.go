package protocol

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// buildClientVersion computes the iLink-App-ClientVersion uint32 from a semantic version.
// Format: 0x00MMNNPP where MM=major, NN=minor, PP=patch.
// Non-numeric pre-release suffixes are ignored for the version parts.
func buildClientVersion(version string) uint32 {
	version = strings.TrimPrefix(version, "v")
	parts := strings.Split(version, ".")
	if len(parts) < 3 {
		parts = append(parts, "0", "0", "0")
	}
	major := parseVersionPart(parts[0])
	minor := parseVersionPart(parts[1])
	patch := parseVersionPart(parts[2])
	return (uint32(major&0xff) << 16) | (uint32(minor&0xff) << 8) | uint32(patch&0xff)
}

func parseVersionPart(s string) int {
	// Strip pre-release/build metadata from the patch part.
	if idx := strings.IndexAny(s, "-+"); idx >= 0 {
		s = s[:idx]
	}
	n, _ := strconv.Atoi(s)
	return n
}

// moduleVersion returns the module version at build time, or a fallback.
// Development builds report "(devel)"; in that case fall back to ChannelVersion.
func moduleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/Icatme/wechatbot-go" {
				if isValidVersion(dep.Version) {
					return dep.Version
				}
				break
			}
		}
		if info.Main.Path == "github.com/Icatme/wechatbot-go" && isValidVersion(info.Main.Version) {
			return info.Main.Version
		}
	}
	return ChannelVersion
}

func isValidVersion(v string) bool {
	return v != "" && v != "(devel)"
}
