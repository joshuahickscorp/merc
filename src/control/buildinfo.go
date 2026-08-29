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
	// The catalogue price authority this process is actually serving from.
	//
	// Reported because a deployment that cannot answer "which board are you
	// pricing from" cannot be audited after the fact, and because the production
	// image shipped without a board at all: the binary started, or rather did
	// not, and nothing about the running service could have said which of the
	// four candidate paths it had resolved.
	PriceBoardSHA256 string `json:"price_board_sha256"`
	PriceBoardSource string `json:"price_board_source"`
	// The capability matrix agents must agree with, and the document it came
	// from. Two different questions; see capability_manifest.go.
	CapabilityMatrixSHA256  string `json:"capability_matrix_sha256"`
	AuthorityDocumentSHA256 string `json:"authority_document_sha256"`
}

func currentControlBuildInfo() ControlBuildInfo {
	info := ControlBuildInfo{
		Version: controlVersion, Commit: controlCommit, BuildDate: controlBuildDate,
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	info.CapabilityMatrixSHA256 = generatedRuntimeMatrixSHA256
	info.AuthorityDocumentSHA256 = generatedRuntimeAuthorityFileSHA256
	// Best effort: /version must answer even when the board is unloadable, since
	// that is exactly the deployment whose operator most needs an answer.
	if _, err := loadPriceBoard(); err == nil {
		info.PriceBoardSHA256, info.PriceBoardSource = priceBoardSHA256, priceBoardSource
	} else {
		info.PriceBoardSource = "unavailable"
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
