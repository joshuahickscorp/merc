package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const webhookSigningSecretPrefix = "cx_whsec_"

var (
	errWebhookSigningKeyUnavailable = errors.New("webhook signing key unavailable")
	errWebhookSigningSecretInvalid  = errors.New("webhook signing secret is missing or cannot be opened")
)

type WebhookRegistration struct {
	ID     uuid.UUID
	Secret string
}

func requireWebhookSigningKey() error {
	if len(tokenKey()) == 0 {
		return fmt.Errorf("%w: CX_TOKEN_KEY is required", errWebhookSigningKeyUnavailable)
	}
	return nil
}

func newWebhookSigningSecret() (plaintext, sealed string, err error) {
	if err := requireWebhookSigningKey(); err != nil {
		return "", "", err
	}
	plaintext = newSecret(webhookSigningSecretPrefix)
	if plaintext == "" {
		return "", "", errors.New("generating webhook signing secret: entropy source failed")
	}
	sealed = sealToken(plaintext)
	if !strings.HasPrefix(sealed, "enc:") {
		return "", "", errors.New("sealing webhook signing secret failed closed")
	}
	return plaintext, sealed, nil
}

func openWebhookSigningSecret(sealed string) (string, error) {
	if !strings.HasPrefix(sealed, "enc:") {
		return "", errWebhookSigningSecretInvalid
	}
	plaintext := openToken(sealed)
	if !strings.HasPrefix(plaintext, webhookSigningSecretPrefix) ||
		len(plaintext) <= len(webhookSigningSecretPrefix) {
		return "", errWebhookSigningSecretInvalid
	}
	return plaintext, nil
}

func signWebhook(secret string, body []byte) string {
	return signWebhookAt(secret, body, time.Now())
}

func signWebhookAt(secret string, body []byte, now time.Time) string {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
