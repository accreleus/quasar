import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
import { AuthProvider } from "./auth/AuthProvider";
import { SetupStatusProvider } from "./setup/useSetupStatus";
import { ThemeProvider } from "./settings/ThemeContext";
import { ToastProvider } from "./components/Toast";
import { RouteErrorBoundary } from "./components/RouteBoundary";
// Token contract, base, then the shared primitives (v3): tokens define the
// custom properties every later sheet reads, base owns the reset, fonts,
// background field and focus ring, primitives owns every shared component
// class, and shell owns the frame around a page (.app/.topbar/.rail/.main).
// The remaining sheets keep their previous order.
import "./styles/tokens.css";
import "./styles/base.css";
import "./styles/primitives.css";
import "./styles/shell.css";
import "./styles.css";
import "./styles/components.css";
import "./components/layout.css";
// styles/admin.css is deliberately NOT imported here (#386): it is 48 KB of the
// 165 KB base CSS bundle and admin-only, while the admin JS has been lazily
// split since #139. It is imported by pages/admin/AdminLayout.tsx instead, so
// Vite emits it as a separate stylesheet that loads with the admin chunk.
// The pre-auth surface (login / register / setup) is imported last on purpose:
// every rule in it is scoped under `.auth-scene`, so it cannot leak, and where
// a selector ties on specificity with a legacy sheet the later artefact should
// win. See the header comment in styles/login.css.
import "./styles/login.css";

const root = document.getElementById("root");
if (!root) throw new Error("#root element not found");

createRoot(root).render(
  <StrictMode>
    <BrowserRouter>
      {/* #521: the provider stack (auth/setup/theme/toast) sat ABOVE the only
          error boundary (RouteErrorBoundary lives inside App, wrapping
          <Routes>). A throw during any provider's render or initial effect —
          AuthProvider init being the plausible one — was uncaught and produced
          a silent blank page. This boundary now covers the whole stack. */}
      <RouteErrorBoundary>
        <ThemeProvider>
          <AuthProvider>
            <SetupStatusProvider>
              <ToastProvider>
                <App />
              </ToastProvider>
            </SetupStatusProvider>
          </AuthProvider>
        </ThemeProvider>
      </RouteErrorBoundary>
    </BrowserRouter>
  </StrictMode>,
);
