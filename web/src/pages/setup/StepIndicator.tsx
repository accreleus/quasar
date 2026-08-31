// Three-dot step indicator for the wizard shell. Built from the existing
// Chip primitive (components/Chip.tsx) — no new visual vocabulary.

import { Chip } from "../../components/Chip";

export type WizardVisibleStep = 1 | 2 | 3 | 4 | 5;

const LABELS: Record<WizardVisibleStep, string> = {
  1: "Create admin",
  2: "Instance basics",
  3: "Host check",
  4: "Libraries",
  5: "Finishing touches",
};

export function StepIndicator({ current }: { current: WizardVisibleStep }) {
  const steps: WizardVisibleStep[] = [1, 2, 3, 4, 5];
  return (
    <div
      className="row"
      style={{ display: "flex", gap: "var(--s2)", flexWrap: "wrap", justifyContent: "center" }}
    >
      {steps.map((step) => (
        <Chip
          key={step}
          variant={step === current ? "accent" : step < current ? "success" : "neutral"}
        >
          {step}. {LABELS[step]}
        </Chip>
      ))}
    </div>
  );
}
