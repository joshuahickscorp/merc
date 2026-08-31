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
	if tel.ch != nil {
		t.Fatal("new telemetry allocated its queue before its first record")
	}
	if tel.queueCap() != admissionTelemetryQueueCap {
		t.Fatalf("queueCap=%d want %d before lazy initialization", tel.queueCap(), admissionTelemetryQueueCap)
	}
	tel.Close(time.Second)
	if !tel.started {
		t.Fatal("Close must start and drain the telemetry workers")
	}
	if tel.ch == nil || cap(tel.ch) != admissionTelemetryQueueCap {
		t.Fatalf("Close initialized queue cap=%d want %d", cap(tel.ch), admissionTelemetryQueueCap)
	}
}
