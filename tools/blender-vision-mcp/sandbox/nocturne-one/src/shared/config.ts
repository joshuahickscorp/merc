import { createHash } from "node:crypto";

export const variants = ["obsidian", "lunar", "ember"] as const;
export const accessories = ["none", "braided-cable"] as const;

export type Variant = (typeof variants)[number];
export type Accessory = (typeof accessories)[number];

export interface Configuration {
  variant: Variant;
  light_intensity: number;
  orientation: number;
  accessory: Accessory;
}

export const defaultConfiguration: Configuration = {
  variant: "obsidian",
  light_intensity: 64,
  orientation: 0,
  accessory: "none"
};

export interface ValidationResult {
  valid: boolean;
  value?: Configuration;
  errors: Record<string, string>;
}

export function validateConfiguration(input: unknown): ValidationResult {
  const errors: Record<string, string> = {};
  if (!input || typeof input !== "object") {
    return { valid: false, errors: { configuration: "Configuration is required." } };
  }
  const item = input as Record<string, unknown>;
  const variant = item.variant;
  const accessory = item.accessory;
  const light = Number(item.light_intensity);
  const orientation = Number(item.orientation);
  if (!variants.includes(variant as Variant)) {
    errors.variant = "Choose obsidian, lunar, or ember.";
  }
  if (!Number.isInteger(light) || light < 0 || light > 100) {
    errors.light_intensity = "Light intensity must be an integer from 0 to 100.";
  }
  if (!Number.isInteger(orientation) || orientation < -45 || orientation > 45) {
    errors.orientation = "Orientation must be an integer from -45 to 45.";
  }
  if (!accessories.includes(accessory as Accessory)) {
    errors.accessory = "Choose no accessory or the braided cable.";
  }
  if (Object.keys(errors).length > 0) {
    return { valid: false, errors };
  }
  return {
    valid: true,
    value: {
      variant: variant as Variant,
      light_intensity: light,
      orientation,
      accessory: accessory as Accessory
    },
    errors
  };
}

export function isValidEmail(value: unknown): value is string {
  return (
    typeof value === "string" &&
    value.length <= 254 &&
    /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value)
  );
}

export function configurationPayload(configuration: Configuration): string {
  return JSON.stringify({
    variant: configuration.variant,
    light_intensity: configuration.light_intensity,
    orientation: configuration.orientation,
    accessory: configuration.accessory
  });
}

export function requestHash(
  configuration: Configuration,
  email: string
): string {
  return createHash("sha256")
    .update(`${email.toLowerCase()}\n${configurationPayload(configuration)}`)
    .digest("hex");
}
