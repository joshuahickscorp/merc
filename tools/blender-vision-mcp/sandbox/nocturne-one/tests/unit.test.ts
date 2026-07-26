import assert from "node:assert/strict";
import test from "node:test";
import {
  configurationPayload,
  defaultConfiguration,
  isValidEmail,
  requestHash,
  validateConfiguration
} from "../src/shared/config.js";

test("configuration validation accepts the governed boundary values", () => {
  const result = validateConfiguration({
    variant: "ember",
    light_intensity: 100,
    orientation: -45,
    accessory: "braided-cable"
  });
  assert.equal(result.valid, true);
  assert.deepEqual(result.value, {
    variant: "ember",
    light_intensity: 100,
    orientation: -45,
    accessory: "braided-cable"
  });
});

test("configuration validation rejects out-of-contract state", () => {
  const result = validateConfiguration({
    variant: "clear",
    light_intensity: 101,
    orientation: 46,
    accessory: "battery"
  });
  assert.equal(result.valid, false);
  assert.deepEqual(Object.keys(result.errors).sort(), [
    "accessory",
    "light_intensity",
    "orientation",
    "variant"
  ]);
});

test("request hashing is deterministic and payload-sensitive", () => {
  const first = requestHash(defaultConfiguration, "one@example.invalid");
  const replay = requestHash(defaultConfiguration, "one@example.invalid");
  const changed = requestHash(
    { ...defaultConfiguration, orientation: 1 },
    "one@example.invalid"
  );
  assert.equal(first, replay);
  assert.notEqual(first, changed);
  assert.equal(configurationPayload(defaultConfiguration).startsWith("{"), true);
});

test("email validation handles valid and invalid values", () => {
  assert.equal(isValidEmail("reserved@example.invalid"), true);
  assert.equal(isValidEmail("invalid"), false);
  assert.equal(isValidEmail(null), false);
});
