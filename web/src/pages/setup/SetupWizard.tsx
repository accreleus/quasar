// /setup — the first-run wizard shell (steps 1-5; StepFinishing is the finish
// point before completeSetup()). Renders outside the authenticated shell
// (registered alongside /login), so it must work with no session: it uses the
// same pre-auth card as /login (AuthCard), widened to 40rem because the steps
// carry host tables and library forms, not two fields. Step content and step
// routing are untouched — only the chrome around them is the card's.

import { useState } from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { useAuth } from "../../auth/context";
import { useSetupStatus } from "../../setup/useSetupStatus";
import { completeSetup } from "../../api/setup";
import { clearWizardStep, loadWizardStep, saveWizardStep, type WizardStep } from "../../setup/progress";
import { reportBestEffortFailure } from "../../lib/reportBestEffortFailure";
import { AuthCard } from "../auth/AuthCard";
import { StepIndicator } from "./StepIndicator";
import { StepClaim } from "./StepClaim";
import { StepBasics } from "./StepBasics";
import { StepHosts } from "./StepHosts";
import { StepLibraries } from "./StepLibraries";
import { StepFinishing } from "./StepFinishing";

const CARD_WIDTH = "min(40rem, 100%)";

function FullScreenMessage({ children }: { children: React.ReactNode }) {
  return (
    <AuthCard width={CARD_WIDTH}>
      <p className="auth-status">{children}</p>
    </AuthCard>
  );
}

export function SetupWizard() {
  const { status: authStatus, isAdmin, token } = useAuth();
  const { status: setupStatus, loading: setupLoading, setStatus: setSetupStatus } = useSetupStatus();
  const navigate = useNavigate();
  const [step, setStep] = useState<WizardStep>(() => loadWizardStep());

  function goTo(next: WizardStep) {
    saveWizardStep(next);
    setStep(next);
  }

  async function finish() {
    clearWizardStep();
    if (token) {
      try {
        // Apply the response to the shared cache so the /admin resume banner
        // retires in this render pass, not after a second round-trip.
        const result = await completeSetup(token);
        setSetupStatus(result);
      } catch (err) {
        // Completion is idempotent and re-triable from the resume banner; a
        // transient failure must neither trap the operator nor read as done.
        reportBestEffortFailure("console-warn", "setup: POST /v1/setup/complete", err);
      }
    }
    navigate("/admin", { replace: true });
  }

  if (authStatus === "loading" || setupLoading) {
    return <FullScreenMessage>Checking instance status…</FullScreenMessage>;
  }

  // Fail-open on a status-fetch error: assume an admin exists rather than
  // strand the operator on an unusable claim form; a wrong assumption still
  // fails cleanly at the claim call (401/409).
  const adminExists = setupStatus?.admin_exists ?? true;

  let body: React.ReactNode;
  let visibleStep: 1 | 2 | 3 | 4 | 5;

  // authStatus first, never the stale adminExists read: the status fetch is
  // not re-run after a claim, so adminExists still reads false in the render
  // pass a claim just completed in — checking it first would re-render the
  // claim form forever.
  if (authStatus === "authenticated") {
    if (!isAdmin) {
      return <Navigate to="/app" replace />;
    } else if (setupStatus?.setup_completed) {
      // A retired wizard cannot be re-entered.
      return <Navigate to="/admin" replace />;
    } else if (step === 5) {
      visibleStep = 5;
      body = <StepFinishing onFinish={finish} />;
    } else if (step === 4) {
      visibleStep = 4;
      body = <StepLibraries onNext={() => goTo(5)} />;
    } else if (step === 3) {
      visibleStep = 3;
      body = <StepHosts onNext={() => goTo(4)} />;
    } else {
      visibleStep = 2;
      body = <StepBasics onNext={() => goTo(3)} />;
    }
  } else if (!adminExists) {
    visibleStep = 1;
    body = (
      <StepClaim
        onClaimed={() => {
          // RequireClaimedInstance reads the same status cache, which still
          // says admin_exists:false — without this update, leaving /setup
          // mid-wizard bounces the just-created admin straight back here.
          setSetupStatus({ admin_exists: true, setup_completed: setupStatus?.setup_completed ?? false });
          goTo(2);
        }}
      />
    );
  } else {
    // An admin already exists (env-bootstrap or a prior claim) but nobody is
    // signed in — the wizard's steps 2-5 need an admin session.
    return <Navigate to="/login" replace state={{ from: { pathname: "/setup" } }} />;
  }

  return (
    <AuthCard width={CARD_WIDTH}>
      {visibleStep !== 1 && (
        <div className="step-indicator">
          <StepIndicator current={visibleStep} />
        </div>
      )}
      {body}
    </AuthCard>
  );
}
