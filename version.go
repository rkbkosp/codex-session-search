package main

import (
	"fmt"
	"strings"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

const releaseRepository = "rkbkosp/codex-session-search"

type buildInfo struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	BuildDate  string `json:"build_date"`
	Repository string `json:"repository"`
}

func currentBuildInfo() buildInfo {
	return buildInfo{
		Version:    version,
		Commit:     commit,
		BuildDate:  buildDate,
		Repository: releaseRepository,
	}
}

func versionLine() string {
	info := currentBuildInfo()
	return fmt.Sprintf("codex-session-search %s (%s, built %s)", info.Version, info.Commit, info.BuildDate)
}

func normalizedVersion(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "refs/tags/")
	value = strings.TrimPrefix(value, "v")
	return value
}

func isDevVersion(value string) bool {
	normalized := normalizedVersion(value)
	return normalized == "" || normalized == "dev" || normalized == "unknown"
}
