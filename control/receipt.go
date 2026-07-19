package main

import "github.com/google/uuid"

type ClearingReceipt struct {
	JobID        uuid.UUID     `json:"job_id"`
	Status       string        `json:"status"`
	Invoice      *InvoiceView  `json:"invoice"`
	Verification Verification  `json:"verification"`
	Classes      []string      `json:"verification_classes"`
	Tasks        []TaskReceipt `json:"tasks"`
}

type TaskReceipt struct {
	ChunkIndex       int    `json:"chunk_index"`
	Status           string `json:"status"`
	IsHoneypot       bool   `json:"is_honeypot"`
	RuntimeCellID    string `json:"runtime_cell_id,omitempty"`
	RuntimeID        string `json:"runtime_id,omitempty"`
	RuntimeMatrixSHA string `json:"runtime_matrix_sha256,omitempty"`
	ModelKind        string `json:"model_kind,omitempty"`
	WorkerClass      string `json:"worker_class"`      // engine|build_hash (classKey); "" if unknown
	VerificationKind string `json:"verification_kind"` // latest comparison event kind; "" if none
	Verdict          string `json:"verdict"`           // durable current task verdict; "" until verified
}

func taskReceiptRow(chunkIndex int, status string, isHoneypot bool, engine, build, kind, verdict string) TaskReceipt {
	return taskReceiptRowWithRuntime(chunkIndex, status, isHoneypot, engine, build, kind, verdict, "", "", "", "")
}

func taskReceiptRowWithRuntime(chunkIndex int, status string, isHoneypot bool, engine, build, kind, verdict, cellID, runtimeID, matrixSHA, modelKind string) TaskReceipt {
	return TaskReceipt{
		ChunkIndex:       chunkIndex,
		Status:           status,
		IsHoneypot:       isHoneypot,
		RuntimeCellID:    cellID,
		RuntimeID:        runtimeID,
		RuntimeMatrixSHA: matrixSHA,
		ModelKind:        modelKind,
		WorkerClass:      classKey(engine, build),
		VerificationKind: kind,
		Verdict:          verdict,
	}
}

func assembleClearingReceipt(jobID uuid.UUID, status string, inv *InvoiceView, verif Verification, classes []string, tasks []TaskReceipt) ClearingReceipt {
	return ClearingReceipt{
		JobID:        jobID,
		Status:       status,
		Invoice:      inv,
		Verification: verif,
		Classes:      classes,
		Tasks:        tasks,
	}
}
