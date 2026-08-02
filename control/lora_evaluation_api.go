package main

import (
	"errors"
	"io"
	"net/http"
)

// handleWorkerLoRAEvaluation is the production evaluator entry point. The
// authenticated worker supplies the evaluator identity; the store derives the
// supplier/account separation and binds the report to the probed LoRA IR.
func (s *Server) handleWorkerLoRAEvaluation(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxJSONRequestBodyBytes+1))
	if err != nil || len(raw) > maxJSONRequestBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "LoRA evaluation exceeds the bounded JSON limit")
		return
	}
	var submission ProjectLoRAEvaluationSubmission
	if err := decodeStrictJSONObject(raw, &submission); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid LoRA evaluation: "+err.Error())
		return
	}
	receipt, err := s.store.RecordProjectLoRAEvaluation(r.Context(), *auth, submission)
	if errors.Is(err, errProjectLoRAEvaluationReceiptNotFound) {
		writeErr(w, http.StatusNotFound, "LoRA compile receipt or step not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "LoRA evaluation refused: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, receipt)
}

func (s *Server) handleProjectLoRAEvaluationReceipt(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	receiptID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	receipt, err := s.store.GetProjectLoRAEvaluationReceipt(r.Context(), auth.BuyerID, receiptID)
	if errors.Is(err, errProjectLoRAEvaluationReceiptNotFound) {
		writeErr(w, http.StatusNotFound, "LoRA evaluation receipt not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LoRA evaluation receipt unavailable")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}
