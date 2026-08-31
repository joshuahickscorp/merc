package main

import (
	"testing"
	"time"
)

func TestAdmissionTelemetryCloseBeforeFirstRecord(t *testing.T) {
	tel := newAdmissionTelemetry(&Store{})
	if tel.started {
		t.Fatal("new telemetry started workers before its first record")
	}
	tel.Close(time.Second)
	if !tel.started {
		t.Fatal("Close must start and drain the telemetry workers")
	}
}
