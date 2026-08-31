// Client-side furthest-step tracking. Step position is per-operator
// convenience by contract (control-api.md); completion is instance state
// (POST /v1/setup/complete → setup_completed_at) and must never be inferred
// from localStorage — see api/setup.ts / setup/useSetupStatus.ts.

const STEP_KEY = "quasar.setup.step";

/** Step 1 (claim) has no stored state — it's only reachable pre-admin. */
export type WizardStep = 2 | 3 | 4 | 5;

export function loadWizardStep(): WizardStep {
  const raw = localStorage.getItem(STEP_KEY);
  if (raw === "5") return 5;
  if (raw === "4") return 4;
  if (raw === "3") return 3;
  return 2;
}

export function saveWizardStep(step: WizardStep): void {
  localStorage.setItem(STEP_KEY, String(step));
}

/** Tidies the local marker only; completion is reported to the server
 *  separately (api/setup.ts `completeSetup`). */
export function clearWizardStep(): void {
  localStorage.removeItem(STEP_KEY);
}
