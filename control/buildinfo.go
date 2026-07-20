package main

import (
	"runtime"
	"runtime/debug"
)

var (
	controlVersion   = "dev"
	controlCommit    = "unknown"
	controlBuildDate = "unknown"
)

type ControlBuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	Modified  bool   `json:"modified"`
}

func currentControlBuildInfo() ControlBuildInfo {
	info := ControlBuildInfo{
		Version: controlVersion, Commit: controlCommit, BuildDate: controlBuildDate,
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "unknown" && setting.Value != "" {
					info.Commit = setting.Value
				}
			case "vcs.modified":
				info.Modified = setting.Value == "true"
			}
		}
	}
	return info
}
