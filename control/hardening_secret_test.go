package main

import "testing"

func TestValidateHardeningSecretRefusesPublishedSampleSecretWithAnyStripeKey(t *testing.T) {
	const goodToken = "token-key-at-least-thirty-two-bytes-long"
	const goodSample = "verification-sample-secret-at-least-32b"

	// No stripe key: published default is a warning path, not a hard refuse.
	missing, err := validateHardeningSecretConfig("development", "", goodToken, "")
	if err != nil || !missing {
		t.Fatalf("dev without stripe should warn only: missing=%v err=%v", missing, err)
	}

	for _, key := range []string{"sk_test_example", "sk_live_example", "rk_test_example"} {
		_, err := validateHardeningSecretConfig("development", key, goodToken, "")
		if err == nil {
			t.Fatalf("empty sample secret with %s must refuse", key)
		}
		_, err = validateHardeningSecretConfig("development", key, goodToken, insecureDevelopmentSamplingSecret)
		if err == nil {
			t.Fatalf("published sample secret with %s must refuse", key)
		}
		missing, err = validateHardeningSecretConfig("development", key, goodToken, goodSample)
		if err != nil || missing {
			t.Fatalf("strong sample secret with %s should be fine: missing=%v err=%v", key, missing, err)
		}
	}
}

func TestVerificationSampleSecretFromEnv(t *testing.T) {
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET", "")
	secret, err := verificationSampleSecretFromEnv()
	if err != nil || secret != insecureDevelopmentSamplingSecret {
		t.Fatalf("dev default without stripe: secret=%q err=%v", secret, err)
	}

	t.Setenv("STRIPE_SECRET_KEY", "sk_test_money_still_buys_fraud_practice")
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET", "")
	if _, err := verificationSampleSecretFromEnv(); err == nil {
		t.Fatal("published default must be refused when any stripe key is set")
	}

	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET", "a-strong-verification-sample-secret-32+")
	secret, err = verificationSampleSecretFromEnv()
	if err != nil || secret == insecureDevelopmentSamplingSecret {
		t.Fatalf("configured secret rejected: %q %v", secret, err)
	}
}
